// Package cacheimport seeds Specula's cache from an OCI image layout produced
// somewhere else.
//
// It exists for the case where the upstreams a cluster needs are unreachable but
// SOME machine can reach them: pull the image there, hand the layout to Specula,
// and every later pull is a cache hit that touches no upstream at all. The same
// mechanism covers an air-gapped install, where there is no upstream by design.
//
// What "seeded" has to mean is precise, because a partial job looks like success
// and fails at pull time. A containerd pull of docker.io/library/redis:7-alpine
// asks Specula for, in order:
//
//  1. the manifest by TAG            → mutable entry  oci:<name>:<tag> → digest
//  2. the manifest by DIGEST         → immutable entry (oci, <name>, <digest>)
//  3. the config and layer BLOBS     → immutable entries, same shape
//
// So an import writes all three. Miss the mutable pointer and the pull still goes
// upstream for step 1; miss a layer and the pull fails at the last step having
// already spent the transfer.
//
// Only OCI layouts are accepted, and that is a correctness requirement rather
// than a convenience: `docker save` (legacy format) re-packs layers as
// uncompressed tars whose digests DIFFER from the registry's, so importing one
// would populate the cache under digests no client will ever ask for. The
// importer detects that layout and refuses it with the command that produces a
// usable one.
package cacheimport

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// ErrLegacyDockerSave reports an input in `docker save`'s legacy format, whose
// layer digests do not match the registry's.
var ErrLegacyDockerSave = errors.New(
	"cacheimport: this is a legacy `docker save` archive, whose layer digests differ " +
		"from the registry's — importing it would fill the cache under digests no client " +
		"asks for. Produce an OCI layout instead:\n" +
		"  crane pull --format=oci <image> layout.tar\n" +
		"  # or: skopeo copy docker://<image> oci-archive:layout.tar")

// Options configures one import.
type Options struct {
	// Source is a path to an OCI layout directory or an OCI archive (tar).
	Source string

	// Target is the reference the cache should answer for, in the form clients
	// use: "docker.io/library/redis:7-alpine", "registry.k8s.io/pause:3.10".
	Target string

	// TTLSeconds is the TTL for the tag→digest pointer. Zero means "always
	// revalidate", which for a seeded cache would send every pull upstream to
	// check a tag it cannot reach; SeedTagTTLSeconds is the default instead.
	TTLSeconds int64

	// DryRun parses and reports without writing anything.
	DryRun bool

	Logger *slog.Logger
}

// SeedTagTTLSeconds is the default TTL for a seeded tag pointer: long, because
// the point of seeding is that the upstream cannot be consulted. An operator
// re-runs the import to move a tag.
const SeedTagTTLSeconds int64 = 30 * 24 * 3600

// Result reports what an import wrote.
type Result struct {
	// Name is the repository name the cache now answers for ("library/redis").
	Name string
	// Tag is the tag pointer written, empty when the target was a digest.
	Tag string
	// ManifestDigest is the digest the tag resolves to.
	ManifestDigest string
	// Manifests and Blobs count the objects written (or that would be).
	Manifests int
	Blobs     int
	// Bytes is the total size of the objects written.
	Bytes int64
	// AlreadyPresent counts objects the CAS already had — a re-import, or content
	// shared with an image imported earlier. They cost no storage.
	AlreadyPresent int
}

// Run imports Options.Source into the cache so pulls of Options.Target hit it.
func Run(ctx context.Context, cm cache.CacheManager, metaStore meta.MetadataStore, opts Options) (*Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if cm == nil {
		return nil, errors.New("cacheimport: nil cache manager")
	}
	name, tag, digestRef, err := ParseTarget(opts.Target)
	if err != nil {
		return nil, err
	}

	src, err := openLayout(opts.Source)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	idx, err := src.index()
	if err != nil {
		return nil, err
	}
	root, err := pickManifest(idx, tag, digestRef)
	if err != nil {
		return nil, err
	}

	res := &Result{Name: name, Tag: tag, ManifestDigest: root.Digest}
	ttl := opts.TTLSeconds
	if ttl == 0 {
		ttl = SeedTagTTLSeconds
	}

	// Walk the manifest graph: an index points at per-platform manifests, each of
	// which points at a config and layers. Everything reachable must land, because
	// a client picks its own platform and will ask for whatever it chose.
	seen := map[string]bool{}
	queue := []descriptor{root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d.Digest] {
			continue
		}
		seen[d.Digest] = true

		body, err := src.blob(d.Digest)
		if err != nil {
			return nil, fmt.Errorf("cacheimport: read %s: %w", d.Digest, err)
		}

		isManifest := isManifestMediaType(d.MediaType)
		if isManifest {
			children, cerr := childDescriptors(body)
			if cerr != nil {
				return nil, fmt.Errorf("cacheimport: parse manifest %s: %w", d.Digest, cerr)
			}
			queue = append(queue, children...)
			res.Manifests++
		} else {
			res.Blobs++
		}
		res.Bytes += int64(len(body))

		if opts.DryRun {
			continue
		}
		already, werr := writeObject(ctx, cm, metaStore, name, d.Digest, body)
		if werr != nil {
			return nil, werr
		}
		if already {
			res.AlreadyPresent++
		}
		log.Debug("cacheimport: stored", "digest", d.Digest, "manifest", isManifest,
			"bytes", len(body), "already_present", already)
	}

	if opts.DryRun {
		return res, nil
	}

	// The tag pointer. Without it a pull by tag still asks upstream to resolve the
	// tag — which is exactly the request that cannot be served here.
	if tag != "" {
		if metaStore == nil {
			return res, errors.New("cacheimport: no metadata store, cannot write the tag pointer " +
				"(pulls by tag would still go upstream)")
		}
		me := artifact.MutableEntry{
			Key:        "oci:" + name + ":" + tag,
			Protocol:   "oci",
			Digest:     root.Digest,
			TTLSeconds: ttl,
			Upstream:   "cache-import",
			FetchedAt:  time.Now().UTC(),
		}
		if err := metaStore.PutMutable(ctx, me); err != nil {
			return res, fmt.Errorf("cacheimport: write tag pointer %s:%s: %w", name, tag, err)
		}
	}
	return res, nil
}

