package verify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// HTTPMirrorDigestFetcher is the production MirrorDigestFetcher: it resolves the
// sha256 digest a mirror advertises for an artifact using ONLY a metadata
// request (an HTTP HEAD, or a small index/metadata GET) — never the full blob
// (DESIGN-REVIEW §1.2: "只比 digest/manifest，不下载全 blob").
//
// The digest returned is always in the sha256 space so it is directly
// comparable to the artifact's CAS digest (art.Digest). This is achievable
// metadata-only for exactly the ecosystems whose metadata publishes a sha256:
//
//   - pypi: the PEP 503 simple-index page lists each file with a
//     "#sha256=<hex>" URL fragment — a poisoned mirror advertising a different
//     digest for the same filename is detected here without a download.
//   - oci:  a HEAD on /v2/<name>/manifests|blobs/<ref> returns the content
//     digest in the "Docker-Content-Digest" response header.
//
// For ecosystems whose metadata does NOT expose a sha256 (npm publishes a
// sha512 "integrity" + sha1 "shasum"; a generic tarball advertises no digest at
// all), FetchDigest returns an error: that mirror simply casts no vote, and the
// wiring layer must not enable metadata-only consensus for such a protocol
// rather than fail every real fetch closed.
type HTTPMirrorDigestFetcher struct {
	client *http.Client
}

// NewHTTPMirrorDigestFetcher builds a fetcher with a bounded HTTP client. A zero
// timeout uses a sane default; the per-call context still governs cancellation.
func NewHTTPMirrorDigestFetcher(timeout time.Duration) *HTTPMirrorDigestFetcher {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &HTTPMirrorDigestFetcher{client: &http.Client{Timeout: timeout}}
}

// Compile-time assertion that HTTPMirrorDigestFetcher satisfies the interface.
var _ MirrorDigestFetcher = (*HTTPMirrorDigestFetcher)(nil)

// maxIndexBytes caps how many bytes of a metadata/index page are read so a
// hostile or oversized mirror response can never exhaust memory (the consensus
// path must stay cheap — it is metadata, not a blob).
const maxIndexBytes = 4 << 20 // 4 MiB

// pep503SHA256 matches a "sha256=<64 hex>" URL fragment in a simple-index href.
var pep503SHA256 = regexp.MustCompile(`sha256=([0-9a-fA-F]{64})`)

// FetchDigest returns the sha256 digest the mirror advertises for ref, using a
// single metadata request. See the type doc for per-protocol behaviour.
func (f *HTTPMirrorDigestFetcher) FetchDigest(ctx context.Context, mirror ConsensusMirror, ref artifact.ArtifactRef) (string, error) {
	base := strings.TrimRight(mirror.BaseURL, "/")
	switch ref.Protocol {
	case "oci":
		return f.fetchOCIDigest(ctx, base, ref)
	case "pypi":
		return f.fetchPyPISHA256(ctx, base, ref)
	default:
		return "", fmt.Errorf("consensus: metadata-only sha256 digest not available for protocol %q on mirror %q", ref.Protocol, mirror.Name)
	}
}

// fetchOCIDigest issues a HEAD against the manifest (mutable/unresolved) or blob
// (resolved) endpoint and returns the Docker-Content-Digest header.
func (f *HTTPMirrorDigestFetcher) fetchOCIDigest(ctx context.Context, base string, ref artifact.ArtifactRef) (string, error) {
	var path string
	if ref.Mutable || ref.Digest == "" {
		path = "/v2/" + ref.Name + "/manifests/" + ref.Version
	} else {
		path = "/v2/" + ref.Name + "/blobs/" + ref.Digest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+path, nil)
	if err != nil {
		return "", err
	}
	// Accept the common manifest media types so registries return the digest for
	// the negotiated manifest rather than defaulting.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("consensus: oci HEAD %s returned HTTP %d", path, resp.StatusCode)
	}
	dcd := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if dcd == "" {
		return "", fmt.Errorf("consensus: oci HEAD %s returned no Docker-Content-Digest header", path)
	}
	return dcd, nil
}

// fetchPyPISHA256 fetches the small PEP 503 simple-index page for the package
// and extracts the sha256 advertised for the requested file (ref.Version is the
// distribution filename for a resolved pypi artifact).
func (f *HTTPMirrorDigestFetcher) fetchPyPISHA256(ctx context.Context, base string, ref artifact.ArtifactRef) (string, error) {
	filename := ref.Version
	if filename == "" {
		return "", fmt.Errorf("consensus: pypi ref has no filename to match")
	}
	url := base + "/simple/" + ref.Name + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("consensus: pypi GET %s returned HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return "", err
	}
	hex, ok := pep503DigestForFile(string(body), filename)
	if !ok {
		return "", fmt.Errorf("consensus: pypi index for %s has no sha256 for file %q", ref.Name, filename)
	}
	return "sha256:" + strings.ToLower(hex), nil
}

// pep503DigestForFile scans a simple-index HTML page for an anchor referencing
// filename and returns the sha256 hex from its "#sha256=" fragment. It matches
// the anchor whose href path segment ends with filename so a substring of a
// longer filename cannot be mistaken for it.
func pep503DigestForFile(html, filename string) (string, bool) {
	for _, seg := range strings.Split(html, "href=") {
		// Isolate the quoted href value: href="...."
		q := strings.IndexAny(seg, `"'`)
		if q < 0 {
			continue
		}
		rest := seg[q+1:]
		end := strings.IndexAny(rest, `"'`)
		if end < 0 {
			continue
		}
		href := rest[:end]
		// The path (before the fragment) must reference this exact filename.
		pathPart := href
		if h := strings.IndexByte(pathPart, '#'); h >= 0 {
			pathPart = pathPart[:h]
		}
		if !strings.HasSuffix(pathPart, "/"+filename) && pathPart != filename && !strings.HasSuffix(pathPart, filename) {
			continue
		}
		if m := pep503SHA256.FindStringSubmatch(href); m != nil {
			return m[1], true
		}
	}
	return "", false
}
