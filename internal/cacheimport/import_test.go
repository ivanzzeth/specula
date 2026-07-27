package cacheimport

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/verify"
)

// The test that decides whether this feature works: after an import, an OCI
// handler with NO UPSTREAM CONFIGURED must serve a complete pull — manifest by
// tag, manifest by digest, config and every layer. Anything the import forgot
// shows up here as the 404 a real containerd would hit, instead of passing
// because some upstream quietly filled the gap.

type fixture struct {
	cm    cache.CacheManager
	meta  meta.MetadataStore
	blobs *local.LocalDiskDriver
	srv   *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })

	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))
	cm := cache.New(blobs, metaStore, verify.NewChain())

	q := filepath.Join(dir, "quarantine")
	if err := os.MkdirAll(q, 0o755); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	h := oci.NewHandler(cm, oci.WithMeta(metaStore), oci.WithQuarantineDir(q))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &fixture{cm: cm, meta: metaStore, blobs: blobs, srv: srv}
}

// ── building a real OCI layout, by hand ──────────────────────────────────────

type builtImage struct {
	dir            string
	tarPath        string
	indexDigest    string
	manifestDigest string
	configDigest   string
	layerDigests   []string
	layerBodies    map[string][]byte
}

func dgst(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// buildLayout writes an OCI layout with an index → manifest → config + layers,
// which is what `crane pull --format=oci` produces. tagRef, when non-empty, is
// recorded as the index entry's ref.name annotation.
func buildLayout(t *testing.T, tagRef string, layers ...[]byte) *builtImage {
	t.Helper()
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	img := &builtImage{dir: dir, layerBodies: map[string][]byte{}}

	write := func(body []byte) string {
		d := dgst(body)
		if err := os.WriteFile(filepath.Join(blobDir, strings.TrimPrefix(d, "sha256:")), body, 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
		return d
	}

	config := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	img.configDigest = write(config)
	img.layerBodies[img.configDigest] = config

	var layerDescs []descriptor
	for _, l := range layers {
		d := write(l)
		img.layerDigests = append(img.layerDigests, d)
		img.layerBodies[d] = l
		layerDescs = append(layerDescs, descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    d,
			Size:      int64(len(l)),
		})
	}

	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    img.configDigest,
			Size:      int64(len(config)),
		},
		"layers": layerDescs,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	img.manifestDigest = write(manifest)
	img.layerBodies[img.manifestDigest] = manifest

	entry := map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest":    img.manifestDigest,
		"size":      len(manifest),
	}
	if tagRef != "" {
		entry["annotations"] = map[string]string{"org.opencontainers.image.ref.name": tagRef}
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{entry},
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}
	img.indexDigest = dgst(index)
	return img
}

// tarUp packs a layout directory into an OCI archive, the `crane pull` tar form.
func tarUp(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "layout.tar")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if werr := tw.WriteHeader(&tar.Header{
			Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(body)),
		}); werr != nil {
			return werr
		}
		_, werr := tw.Write(body)
		return werr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// ── the acceptance test ──────────────────────────────────────────────────────

