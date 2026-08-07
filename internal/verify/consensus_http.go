package verify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/publishmeta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// HTTPMirrorDigestFetcher is the production MirrorDigestFetcher: it resolves the
// content identity a mirror advertises for an artifact using ONLY a metadata
// request (an HTTP HEAD, or a small index/metadata GET) — never the full blob
// (DESIGN-REVIEW §1.2: "只比 digest/manifest，不下载全 blob").
//
// Per-protocol identities:
//
//   - pypi: PEP 503 "#sha256=<hex>" → returned as "sha256:<hex>" (CAS-comparable)
//   - oci:  Docker-Content-Digest header (CAS-comparable)
//   - npm:  packument versions[ver].dist.integrity (SSRI "sha512-…") — Content-ID
//     mode; never equated with CAS sha256
//   - cargo: sparse-index line cksum (sha256 hex) — Content-ID mode
//
// For ecosystems with no comparable metadata identity (generic tarball, …),
// FetchDigest returns an error so that mirror casts no vote.
type HTTPMirrorDigestFetcher struct {
	client *http.Client
}

// NewHTTPMirrorDigestFetcher builds a fetcher with a bounded HTTP client. A zero
// timeout uses a sane default; the per-call context still governs cancellation.
func NewHTTPMirrorDigestFetcher(timeout time.Duration) *HTTPMirrorDigestFetcher {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &HTTPMirrorDigestFetcher{client: &http.Client{
		Timeout:   timeout,
		Transport: upstream.WrapUserAgent(http.DefaultTransport),
	}}
}

// Compile-time assertion that HTTPMirrorDigestFetcher satisfies the interface.
var _ MirrorDigestFetcher = (*HTTPMirrorDigestFetcher)(nil)

// maxIndexBytes caps how many bytes of a metadata/index page are read so a
// hostile or oversized mirror response can never exhaust memory (the consensus
// path must stay cheap — it is metadata, not a blob).
const maxIndexBytes = 4 << 20 // 4 MiB

// maxPyPIIndexBytes is a PyPI-specific, larger cap than maxIndexBytes.
//
// A PEP 503 simple-index page lists EVERY historical release of a package —
// every sdist and every per-platform/per-ABI/per-Python-version wheel — in
// chronological (oldest-first) order, all on one page. For a prolific,
// multi-platform package this is not "small metadata": a real production
// pull of pydantic-core's index page measured ~7.7 MB, with the most recent
// release's anchor sitting past the 4 MiB mark. Under the old shared
// maxIndexBytes cap, io.ReadAll(io.LimitReader(...)) truncated the body
// silently (no error) before the target entry, and pep503DigestForFile then
// legitimately reported "not found" for a file that WAS listed on the page —
// indistinguishable from a genuine absence. Every mirror serves the same
// near-complete, similarly-ordered index, so this was not one flaky mirror:
// it deterministically zeroed out the vote count for every mirror at once,
// producing a "polled 0" consensus failure that looked like an infrastructure
// outage but was actually a parsing cap.
//
// 32 MiB comfortably covers realistic index sizes for even the most
// prolific PyPI packages while still bounding memory against a hostile or
// pathological mirror response.
const maxPyPIIndexBytes = 32 << 20 // 32 MiB

