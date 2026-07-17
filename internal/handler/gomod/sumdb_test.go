package gomod

// sumdb_test.go — coverage for sumdb_passthrough.go:
//   • SumDBHandler.ServeHTTP (0% → covered)
//   • SumDBHandler.serve: method guard, private-module 403, proxy success/fail
//   • moduleFromLookup edge cases
//   • WithSumDBPrivateMatcher, WithSumDBHTTPClient, WithSumDBLogger options

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/verify"
)

// ── fakeUpstreamSumDB — minimal fake sumdb upstream ──────────────────────────

func fakeSumDBUpstream(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── Tests: moduleFromLookup ────────────────────────────────────────────────────

func TestModuleFromLookup_FullLookup(t *testing.T) {
	mod, ok := moduleFromLookup("sum.golang.org/lookup/github.com/foo/bar@v1.0.0")
	assert.True(t, ok)
	assert.Equal(t, "github.com/foo/bar", mod)
}

func TestModuleFromLookup_NonLookup(t *testing.T) {
	// Non-lookup endpoints (supported, latest, tile) must return false.
	for _, sub := range []string{
		"sum.golang.org/supported",
		"sum.golang.org/latest",
		"sum.golang.org/tile/8/0/001",
	} {
		t.Run(sub, func(t *testing.T) {
			_, ok := moduleFromLookup(sub)
			assert.False(t, ok)
		})
	}
}

func TestModuleFromLookup_NoAtVersion(t *testing.T) {
	// lookup path without @version → false.
	_, ok := moduleFromLookup("sum.golang.org/lookup/github.com/foo/bar")
	assert.False(t, ok)
}

func TestModuleFromLookup_EmptyModuleBeforeAt(t *testing.T) {
	// "@v1.0.0" with empty module → false.
	_, ok := moduleFromLookup("sum.golang.org/lookup/@v1.0.0")
	assert.False(t, ok)
}

// ── Tests: SumDBHandler.ServeHTTP (standalone handler mount) ─────────────────

func TestSumDBHandlerServeHTTP_ProxiesRequest(t *testing.T) {
	fakeResp := "github.com/foo/bar@v1.0.0\nhash: h1:abc...\n"
	// When SumDBHandler.ServeHTTP receives /sumdb/lookup/…, it strips /sumdb
	// and constructs targetURL as upstreamURL+"/lookup/…".
	upSrv := fakeSumDBUpstream(t, map[string]string{
		"/lookup/github.com/foo/bar@v1.0.0": fakeResp,
	})

	h := NewSumDBHandler(upSrv.URL)
	// Mount without StripPrefix: ServeHTTP expects the path to contain "/sumdb".
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/lookup/github.com/foo/bar@v1.0.0")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, fakeResp, string(body))
}

func TestSumDBHandlerServeHTTP_NotSumDBPath(t *testing.T) {
	h := NewSumDBHandler("https://sum.golang.org")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/not/sumdb", nil)
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── Tests: SumDBHandler.serve — the actual dispatch ───────────────────────────

func TestSumDBServe_MethodNotAllowed(t *testing.T) {
	h := NewSumDBHandler("https://sum.golang.org")
	// Wire via the parent gomod handler's /sumdb/ route.
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sumdb/sum.golang.org/lookup/github.com/foo@v1.0.0", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
}

func TestSumDBServe_EmptySumdbName(t *testing.T) {
	// Path after /sumdb is empty → 404 "missing sumdb name".
	h := NewSumDBHandler("https://sum.golang.org")
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	// /sumdb with no trailing name
	resp, err := http.Get(srv.URL + "/sumdb")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSumDBServe_PrivateModule_403(t *testing.T) {
	// DESIGN-REVIEW H5: private module lookup must return 403, never forwarded upstream.
	matcher := verify.NewPrivateMatcher([]string{"git.corp.example.com/*"})
	upSrv := fakeSumDBUpstream(t, map[string]string{})

	h := NewSumDBHandler(upSrv.URL, WithSumDBPrivateMatcher(matcher))
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/sum.golang.org/lookup/git.corp.example.com/secret@v1.0.0")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"DESIGN-REVIEW H5: private module must be blocked at sumdb passthrough (GONOSUMDB: never forward to public sumdb)")
}

func TestSumDBServe_PublicModule_Proxied(t *testing.T) {
	fakeResp := "github.com/foo/bar@v1.0.0\nh1:abc...\n"
	// When routed via gomod handler, the gomod handler strips /sumdb, so
	// serve() receives "/sum.golang.org/lookup/…" and targetURL becomes
	// upstreamURL+"/sum.golang.org/lookup/…".
	upSrv := fakeSumDBUpstream(t, map[string]string{
		"/sum.golang.org/lookup/github.com/foo/bar@v1.0.0": fakeResp,
	})

	matcher := verify.NewPrivateMatcher([]string{"git.corp.example.com/*"})
	h := NewSumDBHandler(upSrv.URL, WithSumDBPrivateMatcher(matcher))
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/sum.golang.org/lookup/github.com/foo/bar@v1.0.0")
	require.NoError(t, err)
	defer resp.Body.Close()
	// Non-private → proxied to upstream and returns 200.
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"public module must be proxied to upstream sumdb and return 200")
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, fakeResp, string(body))
}

func TestSumDBServe_UpstreamError_502(t *testing.T) {
	// Upstream that is down → 502.
	h := NewSumDBHandler("http://127.0.0.1:0") // nothing listening
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/sum.golang.org/lookup/github.com/ok/pkg@v1.0.0")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestSumDBServe_HEAD_NoBody(t *testing.T) {
	fakeResp := "hash-data\n"
	// Via gomod handler: targetURL = upSrv.URL + "/sum.golang.org/latest"
	upSrv := fakeSumDBUpstream(t, map[string]string{
		"/sum.golang.org/latest": fakeResp,
	})

	h := NewSumDBHandler(upSrv.URL)
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/sumdb/sum.golang.org/latest", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	// HEAD must not return body.
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body)
}

func TestSumDBServe_AcceptHeader_Forwarded(t *testing.T) {
	// The Accept header from the go client should be forwarded upstream.
	var gotAccept string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer upSrv.Close()

	h := NewSumDBHandler(upSrv.URL)
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	defer srv.Close()

	// Via gomod handler: path /sumdb/sum.golang.org/tile/8/0/001 → proxied.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sumdb/sum.golang.org/tile/8/0/001", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "application/json", gotAccept, "Accept header must be forwarded to upstream sumdb")
}

// ── Tests: WithSumDB option options ──────────────────────────────────────────

func TestWithSumDBOptions(t *testing.T) {
	upSrv := fakeSumDBUpstream(t, nil)
	h := NewSumDBHandler(upSrv.URL,
		WithSumDBHTTPClient(&http.Client{}),
		WithSumDBLogger(nil),
		WithSumDBPrivateMatcher(verify.NewPrivateMatcher(nil)),
	)
	assert.NotNil(t, h)
	assert.Equal(t, upSrv.URL, h.upstreamURL)
}

// ── Tests: gomod handler /sumdb route when sumdb is not configured ────────────

func TestGomodHandler_SumDB_NotConfigured_404(t *testing.T) {
	// When no SumDB sub-handler is configured, /sumdb requests → 404.
	h := NewHandler(newGomodTestCache()) // no WithSumDB
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/sum.golang.org/latest")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
