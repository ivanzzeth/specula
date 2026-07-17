package verify

// Regression tests for Bug 1: consensus tier polls ZERO mirrors for PyPI.
//
// Root-cause A: ref.Name for a resolved wheel is the hash-directory path (e.g.
// "b7/ce/149a00...c5e"), not the package name. fetchPyPISHA256 used ref.Name
// to build the /simple/<name>/ index URL, producing a 404 on every mirror.
//
// Root-cause B: operator configs write the PyPI mirror base_url WITH a
// trailing "/simple" suffix (matching pip --index-url convention). The
// consensus mirror list gets these raw base URLs. Without stripping the suffix,
// the constructed URL becomes ".../simple/simple/<name>/" — a double-/simple
// path that every conformant PyPI mirror returns 404 for.
//
// Both defects result in "polled 0; disagreements: none" in the consensus log
// and an immediate 502 for every pip install when consensus is enabled.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestFetchPyPISHA256_WheelRef_UsesPackageName is the RED test for root-cause A.
//
// In production, a wheel's ArtifactRef has:
//
//	ref.Name    = "b7/ce/149a00...c5e"  (the hash directory path from /packages/)
//	ref.Version = "six-1.17.0-py2.py3-none-any.whl"  (the filename)
//
// Before the fix, fetchPyPISHA256 built the URL as:
//
//	<base>/simple/b7/ce/149a00...c5e/
//
// which is a 404 on every PyPI mirror.
//
// After the fix, the package name is extracted from ref.Version (filename) so
// the correct URL is built: <base>/simple/six/
//
// This test FAILS before the fix (the URL path assertion fails: the server
// receives the wrong path and cannot match it to the expected /simple/six/).
func TestFetchPyPISHA256_WheelRef_UsesPackageName(t *testing.T) {
	// Real SHA256 of six-1.17.0-py2.py3-none-any.whl (64 hex chars).
	const expectedHex = "755d2f8a97de0734d8a5ae31e43eef1330c7e19b31d640b60f0a74ecf234c0b0"
	const wheelFilename = "six-1.17.0-py2.py3-none-any.whl"

	// Serve a minimal PEP 503 simple-index page for the package "six".
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := fmt.Sprintf(`<!DOCTYPE html><html><body>
<a href="/packages/b7/ce/149a00c.../%s#sha256=%s">%s</a>
</body></html>`, wheelFilename, expectedHex, wheelFilename)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "tuna", BaseURL: srv.URL}

	// Production ref: Name is the hash directory path, NOT the package name.
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "b7/ce/149a00c...path", // hash directory — NOT "six"
		Version:  wheelFilename,
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)

	// Before fix: err != nil because the server receives the wrong path and
	// either 404s or cannot find the file in the HTML it served.
	// After fix: err == nil and the path received is /simple/six/
	require.NoError(t, err, "must succeed when package name is extracted from filename")
	assert.Equal(t, "/simple/six/", gotPath,
		"URL must use the package name extracted from the filename, not the hash directory path")
	assert.Equal(t, "sha256:"+expectedHex, got)
}