// FetchDigest returns the content identity the mirror advertises for ref, using a
// single metadata request. See the type doc for per-protocol behaviour.
func (f *HTTPMirrorDigestFetcher) FetchDigest(ctx context.Context, mirror ConsensusMirror, ref artifact.ArtifactRef) (string, error) {
	base := strings.TrimRight(mirror.BaseURL, "/")
	switch ref.Protocol {
	case "oci":
		return f.fetchOCIDigest(ctx, base, ref)
	case "pypi":
		return f.fetchPyPISHA256(ctx, base, ref)
	case "npm":
		return f.fetchNPMIntegrity(ctx, base, ref)
	case "cargo":
		return f.fetchCargoChecksum(ctx, base, ref)
	default:
		return "", fmt.Errorf("consensus: metadata-only content identity not available for protocol %q on mirror %q", ref.Protocol, mirror.Name)
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
//
// Root-cause A fix: for a resolved wheel/sdist, ref.Name is the hash-directory
// path (e.g. "b7/ce/149a00...") from the /packages/ tree, NOT the package name.
// The package name must be extracted from the filename (ref.Version) per PEP 427.
//
// Root-cause B fix: operator configs set base_url WITH a trailing "/simple"
// suffix (matching pip --index-url convention). Stripping it before appending
// "/simple/<pkg>/" prevents the double "/simple/simple/" path.
func (f *HTTPMirrorDigestFetcher) fetchPyPISHA256(ctx context.Context, base string, ref artifact.ArtifactRef) (string, error) {
	filename := ref.Version
	if filename == "" {
		return "", fmt.Errorf("consensus: pypi ref has no filename to match")
	}
	// Root-cause A: extract the PEP 503-normalised package name from the filename
	// so the correct /simple/<name>/ index URL is built.
	pkgName, ok := pypiPackageFromFilename(filename)
	if !ok {
		return "", fmt.Errorf("consensus: pypi: cannot extract package name from filename %q", filename)
	}
	// Root-cause B: strip any trailing /simple suffix that operator configs add.
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/simple")
	url := base + "/simple/" + pkgName + "/"
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
	// Stream-scan rather than buffer-then-parse: a full PEP 503 index page for
	// a prolific package can run into the tens of MB (see maxPyPIIndexBytes),
	// and the entry we want is usually near the END (chronological order).
	// html.NewTokenizer processes incrementally without holding the whole DOM
	// in memory, and pep503ScanForFile returns as soon as it finds a match —
	// so the common case (recent release, near the end of a large page) still
	// only pays for reading up to that point, not for a second buffering pass.
	lr := &io.LimitedReader{R: resp.Body, N: maxPyPIIndexBytes}
	hex, found, err := pep503ScanForFile(lr, filename)
	if err != nil {
		return "", fmt.Errorf("consensus: pypi index for %s: %w", pkgName, err)
	}
	if !found {
		if lr.N <= 0 {
			// The cap was reached before a match (or before EOF) was found.
			// This is DIFFERENT from "the file is genuinely not in the
			// index": it means the scan was cut short and we cannot say
			// whether the entry exists further on. Report it distinctly so
			// it is not mistaken for "PyPI doesn't have this file".
			return "", fmt.Errorf(
				"consensus: pypi index for %s exceeds %d byte scan cap before finding sha256 for file %q — index truncated, result inconclusive",
				pkgName, maxPyPIIndexBytes, filename,
			)
		}
		return "", fmt.Errorf("consensus: pypi index for %s has no sha256 for file %q", pkgName, filename)
	}
	return "sha256:" + strings.ToLower(hex), nil
}

// pypiPackageFromFilename extracts the PEP 503-normalised package name from a
// wheel (PEP 427) or sdist filename.
//
// Wheel:  {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
// Sdist:  {distribution}-{version}.tar.gz | .zip | .tar.bz2 | .tar.xz | .egg
//
// PEP 503 normalisation: lowercase, collapse runs of [-_.] to a single "-".
func pypiPackageFromFilename(filename string) (string, bool) {
	base := filename
	for _, ext := range []string{".whl", ".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".egg"} {
		if strings.HasSuffix(base, ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	// First component before the first "-" separator is the distribution name.
	idx := strings.IndexByte(base, '-')
	if idx <= 0 {
		return "", false
	}
	raw := base[:idx]
	if raw == "" {
		return "", false
	}
	// PEP 503: lowercase + collapse runs of [-_.] to single "-".
	var out strings.Builder
	prevSep := false
	for _, c := range strings.ToLower(raw) {
		if c == '-' || c == '_' || c == '.' {
			if !prevSep {
				out.WriteByte('-')
				prevSep = true
			}
		} else {
			out.WriteRune(c)
			prevSep = false
		}
	}
	result := out.String()
	if result == "" {
		return "", false
	}
	return result, true
}

// pep503DigestForFile parses a PEP 503 simple-index HTML page and returns the
// sha256 hex advertised for the given filename. Per PEP 503 the sha256 is in
// the URL fragment of an <a> element:
//
//	<a href="…/<filename>#sha256=<64-hex-chars>">…</a>
//
// This is a thin, byte-slice-compatible wrapper over pep503ScanForFile (the
// single canonical parsing implementation — see its doc comment) kept for
// callers/tests that already hold the full page in memory. It intentionally
// swallows the distinction between "genuinely not found" and "scan error"
// (both report found=false) since callers using this entrypoint already
// bypassed the streaming/cap machinery that makes that distinction
// meaningful.
func pep503DigestForFile(body []byte, filename string) (string, bool) {
	hex, found, _ := pep503ScanForFile(bytes.NewReader(body), filename)
	return hex, found
}

// pep503ScanForFile incrementally tokenizes a PEP 503 simple-index HTML
// stream (via golang.org/x/net/html's html.NewTokenizer, NOT html.Parse) and
// returns the sha256 hex advertised for filename as soon as a matching <a>
// tag is found — without ever buffering the full page or building a DOM
// tree. This is the single canonical parser for PEP 503 pages; both the
// production streaming fetch path (fetchPyPISHA256) and the legacy
// byte-slice wrapper (pep503DigestForFile) route through it so there is
// exactly one implementation of "how to read a PEP 503 page", per the
// project's one-logic-one-implementation rule.
//
// Using a real (streaming) HTML tokenizer rather than regex/string-splitting
// still handles any valid HTML a PyPI-compatible server might emit —
// whitespace variations, entity encoding, attribute ordering — while keeping
// memory bounded to the current token rather than the whole document.
//
// err is non-nil only for a genuine tokenizer error other than normal EOF; a
// clean end-of-stream with no match is reported as ("", false, nil).
func pep503ScanForFile(r io.Reader, filename string) (hex string, found bool, err error) {
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if tokErr := z.Err(); tokErr != io.EOF {
				return "", false, tokErr
			}
			return "", false, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr || string(name) != "a" {
				continue
			}
			for {
				key, val, more := z.TagAttr()
				if string(key) == "href" {
					if h, ok := extractSHA256FromHref(string(val), filename); ok {
						return h, true, nil
					}
				}
				if !more {
					break
				}
			}
		}
	}
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

// fetchNPMIntegrity GETs the package packument and returns versions[ver].dist.integrity
// (SSRI, typically "sha512-…"). ref.Version is the tarball filename
// (e.g. "left-pad-1.3.0.tgz"); the semver key is derived via VersionFromNPMTarball.
func (f *HTTPMirrorDigestFetcher) fetchNPMIntegrity(ctx context.Context, base string, ref artifact.ArtifactRef) (string, error) {
	if ref.Name == "" {
		return "", fmt.Errorf("consensus: npm ref has no package name")
	}
	ver := publishmeta.VersionFromNPMTarball(ref.Name, ref.Version)
	if ver == "" || ver == "packument" {
		return "", fmt.Errorf("consensus: npm ref has no version for integrity lookup")
	}
	url := base + "/" + npmPackumentPath(ref.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("consensus: npm GET %s returned HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return "", err
	}
	integrity, ok := npmIntegrityFromPackument(body, ver)
	if !ok {
		return "", fmt.Errorf("consensus: npm packument for %s has no dist.integrity for version %q", ref.Name, ver)
	}
	return integrity, nil
}

// npmPackumentPath encodes a package name for the registry packument URL.
// Scoped packages use "@scope%2Fname" (npm registry convention).
func npmPackumentPath(name string) string {
	if strings.HasPrefix(name, "@") {
		if i := strings.IndexByte(name, '/'); i > 0 {
			return name[:i] + "%2F" + name[i+1:]
		}
	}
	return name
}

// npmIntegrityFromPackument extracts versions[version].dist.integrity.
func npmIntegrityFromPackument(packument []byte, version string) (string, bool) {
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(packument, &doc); err != nil || doc.Versions == nil {
		return "", false
	}
	entry, ok := doc.Versions[version]
	if !ok {
		return "", false
	}
	integrity := strings.TrimSpace(entry.Dist.Integrity)
	if integrity == "" {
		return "", false
	}
	return integrity, true
}

// fetchCargoChecksum GETs the sparse-index document for the crate and returns
// the cksum field for vers == ref.Version (exact string as published — usually
// lowercase sha256 hex without a prefix).
func (f *HTTPMirrorDigestFetcher) fetchCargoChecksum(ctx context.Context, base string, ref artifact.ArtifactRef) (string, error) {
	if ref.Name == "" || ref.Version == "" {
		return "", fmt.Errorf("consensus: cargo ref missing name or version")
	}
	idxPath := cargoCrateIndexPath(ref.Name)
	if idxPath == "" {
		return "", fmt.Errorf("consensus: cargo: empty index path for %q", ref.Name)
	}
	url := base + "/" + idxPath
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
		return "", fmt.Errorf("consensus: cargo GET %s returned HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return "", err
	}
	cksum, ok := cargoChecksumFromIndex(body, ref.Version)
	if !ok {
		return "", fmt.Errorf("consensus: cargo index for %s has no cksum for version %q", ref.Name, ref.Version)
	}
	return cksum, nil
}

// cargoCrateIndexPath mirrors handler/cargo.CrateIndexPath (kept local so verify
// does not import the handler package).
func cargoCrateIndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 0:
		return ""
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	default:
		return n[:2] + "/" + n[2:4] + "/" + n
	}
}

// cargoChecksumFromIndex scans sparse-index NDJSON for vers and returns cksum.
func cargoChecksumFromIndex(index []byte, version string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(index))
	// Some crates have very wide lines; raise the scanner buffer.
	sc.Buffer(make([]byte, 0, 64*1024), maxIndexBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Vers  string `json:"vers"`
			Cksum string `json:"cksum"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Vers == version {
			ck := strings.TrimSpace(row.Cksum)
			if ck == "" {
				return "", false
			}
			return ck, true
		}
	}
	return "", false
}