func TestImportedImageServesACompletePullWithNoUpstream(t *testing.T) {
	f := newFixture(t)
	layerA := []byte("layer one bytes, pretend gzip")
	layerB := []byte("layer two bytes, also pretend gzip")
	img := buildLayout(t, "7-alpine", layerA, layerB)

	res, err := Run(context.Background(), f.cm, f.meta, Options{
		Source: img.dir,
		Target: "docker.io/library/redis:7-alpine",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Name != "library/redis" {
		t.Errorf("Name = %q, want library/redis", res.Name)
	}
	if res.Tag != "7-alpine" {
		t.Errorf("Tag = %q", res.Tag)
	}
	if res.Blobs != 3 { // config + two layers
		t.Errorf("Blobs = %d, want 3", res.Blobs)
	}
	if res.Manifests != 1 {
		t.Errorf("Manifests = %d, want 1", res.Manifests)
	}

	// 1. manifest by tag — the request a pull starts with.
	resp := get(t, f.srv.URL+"/v2/library/redis/manifests/7-alpine",
		"application/vnd.oci.image.manifest.v1+json")
	if resp.code != http.StatusOK {
		t.Fatalf("GET manifest by tag = %d, want 200 — without the tag pointer every pull still goes upstream", resp.code)
	}
	if resp.digestHeader != img.manifestDigest {
		t.Errorf("tag resolved to %q, want %q", resp.digestHeader, img.manifestDigest)
	}

	// 2. manifest by digest.
	resp = get(t, f.srv.URL+"/v2/library/redis/manifests/"+img.manifestDigest,
		"application/vnd.oci.image.manifest.v1+json")
	if resp.code != http.StatusOK {
		t.Fatalf("GET manifest by digest = %d, want 200", resp.code)
	}

	// 3. config and every layer.
	for _, d := range append([]string{img.configDigest}, img.layerDigests...) {
		r := get(t, f.srv.URL+"/v2/library/redis/blobs/"+d, "")
		if r.code != http.StatusOK {
			t.Fatalf("GET blob %s = %d, want 200", d, r.code)
		}
		if want := img.layerBodies[d]; string(r.body) != string(want) {
			t.Errorf("blob %s served %d bytes, want %d", d, len(r.body), len(want))
		}
	}
}

type response struct {
	code         int
	body         []byte
	digestHeader string
}

func get(t *testing.T, url, accept string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, body: body, digestHeader: resp.Header.Get("Docker-Content-Digest")}
}

// The archive form is what `crane pull --format=oci image.tar` produces, and is
// how a layout actually travels between machines.
func TestImportFromAnOCIArchive(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "3.10", []byte("pause layer"))
	tarPath := tarUp(t, img.dir)

	res, err := Run(context.Background(), f.cm, f.meta, Options{
		Source: tarPath,
		Target: "registry.k8s.io/pause:3.10",
	})
	if err != nil {
		t.Fatalf("import from archive: %v", err)
	}
	// A non-Hub registry keeps its host in the repository name, matching the path
	// a node requests.
	if res.Name != "registry.k8s.io/pause" {
		t.Fatalf("Name = %q, want registry.k8s.io/pause", res.Name)
	}
	if r := get(t, f.srv.URL+"/v2/registry.k8s.io/pause/manifests/3.10", ""); r.code != http.StatusOK {
		t.Fatalf("GET manifest by tag = %d, want 200", r.code)
	}
}

