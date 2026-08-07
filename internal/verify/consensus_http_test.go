package verify

// Tests for HTTPMirrorDigestFetcher and its helpers (consensus_http.go).
//
// Traceable to:
//   - DESIGN-REVIEW §1.2 "只比 digest/manifest，不下载全 blob"
//   - PRD §G2 consensus tier: metadata-only sha256 fetch
//   - consensus_http.go doc: sha256 is available metadata-only for pypi and oci;
//     other protocols get an error (they must not claim consensus they didn't check)

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// extractSHA256FromHref (internal parsing helper)
// ─────────────────────────────────────────────────────────────────────────────

// TestExtractSHA256FromHref covers the core PEP 503 digest-extraction logic.
// Per spec, an <a href="…/filename#sha256=<64hex>"> conveys the sha256 for that
// file. Incorrect hrefs must not produce a false positive.
func TestExtractSHA256FromHref(t *testing.T) {
	const validHex = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	const filename = "requests-2.31.0-py3-none-any.whl"

	tests := []struct {
		name     string
		href     string
		filename string
		wantHex  string
		wantOK   bool
	}{
		{
			name:     "full URL path with sha256 fragment",
			href:     "https://files.pypi.org/packages/" + filename + "#sha256=" + validHex,
			filename: filename,
			wantHex:  validHex,
			wantOK:   true,
		},
		{
			name:     "relative path with sha256 fragment",
			href:     "../../packages/" + filename + "#sha256=" + validHex,
			filename: filename,
			wantHex:  validHex,
			wantOK:   true,
		},
		{
			name:     "filename only (no directory) with fragment",
			href:     filename + "#sha256=" + validHex,
			filename: filename,
			wantHex:  validHex,
			wantOK:   true,
		},
		{
			name:     "wrong filename: must not match",
			href:     "https://example.com/other-file.whl#sha256=" + validHex,
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "filename prefix collision: shorter name must not match longer path",
			href:     "https://example.com/my-package-extra.whl#sha256=" + validHex,
			filename: "my-package.whl",
			wantOK:   false,
		},
		{
			name:     "no fragment at all",
			href:     "https://example.com/" + filename,
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "wrong fragment algorithm (md5, not sha256)",
			href:     "https://example.com/" + filename + "#md5=abc",
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "sha256 fragment but too short (not 64 chars)",
			href:     "https://example.com/" + filename + "#sha256=tooshort",
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "sha256 fragment 63 chars (one too few)",
			href:     "https://example.com/" + filename + "#sha256=" + validHex[:63],
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "sha256 fragment 65 chars (one too many)",
			href:     "https://example.com/" + filename + "#sha256=" + validHex + "f",
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "empty href",
			href:     "",
			filename: filename,
			wantOK:   false,
		},
		{
			name:     "empty fragment",
			href:     filename + "#",
			filename: filename,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractSHA256FromHref(tc.href, tc.filename)
			assert.Equal(t, tc.wantOK, ok, "ok mismatch for href=%q filename=%q", tc.href, tc.filename)
			if tc.wantOK {
				assert.Equal(t, tc.wantHex, got, "hex value mismatch")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pep503DigestForFile (HTML parsing)
// ─────────────────────────────────────────────────────────────────────────────

// TestPep503DigestForFile verifies the HTML-based digest extraction logic with
// realistic PEP 503 simple-index pages.
func TestPep503DigestForFile(t *testing.T) {
	const validHex = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	const filename = "requests-2.31.0-py3-none-any.whl"

	t.Run("valid simple index page", func(t *testing.T) {
		html := fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>Links for requests</title></head>
<body>
<a href="/packages/%s#sha256=%s" data-requires-python="">%s</a>
<a href="/packages/requests-2.30.0.tar.gz#sha256=%s">requests-2.30.0.tar.gz</a>
</body></html>`, filename, validHex, filename, validHex)

		hex, ok := pep503DigestForFile([]byte(html), filename)
		assert.True(t, ok, "should find sha256 for the filename")
		assert.Equal(t, validHex, hex)
	})

	t.Run("file not listed in page", func(t *testing.T) {
		html := `<!DOCTYPE html><html><body>
<a href="/packages/other-pkg-1.0.whl#sha256=` + validHex + `">other-pkg-1.0.whl</a>
</body></html>`
		_, ok := pep503DigestForFile([]byte(html), filename)
		assert.False(t, ok, "should not find digest for unlisted file")
	})

	t.Run("malformed HTML but parseable", func(t *testing.T) {
		// golang.org/x/net/html is lenient; partial HTML is parsed.
		html := `<a href="/packages/` + filename + `#sha256=` + validHex + `">` + filename + `</a>`
		hex, ok := pep503DigestForFile([]byte(html), filename)
		assert.True(t, ok)
		assert.Equal(t, validHex, hex)
	})

	t.Run("empty page body", func(t *testing.T) {
		_, ok := pep503DigestForFile([]byte("<html><body></body></html>"), filename)
		assert.False(t, ok)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// NewHTTPMirrorDigestFetcher
// ─────────────────────────────────────────────────────────────────────────────

func TestNewHTTPMirrorDigestFetcher_ZeroTimeout_UsesDefault(t *testing.T) {
	f := NewHTTPMirrorDigestFetcher(0)
	require.NotNil(t, f)
	// With zero timeout, the fetcher should still have a non-zero client timeout.
	assert.NotZero(t, f.client.Timeout, "zero timeout must use sane default")
}

func TestNewHTTPMirrorDigestFetcher_ExplicitTimeout(t *testing.T) {
	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	require.NotNil(t, f)
	assert.Equal(t, 5*time.Second, f.client.Timeout)
}

func TestHTTPMirrorDigestFetcher_Interface(t *testing.T) {
	f := NewHTTPMirrorDigestFetcher(0)
	var _ MirrorDigestFetcher = f // compile-time interface assertion
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchDigest — OCI protocol (httptest server)
// ─────────────────────────────────────────────────────────────────────────────

// TestHTTPMirrorDigestFetcher_OCI_Success verifies that a HEAD response with a
// Docker-Content-Digest header returns the correct digest (metadata-only fetch).
func TestHTTPMirrorDigestFetcher_OCI_Success(t *testing.T) {
	const digest = "sha256:cafebabe00000000cafebabe00000000cafebabe00000000cafebabe00000000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodHead, r.Method, "OCI digest fetch must use HEAD")
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/nginx",
		Version:  "sha256:cafebabe00000000cafebabe00000000cafebabe00000000cafebabe00000000",
		Digest:   digest,
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)
	require.NoError(t, err)
	assert.Equal(t, digest, got)
}

// TestHTTPMirrorDigestFetcher_OCI_MutableRef verifies that a mutable tag
// (ref.Mutable=true or empty Digest) uses the manifests path, not blobs.
func TestHTTPMirrorDigestFetcher_OCI_MutableRef(t *testing.T) {
	const digest = "sha256:deadbeef00000000deadbeef00000000deadbeef00000000deadbeef00000000"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/nginx",
		Version:  "latest",
		Mutable:  true,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)
	require.NoError(t, err)
	assert.Equal(t, digest, got)
	assert.Contains(t, gotPath, "/manifests/", "mutable ref must use manifests path")
}

// TestHTTPMirrorDigestFetcher_OCI_NoDigestHeader verifies that a HEAD response
// without a Docker-Content-Digest header returns an error (the mirror cannot provide
// the digest metadata — it must not count as agreement).
func TestHTTPMirrorDigestFetcher_OCI_NoDigestHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally omit Docker-Content-Digest header.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/nginx",
		Version:  "sha256:abc",
		Digest:   "sha256:abc",
		Mutable:  false,
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err, "missing Docker-Content-Digest header must return error")
	assert.Contains(t, err.Error(), "Docker-Content-Digest")
}

// TestHTTPMirrorDigestFetcher_OCI_NonOKStatus verifies that a non-200 response
// returns an error (mirror is down/errored → no vote in consensus).
func TestHTTPMirrorDigestFetcher_OCI_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/nginx",
		Version:  "latest",
		Mutable:  true,
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err, "404 response must return error")
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchDigest — PyPI protocol (httptest server)
// ─────────────────────────────────────────────────────────────────────────────

const pypiTestHex = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

// TestHTTPMirrorDigestFetcher_PyPI_Success verifies that the PEP 503 simple-index
// page is fetched and the sha256 extracted for the requested filename.
func TestHTTPMirrorDigestFetcher_PyPI_Success(t *testing.T) {
	const pkgName = "requests"
	const filename = "requests-2.31.0-py3-none-any.whl"

	html := fmt.Sprintf(`<!DOCTYPE html><html><body>
<a href="/packages/%s#sha256=%s">%s</a>
</body></html>`, filename, pypiTestHex, filename)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method, "PyPI fetch must use GET")
		assert.Equal(t, "/simple/"+pkgName+"/", r.URL.Path)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     pkgName,
		Version:  filename, // for PyPI the Version field is the filename
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+pypiTestHex, got)
}

// TestHTTPMirrorDigestFetcher_PyPI_FileNotInIndex verifies that a file not listed
// in the simple index returns an error (mirror doesn't have this file → no vote).
func TestHTTPMirrorDigestFetcher_PyPI_FileNotInIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body></body></html>`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "requests",
		Version:  "requests-2.31.0-py3-none-any.whl",
		Mutable:  false,
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err, "file not in index must return error")
}

// TestHTTPMirrorDigestFetcher_PyPI_EmptyFilename verifies that an empty filename
// in the ref returns an error (guard against misconfigured refs).
func TestHTTPMirrorDigestFetcher_PyPI_EmptyFilename(t *testing.T) {
	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: "http://example.com"}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "requests",
		Version:  "", // empty filename
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err, "empty filename must return error")
}

// TestHTTPMirrorDigestFetcher_PyPI_ServerError verifies that a server-side
// error returns an error (mirror is unhealthy → no vote in consensus).
func TestHTTPMirrorDigestFetcher_PyPI_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "requests",
		Version:  "requests-2.31.0-py3-none-any.whl",
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err)
}

// TestHTTPMirrorDigestFetcher_PyPI_LargeIndexPastOldCap_Success reproduces the
// production incident: a real PEP 503 simple-index page for a prolific,
// multi-platform package (pydantic-core) runs well past the OLD 4 MiB
// maxIndexBytes cap (measured ~7.7 MB), with the target release's <a> anchor
// listed near the end (PyPI orders entries chronologically, oldest first).
//
// Under the old io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes)) code,
// this page would have been silently truncated before the anchor and every
// mirror would report "not found" — deterministically producing a
// consensus "polled 0" quorum failure across ALL configured mirrors despite
// every mirror actually having the file. This test builds a synthetic page
// past the old 4 MiB cap with the real target anchor near the end and
// asserts the fix (32 MiB maxPyPIIndexBytes cap + streaming tokenizer scan)
// finds it.
func TestHTTPMirrorDigestFetcher_PyPI_LargeIndexPastOldCap_Success(t *testing.T) {
	const pkgName = "pydantic-core"
	const filename = "pydantic_core-2.46.4-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"

	var buf bytes.Buffer
	buf.WriteString("<!DOCTYPE html><html><body>\n")
	// Pad with filler <a> entries (older releases) until well past the OLD
	// 4 MiB cap, mimicking PyPI's chronological (oldest-first) ordering —
	// the entry we want is appended LAST, after the padding.
	filler := `<a href="/packages/pydantic_core-0.0.1-cp38-cp38-manylinux_2_17_x86_64.whl#sha256=` +
		strings.Repeat("0", 64) + `">pydantic_core-0.0.1-cp38-cp38-manylinux_2_17_x86_64.whl</a>` + "\n"
	for buf.Len() < maxIndexBytes+3<<20 { // > old 4 MiB cap by 3 MiB
		buf.WriteString(filler)
	}
	fmt.Fprintf(&buf, `<a href="/packages/%s#sha256=%s">%s</a>`+"\n", filename, pypiTestHex, filename)
	buf.WriteString("</body></html>")

	require.Greater(t, buf.Len(), maxIndexBytes, "synthetic page must exceed the old 4 MiB cap")
	require.Less(t, buf.Len(), maxPyPIIndexBytes, "synthetic page must stay under the new 32 MiB cap")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(10 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     pkgName,
		Version:  filename,
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)
	require.NoError(t, err, "a page past the OLD 4 MiB cap must still be scanned successfully under the fix")
	assert.Equal(t, "sha256:"+pypiTestHex, got)
}

// TestHTTPMirrorDigestFetcher_PyPI_IndexExceedsNewCap_TruncatedError asserts
// that when a page exceeds even the new maxPyPIIndexBytes cap without a match
// being found, the resulting error is DISTINCTLY worded from the
// genuinely-not-found case (TestHTTPMirrorDigestFetcher_PyPI_FileNotInIndex) —
// callers/operators must be able to tell "we scanned everything, it's not
// there" apart from "we gave up scanning before we could tell".
func TestHTTPMirrorDigestFetcher_PyPI_IndexExceedsNewCap_TruncatedError(t *testing.T) {
	const filename = "pydantic_core-2.46.4-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"

	// Build the full oversized body up front (not written incrementally
	// against the live connection) — the fetcher is expected to stop reading
	// once its cap is hit, and a handler that keeps blocking on further
	// Write calls after the client stops reading is a self-inflicted test
	// hang, not a signal about the fetcher itself.
	var buf bytes.Buffer
	buf.WriteString("<!DOCTYPE html><html><body>\n")
	filler := `<a href="/packages/pydantic_core-0.0.1-cp38-cp38-manylinux_2_17_x86_64.whl#sha256=` +
		strings.Repeat("0", 64) + `">pydantic_core-0.0.1-cp38-cp38-manylinux_2_17_x86_64.whl</a>` + "\n"
	for buf.Len() < maxPyPIIndexBytes+1<<20 { // > new 32 MiB cap, target NEVER written
		buf.WriteString(filler)
	}
	buf.WriteString("</body></html>")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(30 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: srv.URL}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "pydantic-core",
		Version:  filename,
		Mutable:  false,
	}

	_, err := f.FetchDigest(t.Context(), mirror, ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated", "exceeding the cap must be reported distinctly from a genuine not-found")
	assert.Contains(t, err.Error(), "inconclusive")
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchDigest — unsupported protocols (no metadata content identity)
// ─────────────────────────────────────────────────────────────────────────────
// Covered in consensus_npm_cargo_http_test.go (npm/cargo now supported; tarball
// and others still error).
