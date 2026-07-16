package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

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
	hex, ok := pep503DigestForFile(body, filename)
	if !ok {
		return "", fmt.Errorf("consensus: pypi index for %s has no sha256 for file %q", ref.Name, filename)
	}
	return "sha256:" + strings.ToLower(hex), nil
}

// pep503DigestForFile parses a PEP 503 simple-index HTML page using
// golang.org/x/net/html and returns the sha256 hex advertised for the given
// filename. Per PEP 503 the sha256 is in the URL fragment of an <a> element:
//
//	<a href="…/<filename>#sha256=<64-hex-chars>">…</a>
//
// Using a real HTML parser rather than regex/string-splitting ensures we
// handle any valid HTML that a PyPI-compatible server might emit, including
// whitespace variations, entity encoding, and attribute ordering.
func pep503DigestForFile(body []byte, filename string) (string, bool) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	return walkHTMLForDigest(doc, filename)
}

// walkHTMLForDigest recursively walks the HTML node tree looking for <a>
// elements whose href references filename with a sha256 fragment.
func walkHTMLForDigest(n *html.Node, filename string) (string, bool) {
	if n.Type == html.ElementNode && n.Data == "a" {
		for _, a := range n.Attr {
			if a.Key == "href" {
				if hex, ok := extractSHA256FromHref(a.Val, filename); ok {
					return hex, true
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hex, ok := walkHTMLForDigest(c, filename); ok {
			return hex, true
		}
	}
	return "", false
}

// extractSHA256FromHref splits an href on '#', checks the path suffix matches
// the exact filename, and extracts the sha256 hex from the fragment.
// Returns ("", false) when the href doesn't match or has no sha256 fragment.
func extractSHA256FromHref(href, filename string) (string, bool) {
	pathPart := href
	fragment := ""
	if h := strings.IndexByte(href, '#'); h >= 0 {
		pathPart = href[:h]
		fragment = href[h+1:]
	}
	// The path component must end with "/" + filename or equal filename exactly,
	// preventing a shorter name from matching a longer one (e.g. "foo" ≠ "foobar").
	if !strings.HasSuffix(pathPart, "/"+filename) && pathPart != filename {
		return "", false
	}
	const pfx = "sha256="
	if !strings.HasPrefix(fragment, pfx) {
		return "", false
	}
	hex := fragment[len(pfx):]
	// A SHA256 hex digest is always exactly 64 hex characters.
	if len(hex) != 64 {
		return "", false
	}
	return hex, true
}
