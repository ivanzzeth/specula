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

// maxNPMPackumentBytes is the npm counterpart of maxPyPIIndexBytes, and exists
// for exactly the same reason — the PyPI fix above was never carried across to
// npm, so the identical bug stayed live on this path.
//
// An npm packument lists EVERY published version of a package in one JSON
// document, oldest-first, and the FULL form embeds each version's README and
// metadata. Measured against the live registries: react's full packument is
// ~6.9 MB. Under the old shared 4 MiB maxIndexBytes cap, io.LimitReader
// truncated the JSON mid-document; json.Unmarshal then failed and every mirror
// was recorded as "no dist.integrity for version X". Because all mirrors serve
// the same oversized document, this zeroed the vote count for all of them at
// once — surfacing as "0 of 2 mirrors responded", which reads like an outage
// but was a parsing cap. Observed as `npm ci` failing every tarball with
// HTTP 502 "tarball failed integrity verification".
//
// Requesting the abbreviated packument (see acceptNPMAbbreviated) cuts react to
// ~2.9 MB, but that is a request, not a guarantee: mirrors may ignore the
// Accept header (huaweicloud returns the full 6.9 MB document regardless). So
// the cap must accommodate full packuments on its own.
//
// The number is NOT a parsing limit — the parser streams, so peak memory is one
// version entry regardless of document size (see
// npmIntegrityFromPackumentStream). It is only a stop against a stream that
// never ends. Sized well above the worst real observation (vite full: ~38.9 MB
// from a mirror that ignores the Accept header) so that a badly-behaved mirror
// cannot be silently excluded from the quorum.
const maxNPMPackumentBytes = 256 << 20 // 256 MiB

// acceptNPMAbbreviated is npm's abbreviated-packument media type. It carries
// dist.integrity (all we need) without the per-version README bulk, and the
// registry falls back to the full document if it does not understand it.
const acceptNPMAbbreviated = "application/vnd.npm.install-v1+json, application/json"

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
	pkgName, ok := PyPIPackageFromFilename(filename)
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

// PyPIPackageFromFilename extracts the PEP 503-normalised package name from a
// wheel (PEP 427) or sdist filename.
//
// Wheel:  {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
// Sdist:  {distribution}-{version}.tar.gz | .zip | .tar.bz2 | .tar.xz | .egg
//
// The name/version split direction depends on the archive type:
//
//   - Wheel: PEP 427 requires build backends to escape every run of "-_." in
//     the {distribution} field to a single "_", so the distribution segment
//     is guaranteed dash-free. Splitting at the FIRST "-" is therefore always
//     correct and unambiguous.
//   - Sdist/egg: the filename uses the raw, un-escaped PyPI project name,
//     which for many real packages contains literal hyphens (e.g.
//     "alibabacloud-tea-0.4.3.tar.gz", "scikit-learn-1.3.0.tar.gz"). Splitting
//     at the first "-" truncates the name (e.g. "alibabacloud" instead of
//     "alibabacloud-tea"), which then 404s identically on every mirror and
//     surfaces as "polled 0" — a different bug from the index-truncation one.
//     PEP 440 canonical version strings never contain a literal "-" (pre/post/
//     dev/local segments use ".", "a"/"b"/"rc", or "+"), so the LAST "-" is
//     the unambiguous name/version boundary regardless of hyphens in the name.
//
// PEP 503 normalisation: lowercase, collapse runs of [-_.] to a single "-".
func PyPIPackageFromFilename(filename string) (string, bool) {
	base := filename
	isWheel := false
	for _, ext := range []string{".whl", ".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".egg"} {
		if strings.HasSuffix(base, ext) {
			base = base[:len(base)-len(ext)]
			isWheel = ext == ".whl"
			break
		}
	}
	// Wheel: first "-" (PEP 427 escaping guarantees a dash-free name field).
	// Sdist/egg/unrecognised: last "-" (PEP 440 versions never contain "-",
	// so it unambiguously separates a possibly-hyphenated name from the
	// version even when the name itself has internal hyphens).
	var idx int
	if isWheel {
		idx = strings.IndexByte(base, '-')
	} else {
		idx = strings.LastIndexByte(base, '-')
	}
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
	// Ask for the abbreviated packument: same dist.integrity, without the
	// per-version README bulk (react: ~6.9 MB → ~2.9 MB). Registries that do
	// not understand it fall back to the full document, which is why the cap
	// below must still accommodate full packuments.
	req.Header.Set("Accept", acceptNPMAbbreviated)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("consensus: npm GET %s returned HTTP %d", url, resp.StatusCode)
	}
	// STREAM the document instead of buffering it.
	//
	// A mirror is free to ignore the Accept header above: huaweicloud returns
	// vite's FULL packument (~38.9 MB) where the mirrors that honour it send
	// ~2.2 MB. Buffering meant the byte cap had to cover the worst-behaved
	// mirror — i.e. that mirror got to dictate everyone's memory ceiling, and
	// falling short of it made the mirror unable to vote, which silently
	// lowered the effective quorum.
	//
	// Streaming decouples the two: peak memory is one version entry, so the cap
	// is only a guard against a genuinely hostile stream, not a parsing limit.
	counted := &countingReader{r: io.LimitReader(resp.Body, maxNPMPackumentBytes)}
	integrity, err := npmIntegrityFromPackumentStream(counted, ver)
	if err != nil {
		// Hitting the guard looks like a mid-document EOF to the decoder. Say
		// "truncated" rather than "malformed": the first is our own limit, the
		// second blames the mirror for bytes it may well have sent correctly.
		if counted.n >= maxNPMPackumentBytes {
			return "", fmt.Errorf(
				"consensus: npm packument for %s exceeds %d byte stream guard before version %q could be resolved — packument truncated, result inconclusive",
				ref.Name, maxNPMPackumentBytes, ver)
		}
		return "", fmt.Errorf("consensus: npm packument for %s: %w", ref.Name, err)
	}
	if integrity == "" {
		return "", fmt.Errorf("consensus: npm packument for %s has no dist.integrity for version %q", ref.Name, ver)
	}
	return integrity, nil
}