// TestFetchPyPISHA256_BaseURLWithSimpleSuffix_NoDoublePath is the RED test for
// root-cause B.
//
// Operator configs (and the live /tmp/e2e-manual/c.yaml) write PyPI mirror
// base_url WITH a trailing "/simple" suffix, matching pip's --index-url
// convention. Without stripping it, the consensus fetcher constructs:
//
//	https://pypi.tuna.tsinghua.edu.cn/simple/simple/six/
//	(double /simple)
//
// which returns 404 on every conformant PyPI mirror.
//
// This test FAILS before the fix (the server receives a path with "/simple/simple/").
func TestFetchPyPISHA256_BaseURLWithSimpleSuffix_NoDoublePath(t *testing.T) {
	// Real SHA256 of six-1.17.0-py2.py3-none-any.whl (64 hex chars).
	const expectedHex = "755d2f8a97de0734d8a5ae31e43eef1330c7e19b31d640b60f0a74ecf234c0b0"
	const wheelFilename = "six-1.17.0-py2.py3-none-any.whl"
	const pkgName = "six"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := fmt.Sprintf(`<!DOCTYPE html><html><body>
<a href="/packages/%s#sha256=%s">%s</a>
</body></html>`, wheelFilename, expectedHex, wheelFilename)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	// Base URL WITH trailing "/simple" — the typical operator configuration.
	mirror := ConsensusMirror{Name: "tuna", BaseURL: srv.URL + "/simple"}

	// Normal ref with the package name in Name (testing /simple suffix stripping).
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     pkgName,
		Version:  wheelFilename,
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)

	// Before fix: gotPath = "/simple/simple/six/" (double /simple) → the HTML
	// served by srv is for the wrong path so the file lookup fails with an error.
	// After fix: gotPath = "/simple/six/" (exactly once).
	require.NoError(t, err,
		"base URL with trailing /simple must not produce a double /simple path")
	assert.Equal(t, "/simple/"+pkgName+"/", gotPath,
		"URL must contain /simple/ exactly once even when base_url ends in /simple")
	assert.Equal(t, "sha256:"+expectedHex, got)
}

// TestFetchPyPISHA256_WheelRef_WithSimpleSuffix_BothFixes tests that BOTH
// root-cause A (hash-directory Name) and root-cause B (/simple suffix) are fixed
// simultaneously — this is the real production scenario where the two bugs compound.
func TestFetchPyPISHA256_WheelRef_WithSimpleSuffix_BothFixes(t *testing.T) {
	// Real SHA256 of six-1.17.0-py2.py3-none-any.whl (64 hex chars).
	const expectedHex = "755d2f8a97de0734d8a5ae31e43eef1330c7e19b31d640b60f0a74ecf234c0b0"
	const wheelFilename = "six-1.17.0-py2.py3-none-any.whl"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := fmt.Sprintf(`<!DOCTYPE html><html><body>
<a href="/packages/b7/%s#sha256=%s">%s</a>
</body></html>`, wheelFilename, expectedHex, wheelFilename)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	// Both bugs present: base has /simple suffix AND Name is a hash directory.
	mirror := ConsensusMirror{Name: "tuna", BaseURL: srv.URL + "/simple"}
	ref := artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "b7/ce/149a00c", // hash directory path
		Version:  wheelFilename,
		Mutable:  false,
	}

	got, err := f.FetchDigest(t.Context(), mirror, ref)

	require.NoError(t, err)
	assert.Equal(t, "/simple/six/", gotPath)
	assert.Equal(t, "sha256:"+expectedHex, got)
}

// TestPypiPackageFromFilename_WheelAndSdist unit-tests the filename parser that
// extracts the package name from a wheel or sdist filename per PEP 427.
// Tests are RED before the helper function exists.
func TestPypiPackageFromFilename_WheelAndSdist(t *testing.T) {
	cases := []struct {
		filename    string
		wantPackage string
		wantOK      bool
	}{
		// Wheels (PEP 427: {dist}-{ver}-{python}-{abi}-{platform}.whl)
		{"six-1.17.0-py2.py3-none-any.whl", "six", true},
		{"requests-2.31.0-py3-none-any.whl", "requests", true},
		{"Pillow-10.0.0-cp311-cp311-manylinux_2_17_x86_64.whl", "pillow", true},
		{"python_dateutil-2.8.2-py2.py3-none-any.whl", "python-dateutil", true},
		// Sdists
		{"six-1.17.0.tar.gz", "six", true},
		{"requests-2.31.0.zip", "requests", true},
		{"Django-4.2.tar.gz", "django", true},
		// Unrecognised
		{"notapackage", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			got, ok := pypiPackageFromFilename(tc.filename)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantPackage, got)
			}
		})
	}
}