// The trap this refuses to walk into: `docker save` re-packs layers, so its
// digests are not the registry's. Importing one would fill the cache under
// digests no client ever asks for — a silent no-op that looks like success.
func TestLegacyDockerSaveArchiveIsRefused(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "docker-save.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tw := tar.NewWriter(f)
	body := []byte(`[{"Config":"c.json","RepoTags":["redis:7-alpine"],"Layers":["l/layer.tar"]}]`)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("hdr: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tw.Close()
	_ = f.Close()

	fx := newFixture(t)
	_, err = Run(context.Background(), fx.cm, fx.meta, Options{
		Source: tarPath, Target: "docker.io/library/redis:7-alpine",
	})
	if !errors.Is(err, ErrLegacyDockerSave) {
		t.Fatalf("error = %v, want ErrLegacyDockerSave", err)
	}
	if !strings.Contains(err.Error(), "crane pull --format=oci") {
		t.Errorf("the error must name the command that produces a usable layout: %v", err)
	}
}

func TestLegacyDockerSaveDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("[]"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fx := newFixture(t)
	_, err := Run(context.Background(), fx.cm, fx.meta, Options{Source: dir, Target: "redis:7"})
	if !errors.Is(err, ErrLegacyDockerSave) {
		t.Fatalf("error = %v, want ErrLegacyDockerSave", err)
	}
}

// Re-importing must be cheap and must not duplicate anything: the CAS already
// holds the bytes, so the second run reports them as already present.
func TestReimportStoresNothingTwice(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("a layer"))
	opts := Options{Source: img.dir, Target: "docker.io/library/app:v1"}

	first, err := Run(context.Background(), f.cm, f.meta, opts)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	usedAfterFirst, err := f.blobs.UsageBytes(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}

	second, err := Run(context.Background(), f.cm, f.meta, opts)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.AlreadyPresent != first.Manifests+first.Blobs {
		t.Errorf("second run reported %d already present, want %d",
			second.AlreadyPresent, first.Manifests+first.Blobs)
	}
	usedAfterSecond, err := f.blobs.UsageBytes(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usedAfterSecond != usedAfterFirst {
		t.Errorf("CAS grew from %d to %d bytes on re-import", usedAfterFirst, usedAfterSecond)
	}
}

// Two images sharing a layer must share the object, since that is the whole
// point of a content-addressed store.
func TestTwoImportsSharingALayerStoreItOnce(t *testing.T) {
	f := newFixture(t)
	shared := []byte("a base layer both images use")
	a := buildLayout(t, "a", shared, []byte("only in A"))
	b := buildLayout(t, "b", shared, []byte("only in B"))

	if _, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: a.dir, Target: "docker.io/library/a:a"}); err != nil {
		t.Fatalf("import a: %v", err)
	}
	res, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: b.dir, Target: "docker.io/library/b:b"})
	if err != nil {
		t.Fatalf("import b: %v", err)
	}
	if res.AlreadyPresent == 0 {
		t.Error("the shared layer was stored a second time")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("layer"))
	res, err := Run(context.Background(), f.cm, f.meta, Options{
		Source: img.dir, Target: "docker.io/library/app:v1", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Blobs == 0 {
		t.Error("dry run reported nothing to do")
	}
	used, err := f.blobs.UsageBytes(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if used != 0 {
		t.Errorf("dry run wrote %d bytes", used)
	}
	if r := get(t, f.srv.URL+"/v2/library/app/manifests/v1", ""); r.code == http.StatusOK {
		t.Error("dry run made the tag resolvable")
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in, name, tag, digest string
	}{
		{"docker.io/library/redis:7-alpine", "library/redis", "7-alpine", ""},
		{"index.docker.io/library/redis:7", "library/redis", "7", ""},
		{"redis:7-alpine", "library/redis", "7-alpine", ""},
		{"redis", "library/redis", "latest", ""},
		{"myorg/app:v1", "myorg/app", "v1", ""},
		{"registry.k8s.io/pause:3.10", "registry.k8s.io/pause", "3.10", ""},
		{"ghcr.io/owner/repo:tag", "ghcr.io/owner/repo", "tag", ""},
		// A port must not be read as a tag.
		{"registry.internal:5000/team/app:v2", "registry.internal:5000/team/app", "v2", ""},
		{"registry.internal:5000/team/app", "registry.internal:5000/team/app", "latest", ""},
		{"docker.io/library/redis@sha256:" + strings.Repeat("a", 64),
			"library/redis", "", "sha256:" + strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		name, tag, digest, err := ParseTarget(c.in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", c.in, err)
			continue
		}
		if name != c.name || tag != c.tag || digest != c.digest {
			t.Errorf("ParseTarget(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, name, tag, digest, c.name, c.tag, c.digest)
		}
	}
	if _, _, _, err := ParseTarget(""); err == nil {
		t.Error("empty target must be an error")
	}
}

// A layout holding several tagged manifests must not guess: publishing a tag that
// points at the wrong image is worse than refusing.
func TestAmbiguousLayoutIsRefusedRatherThanGuessed(t *testing.T) {
	img := buildLayout(t, "", []byte("layer"))
	// Rewrite the index with two unannotated entries.
	idxPath := filepath.Join(img.dir, "index.json")
	body, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx map[string]any
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatalf("parse: %v", err)
	}
	ms := idx["manifests"].([]any)
	idx["manifests"] = append(ms, ms[0])
	out, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(idxPath, out, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := newFixture(t)
	_, err = Run(context.Background(), f.cm, f.meta,
		Options{Source: img.dir, Target: "docker.io/library/x:v1"})
	if err == nil {
		t.Fatal("an ambiguous layout was imported without complaint")
	}
	if !strings.Contains(err.Error(), "import by digest") {
		t.Errorf("the error must say how to disambiguate: %v", err)
	}
}

// Importing by digest pins exactly one manifest out of a multi-entry layout.
func TestImportByDigestPicksThatManifest(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("layer"))
	res, err := Run(context.Background(), f.cm, f.meta, Options{
		Source: img.dir,
		Target: "docker.io/library/app@" + img.manifestDigest,
	})
	if err != nil {
		t.Fatalf("import by digest: %v", err)
	}
	if res.Tag != "" {
		t.Errorf("Tag = %q, want empty for a digest import", res.Tag)
	}
	if r := get(t, f.srv.URL+"/v2/library/app/manifests/"+img.manifestDigest, ""); r.code != http.StatusOK {
		t.Fatalf("GET manifest by digest = %d, want 200", r.code)
	}
}

// A layout whose blob bytes do not match their filename digest must be rejected
// by the verify-on-write path rather than poisoning the cache.
func TestCorruptLayoutIsRejected(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("good layer"))
	victim := filepath.Join(img.dir, "blobs", "sha256",
		strings.TrimPrefix(img.layerDigests[0], "sha256:"))
	if err := os.WriteFile(victim, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: img.dir, Target: "docker.io/library/app:v1"})
	if err == nil {
		t.Fatal("a layout with tampered bytes was imported")
	}
	if !strings.Contains(err.Error(), img.layerDigests[0]) {
		t.Errorf("the error should name the offending digest: %v", err)
	}
}

func TestMissingSourceIsAClearError(t *testing.T) {
	f := newFixture(t)
	_, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: filepath.Join(t.TempDir(), "nope"), Target: "redis:7"})
	if err == nil {
		t.Fatal("missing source accepted")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the path: %v", err)
	}
}