// writeObject stores one blob or manifest under name, keyed the way the read path
// looks it up. Reports whether the CAS already held these bytes.
func writeObject(ctx context.Context, cm cache.CacheManager, metaStore meta.MetadataStore,
	name, digest string, body []byte) (already bool, err error) {
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     name,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}
	if existing, lerr := cm.Lookup(ctx, ref); lerr == nil && existing != nil {
		return true, nil
	}

	// The bytes may already be in the CAS under another repository name: images
	// built on a common base share layers, so this is the ordinary case when
	// seeding a second image. Recording a row for this name is then the whole job
	// — writing the bytes again would spool a temp file and re-hash a layer the
	// store already holds, only for Put to discard it.
	if metaStore != nil {
		if src := existingByDigest(ctx, metaStore, digest); src != nil {
			adopted := *src
			adopted.Ref = ref
			adopted.Origin = artifact.OriginCached
			if perr := metaStore.Put(ctx, adopted); perr != nil {
				return false, fmt.Errorf("cacheimport: record %s under %s: %w", digest, name, perr)
			}
			return true, nil
		}
	}

	// Store consumes a file, as it does for an upstream fetch: same
	// verify-on-write path, so a corrupt layout is rejected here rather than
	// discovered by a client.
	tmp, err := os.CreateTemp("", "specula-import-*")
	if err != nil {
		return false, fmt.Errorf("cacheimport: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(body); err != nil {
		return false, fmt.Errorf("cacheimport: spool %s: %w", digest, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("cacheimport: close spool: %w", err)
	}

	art := &artifact.Artifact{
		Path:   tmpPath,
		Digest: digest,
		Size:   int64(len(body)),
		Meta:   artifact.UpstreamMeta{Upstream: "cache-import"},
	}
	if _, err := cm.Store(ctx, ref, art); err != nil {
		return false, fmt.Errorf("cacheimport: store %s: %w", digest, err)
	}
	return false, nil
}

// existingByDigest finds any cached entry already holding these exact bytes,
// regardless of which repository name recorded them. Pull-through content only:
// adopting a hosted org's private blob into a public cache name would disclose it.
func existingByDigest(ctx context.Context, metaStore meta.MetadataStore, digest string) *artifact.CacheEntry {
	page, err := metaStore.ListEntries(ctx, "oci", meta.EntryFilter{
		Digest: digest,
		Origin: artifact.OriginCached,
	}, meta.Page{Limit: 1})
	if err != nil || len(page.Entries) == 0 {
		return nil
	}
	e := page.Entries[0].CacheEntry
	return &e
}

// ParseTarget splits a client-shaped reference into the repository name Specula
// answers for plus the tag or digest.
//
// The registry host is dropped for Docker Hub only, mirroring how the read path
// names things: a node asking for docker.io/library/redis reaches /v2/library/redis,
// while registry.k8s.io/pause keeps its host in the path.
func ParseTarget(target string) (name, tag, digest string, err error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", "", errors.New("cacheimport: empty target reference")
	}
	if i := strings.Index(t, "@"); i >= 0 {
		name, digest = t[:i], t[i+1:]
		if !strings.Contains(digest, ":") {
			return "", "", "", fmt.Errorf("cacheimport: malformed digest %q", digest)
		}
	} else {
		// A colon after the last slash is a tag; a colon before it is a port.
		slash := strings.LastIndex(t, "/")
		colon := strings.LastIndex(t, ":")
		if colon > slash {
			name, tag = t[:colon], t[colon+1:]
		} else {
			name, tag = t, "latest"
		}
	}

	name = strings.TrimPrefix(name, "docker.io/")
	name = strings.TrimPrefix(name, "index.docker.io/")
	if name == "" {
		return "", "", "", fmt.Errorf("cacheimport: no repository in %q", target)
	}
	// A bare Hub name is library/<name>, the path a client actually requests.
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return name, tag, digest, nil
}