// countingReader tracks how many bytes were consumed, so a decode failure at
// exactly the stream guard can be attributed to the guard rather than reported
// as a malformed document.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// npmIntegrityFromPackumentStream scans versions[ver].dist.integrity without
// holding the whole document in memory, and stops as soon as it finds the
// target version.
//
// Returns ("", nil) when the document parsed fine but the version is absent —
// kept distinct from an error so consensus can tell "this mirror says no such
// version" (a real vote) apart from "this mirror could not be read" (an
// infrastructure failure). Conflating those two is what made an oversized
// packument look like a mirror outage.
func npmIntegrityFromPackumentStream(r io.Reader, version string) (string, error) {
	dec := json.NewDecoder(r)

	// Walk to the "versions" object without materialising anything else.
	if _, err := dec.Token(); err != nil { // opening '{'
		return "", fmt.Errorf("malformed packument: %w", err)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("malformed packument: %w", err)
		}
		key, _ := keyTok.(string)
		if key != "versions" {
			// Skip this whole value (README, times, dist-tags, …).
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return "", fmt.Errorf("malformed packument: %w", err)
			}
			continue
		}

		if _, err := dec.Token(); err != nil { // '{' of versions
			return "", fmt.Errorf("malformed versions object: %w", err)
		}
		for dec.More() {
			verTok, err := dec.Token()
			if err != nil {
				return "", fmt.Errorf("malformed versions object: %w", err)
			}
			verKey, _ := verTok.(string)

			// Decode only the entry we actually want; skip the rest whole.
			if verKey != version {
				var discard json.RawMessage
				if err := dec.Decode(&discard); err != nil {
					return "", fmt.Errorf("malformed version entry %q: %w", verKey, err)
				}
				continue
			}
			var entry struct {
				Dist struct {
					Integrity string `json:"integrity"`
				} `json:"dist"`
			}
			if err := dec.Decode(&entry); err != nil {
				return "", fmt.Errorf("malformed version entry %q: %w", verKey, err)
			}
			return strings.TrimSpace(entry.Dist.Integrity), nil
		}
		// "versions" existed but did not contain the target.
		return "", nil
	}
	return "", nil
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
	// Read one byte past the cap so "exactly at the limit" stays distinguishable
	// from "overran it".
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxIndexBytes {
		// Same distinction the npm and PyPI paths make: truncated (we never got
		// to look) must not be reported as absent (we looked, it is not there).
		// The cap is NOT raised here — the largest real sparse index measured
		// ~1.3 MB (aws-sdk-ec2) against a 4 MiB cap, and NDJSON carries no
		// README bulk that could close that gap. What was missing was only the
		// ability to tell the two failures apart.
		return "", fmt.Errorf(
			"consensus: cargo index for %s exceeds %d byte scan cap before version %q could be resolved — index truncated, result inconclusive",
			ref.Name, maxIndexBytes, ref.Version)
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