func TestSeededTagTTLIsLongEnoughToBeUseful(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("layer"))
	if _, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: img.dir, Target: "docker.io/library/app:v1"}); err != nil {
		t.Fatalf("import: %v", err)
	}
	me, err := f.meta.GetMutable(context.Background(), "oci:library/app:v1")
	if err != nil || me == nil {
		t.Fatalf("tag pointer missing: %v", err)
	}
	// A zero TTL means "revalidate every request", which for seeded content sends
	// every pull to an upstream that by assumption cannot answer.
	if me.TTLSeconds <= 0 {
		t.Errorf("seeded tag TTL = %d; every pull would revalidate upstream", me.TTLSeconds)
	}
	if me.Digest != img.manifestDigest {
		t.Errorf("tag points at %q, want %q", me.Digest, img.manifestDigest)
	}
}

func TestResultCountsAreReported(t *testing.T) {
	f := newFixture(t)
	img := buildLayout(t, "v1", []byte("l1"), []byte("l2"), []byte("l3"))
	res, err := Run(context.Background(), f.cm, f.meta,
		Options{Source: img.dir, Target: "docker.io/library/app:v1"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Blobs != 4 { // config + 3 layers
		t.Errorf("Blobs = %d, want 4", res.Blobs)
	}
	if res.Bytes <= 0 {
		t.Error("Bytes not reported")
	}
	if res.ManifestDigest != img.manifestDigest {
		t.Errorf("ManifestDigest = %q, want %q", res.ManifestDigest, img.manifestDigest)
	}
	_ = fmt.Sprint(res)
}