// ── OCI layout reading ───────────────────────────────────────────────────────

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type indexDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []descriptor `json:"manifests"`
}

type manifestDoc struct {
	MediaType string       `json:"mediaType,omitempty"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Subject   *descriptor  `json:"subject,omitempty"`
	Manifests []descriptor `json:"manifests,omitempty"` // when this is an index
}

// layout reads blobs by digest out of an OCI layout, whether it is a directory
// or a tar archive.
type layout struct {
	dir string
	// tarBlobs holds an archive's contents in memory only for the index and
	// manifests; layer bytes are read on demand from the file.
	tarPath string
	entries map[string]int64 // digest → offset is not portable across readers, so
	// archives are indexed by name instead and re-scanned per read.
}

func openLayout(src string) (*layout, error) {
	if src == "" {
		return nil, errors.New("cacheimport: empty source path")
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("cacheimport: source %q: %w", src, err)
	}
	if st.IsDir() {
		if _, err := os.Stat(filepath.Join(src, "index.json")); err != nil {
			if _, lerr := os.Stat(filepath.Join(src, "manifest.json")); lerr == nil {
				return nil, ErrLegacyDockerSave
			}
			return nil, fmt.Errorf("cacheimport: %q has no index.json — not an OCI layout", src)
		}
		return &layout{dir: src}, nil
	}
	l := &layout{tarPath: src}
	if err := l.checkArchive(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *layout) Close() error { return nil }

// checkArchive verifies the tar looks like an OCI archive and not a docker save.
func (l *layout) checkArchive() error {
	f, err := os.Open(l.tarPath)
	if err != nil {
		return fmt.Errorf("cacheimport: open %q: %w", l.tarPath, err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var hasIndex, hasDockerManifest bool
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("cacheimport: read %q: %w", l.tarPath, err)
		}
		switch path.Clean(hdr.Name) {
		case "index.json":
			hasIndex = true
		case "manifest.json":
			hasDockerManifest = true
		}
	}
	if hasIndex {
		return nil
	}
	if hasDockerManifest {
		return ErrLegacyDockerSave
	}
	return fmt.Errorf("cacheimport: %q contains no index.json — not an OCI archive", l.tarPath)
}

func (l *layout) index() (*indexDoc, error) {
	body, err := l.read("index.json")
	if err != nil {
		return nil, err
	}
	var idx indexDoc
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("cacheimport: parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return nil, errors.New("cacheimport: index.json lists no manifests")
	}
	return &idx, nil
}

func (l *layout) blob(digest string) ([]byte, error) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok {
		return nil, fmt.Errorf("cacheimport: malformed digest %q", digest)
	}
	return l.read(path.Join("blobs", algo, hex))
}

func (l *layout) read(name string) ([]byte, error) {
	if l.dir != "" {
		return os.ReadFile(filepath.Join(l.dir, filepath.FromSlash(name)))
	}
	f, err := os.Open(l.tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cacheimport: %q not found in %s", name, l.tarPath)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(hdr.Name) == path.Clean(name) {
			return io.ReadAll(tr)
		}
	}
}

// pickManifest chooses which index entry to import.
//
// A layout produced for one reference normally holds exactly one; when it holds
// several, the tag annotation decides, because importing the wrong one would
// publish a tag pointing at another image.
func pickManifest(idx *indexDoc, tag, digest string) (descriptor, error) {
	if digest != "" {
		for _, m := range idx.Manifests {
			if m.Digest == digest {
				return m, nil
			}
		}
		return descriptor{}, fmt.Errorf("cacheimport: %s is not in this layout", digest)
	}
	if len(idx.Manifests) == 1 {
		return idx.Manifests[0], nil
	}
	var candidates []string
	for _, m := range idx.Manifests {
		ref := m.Annotations["org.opencontainers.image.ref.name"]
		candidates = append(candidates, ref)
		if ref == "" {
			continue
		}
		if ref == tag || strings.HasSuffix(ref, ":"+tag) {
			return m, nil
		}
	}
	return descriptor{}, fmt.Errorf(
		"cacheimport: layout holds %d manifests and none is annotated for tag %q (found %v); "+
			"import by digest with --as <name>@<digest>",
		len(idx.Manifests), tag, candidates)
}

func isManifestMediaType(mt string) bool {
	switch mt {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json":
		return true
	}
	return false
}

// childDescriptors returns everything a manifest or index points at.
func childDescriptors(body []byte) ([]descriptor, error) {
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var out []descriptor
	out = append(out, doc.Manifests...)
	if doc.Config.Digest != "" {
		out = append(out, doc.Config)
	}
	out = append(out, doc.Layers...)
	if doc.Subject != nil && doc.Subject.Digest != "" {
		out = append(out, *doc.Subject)
	}
	return out, nil
}
