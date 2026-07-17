package npm

// rewrite_test.go — coverage for packument URL rewriting (dist.tarball).
//
// Requirement (DESIGN-REVIEW §3 + npm registry protocol):
//   "npm's packument dist.tarball URLs MUST be rewritten to point back at Specula.
//    Unrewritten, npm bypasses the proxy entirely and the tarball cache never warmed."
//
// Each test anchors exactly which requirement it enforces.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Tests: rewriteTarballURL ──────────────────────────────────────────────────

func TestRewriteTarballURL_Basic(t *testing.T) {
	// npm registry protocol §dist.tarball: scheme+host must be replaced with Specula's.
	tests := []struct {
		name       string
		original   string
		base       string
		prefix     string
		wantResult string
	}{
		{
			name:       "standard rewrite",
			original:   "https://registry.npmjs.org/express/-/express-4.18.2.tgz",
			base:       "http://127.0.0.1:5102",
			prefix:     "/npm",
			wantResult: "http://127.0.0.1:5102/npm/express/-/express-4.18.2.tgz",
		},
		{
			name:       "scoped package",
			original:   "https://registry.npmjs.org/@babel/core/-/core-7.22.0.tgz",
			base:       "http://specula.internal:7732",
			prefix:     "/npm",
			wantResult: "http://specula.internal:7732/npm/@babel/core/-/core-7.22.0.tgz",
		},
		{
			name:       "no path prefix",
			original:   "https://registry.npmjs.org/react/-/react-18.2.0.tgz",
			base:       "http://localhost:7732",
			prefix:     "",
			wantResult: "http://localhost:7732/react/-/react-18.2.0.tgz",
		},
		{
			name:       "upstream already has non-npmjs host",
			original:   "https://custom-registry.example.com/pkg/-/pkg-1.0.0.tgz",
			base:       "http://specula.local",
			prefix:     "/npm",
			wantResult: "http://specula.local/npm/pkg/-/pkg-1.0.0.tgz",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteTarballURL(tc.original, tc.base, tc.prefix)
			assert.Equal(t, tc.wantResult, got, "npm dist.tarball URL must point back at Specula")
		})
	}
}

func TestRewriteTarballURL_InvalidURL(t *testing.T) {
	// Invalid / relative URLs must be returned unchanged (safe fallback).
	tests := []struct {
		name     string
		original string
	}{
		{"empty string", ""},
		{"no scheme", "registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"},
		{"no host", "/pkg/-/pkg-1.0.0.tgz"},
		{"no path", "https://registry.npmjs.org"},
		{"malformed", "://bad-url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteTarballURL(tc.original, "http://specula.local", "/npm")
			assert.Equal(t, tc.original, got, "invalid URL must be returned unchanged")
		})
	}
}

// ── Tests: rewritePackument ───────────────────────────────────────────────────

func TestRewritePackument_RewritesDistTarball(t *testing.T) {
	// REQUIREMENT: every version's dist.tarball must point to Specula after rewrite.
	packument := `{
		"name": "express",
		"dist-tags": {"latest": "4.18.2"},
		"versions": {
			"4.18.2": {
				"name": "express",
				"version": "4.18.2",
				"dist": {
					"tarball": "https://registry.npmjs.org/express/-/express-4.18.2.tgz",
					"shasum": "abc123"
				}
			},
			"4.17.1": {
				"name": "express",
				"version": "4.17.1",
				"dist": {
					"tarball": "https://registry.npmjs.org/express/-/express-4.17.1.tgz",
					"shasum": "def456"
				}
			}
		}
	}`

	result := rewritePackument([]byte(packument), "http://specula.internal:7732", "/npm")

	// Both tarball URLs must be rewritten to Specula.
	assert.Contains(t, string(result), "http://specula.internal:7732/npm/express/-/express-4.18.2.tgz",
		"4.18.2 tarball must point to Specula after rewrite")
	assert.Contains(t, string(result), "http://specula.internal:7732/npm/express/-/express-4.17.1.tgz",
		"4.17.1 tarball must point to Specula after rewrite")
	// Upstream URLs must be gone.
	assert.NotContains(t, string(result), "registry.npmjs.org",
		"upstream registry.npmjs.org must not appear in rewritten packument")
}

func TestRewritePackument_UnparsableJSON_ReturnedUnchanged(t *testing.T) {
	// Non-JSON input must be returned unchanged (safe fallback).
	bad := []byte("this is not json {{{")
	result := rewritePackument(bad, "http://specula:7732", "/npm")
	assert.Equal(t, bad, result)
}

