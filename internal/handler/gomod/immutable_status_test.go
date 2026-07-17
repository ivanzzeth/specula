package gomod

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGomodImmutable_Upstream404_Returns404 is the load-bearing regression test
// for BUG 1. An immutable .info/.mod/.zip fetch that a REAL upstream answers
// with 404 must surface to the go client as 404 — the GOPROXY protocol requires
// 404/410 for "this module/version does not exist" so the client can resolve
// module-path boundaries (probe .../a/b/c, then .../a/b, then .../a). A 502 is a
// hard transport failure that aborts that walk.
//
// The upstream is the production fallbackClient (upstream.NewClient) driven
// against a REAL httptest server returning 404: a generic double that returned
// an opaque error could not distinguish a clean 404 from a transport failure —
// which is exactly how this shipped.
//
// RED before the fix: serveImmutable maps every coalescedFetch error to 502.
func TestGomodImmutable_Upstream404_Returns404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer up.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, up.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, file := range []string{"v9.9.9.info", "v9.9.9.mod", "v9.9.9.zip"} {
		resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/" + file)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equalf(t, http.StatusNotFound, resp.StatusCode,
			"upstream 404 for %s must surface as 404, got %d (body=%q)", file, resp.StatusCode, body)
	}
}

// TestGomodImmutable_Upstream410_Returns410 checks 410 Gone is preserved too
// (the GOPROXY protocol treats 410 the same as 404 for does-not-exist).
func TestGomodImmutable_Upstream410_Returns410(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer up.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, up.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equalf(t, http.StatusGone, resp.StatusCode,
		"upstream 410 must surface as 410, got %d (body=%q)", resp.StatusCode, body)
}

// TestGomodMutable_Upstream404_Returns404 covers the MUTABLE @v/list and
// @latest endpoints (serveMutable). The go client probes these to resolve
// module-path boundaries — for example.com/a/b/c it asks .../a/b/c/@v/list,
// then .../a/b/@v/list, then .../a/@v/list — so a parent-path prefix that is
// not a module must surface goproxy.cn's 404, not a 502 that aborts the walk.
//
// RED before the fix: serveMutable flattens the fresh-fetch error to 502.
func TestGomodMutable_Upstream404_Returns404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer up.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, up.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/github.com/pkg/@v/list", "/github.com/pkg/@latest"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equalf(t, http.StatusNotFound, resp.StatusCode,
			"mutable %s upstream 404 must surface as 404, got %d (body=%q)", path, resp.StatusCode, body)
	}
}

// TestGomodMutable_UpstreamConnRefused_Returns502 is the mutable-path guardrail:
// a genuine transport failure must stay 502.
func TestGomodMutable_UpstreamConnRefused_Returns502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, deadURL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/pkg/errors/@v/list")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equalf(t, http.StatusBadGateway, resp.StatusCode,
		"mutable-path transport failure must stay 502, got %d (body=%q)", resp.StatusCode, body)
}

// TestGomodImmutable_UpstreamConnRefused_Returns502 proves we do NOT swing the
// other way: a genuine transport failure must STAY 502. Turning a real outage
// into a 404 would make the go client cache a "module does not exist" for a
// module that actually does.
func TestGomodImmutable_UpstreamConnRefused_Returns502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // address now refuses connections — a real transport failure

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, deadURL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equalf(t, http.StatusBadGateway, resp.StatusCode,
		"a genuine upstream connection failure must stay 502, got %d (body=%q)", resp.StatusCode, body)
}