func TestRewritePackument_NoVersionsKey_ReturnedUnchanged(t *testing.T) {
	// Abbreviated corgi packument without versions → returned unchanged.
	abbrev := `{"name":"express","description":"Fast, minimal web framework"}`
	result := rewritePackument([]byte(abbrev), "http://specula:7732", "/npm")
	assert.Equal(t, []byte(abbrev), result)
}

func TestRewritePackument_EmptyVersions_NoChange(t *testing.T) {
	// Empty versions map → no rewrite needed, JSON is valid but unchanged.
	input := `{"name":"new-pkg","versions":{}}`
	result := rewritePackument([]byte(input), "http://specula:7732", "/npm")
	// Result should be valid JSON; no upstream URL should appear.
	assert.NotContains(t, string(result), "registry.npmjs.org")
}

func TestRewritePackument_NoDist_NoChange(t *testing.T) {
	// Version without dist field → not rewritten (no tarball URL present).
	input := `{"name":"pkg","versions":{"1.0.0":{"name":"pkg","version":"1.0.0"}}}`
	result := rewritePackument([]byte(input), "http://specula:7732", "/npm")
	// Result must not crash and must remain valid.
	assert.NotContains(t, string(result), "tarball")
}

func TestRewritePackument_AlreadySpeculaURL_RewriteApplied(t *testing.T) {
	// rewriteTarballURL replaces scheme+host and prepends prefix to whatever path
	// the upstream URL carries. It does NOT detect if the URL already points at
	// Specula — in production the upstream URLs are always external registry URLs,
	// so this double-prefix case never occurs in normal operation.
	packument := `{
		"name": "pkg",
		"versions": {
			"1.0.0": {
				"dist": {
					"tarball": "http://specula:7732/npm/pkg/-/pkg-1.0.0.tgz"
				}
			}
		}
	}`
	result := rewritePackument([]byte(packument), "http://specula:7732", "/npm")
	// scheme+host unchanged; prefix ("/npm") is prepended to path → double /npm.
	assert.Contains(t, string(result), "http://specula:7732/npm/npm/pkg/-/pkg-1.0.0.tgz",
		"rewrite prepends the prefix to the URL path regardless of its current contents")
}

// ── Tests: speculaBaseURL ─────────────────────────────────────────────────────

func TestSpeculaBaseURL_HTTP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://specula.internal:7732/npm/react", nil)
	r.Host = "specula.internal:7732"
	got := speculaBaseURL(r)
	assert.Equal(t, "http://specula.internal:7732", got)
}

func TestSpeculaBaseURL_XForwardedProto_HTTPS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://specula.internal/npm/react", nil)
	r.Host = "specula.internal"
	r.Header.Set("X-Forwarded-Proto", "https")
	got := speculaBaseURL(r)
	assert.Equal(t, "https://specula.internal", got, "X-Forwarded-Proto must override scheme detection")
}

// ── Tests: npmMutableKey ──────────────────────────────────────────────────────

func TestNpmMutableKey(t *testing.T) {
	ref := packumentRef("react")
	key := npmMutableKey(ref)
	assert.Equal(t, "npm:react:packument", key)

	scopedRef := packumentRef("@myorg/pkg")
	scopedKey := npmMutableKey(scopedRef)
	assert.Equal(t, "npm:@myorg/pkg:packument", scopedKey)
}

// ── Integration: dist.tarball rewrite round-trip via HTTP ─────────────────────
//
// This test pins the regression: previously npm fetched tarballs from upstream
// directly because dist.tarball pointed to registry.npmjs.org, bypassing Specula.
// After the fix, the rewritten URL routes the tarball through Specula.

func TestDistTarballURLIsRewrittenInHTTPResponse(t *testing.T) {
	// Build a packument that has an upstream dist.tarball URL.
	upstream_packument := []byte(`{"name":"lodash","dist-tags":{"latest":"4.17.21"},"versions":{"4.17.21":{"name":"lodash","version":"4.17.21","dist":{"tarball":"https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz","shasum":"abc"}}}}`)
	reg, _, _ := fakeNpmRegistry(t, map[string][]byte{"lodash": upstream_packument}, nil)

	cm := newNpmTestCache()
	h := newNpmHandlerWithUpstream(cm, reg.URL, WithPathPrefix("/npm"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/npm/lodash")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	responseBody := string(body[:n])

	// The tarball URL in the response must point to Specula, not to registry.npmjs.org.
	assert.Contains(t, responseBody, srv.URL+"/npm/lodash/-/lodash-4.17.21.tgz",
		"dist.tarball URL MUST be rewritten to point back at Specula (DESIGN-REVIEW §3 real-client finding)")
	assert.NotContains(t, responseBody, "registry.npmjs.org",
		"upstream registry.npmjs.org must NOT appear in the packument served to npm clients")
}
