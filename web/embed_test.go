package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// A binary built without `make ui` must still boot and serve the control plane.
// Before the committed fallback page this panicked in newHandler
// ("webui: read dist/index.html"), which killed the data plane too — a whole
// cluster lost its mirror because a frontend asset was missing.
func TestNoIndexServesFallbackInsteadOfPanicking(t *testing.T) {
	h := newHandler(false, fstest.MapFS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "WebUI not built") {
		t.Fatalf("expected the fallback page, got:\n%s", truncate(body))
	}
	// The page must tell the operator how to fix it, not just that it is broken.
	if !strings.Contains(body, "make ui") {
		t.Fatalf("fallback page must carry the build command:\n%s", truncate(body))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// SPA routes hit the same catch-all, so a deep link must also degrade to the
// fallback rather than 404 or panic.
func TestNoIndexFallbackOnDeepLink(t *testing.T) {
	h := newHandler(false, fstest.MapFS{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cache/oci", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WebUI not built") {
		t.Fatalf("status=%d body=%s", rec.Code, truncate(rec.Body.String()))
	}
}

// The fallback must never shadow a real build.
func TestRealIndexWins(t *testing.T) {
	h := newHandler(false, fstest.MapFS{
		"index.html": {Data: []byte("<html><head></head><body>real spa</body></html>")},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "real spa") {
		t.Fatalf("expected the real index, got:\n%s", truncate(body))
	}
	if strings.Contains(body, "WebUI not built") {
		t.Fatal("fallback leaked into a real build")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("index.html must not be long-cached, got %q", cc)
	}
}

func TestDevModeInjectsAppEnv(t *testing.T) {
	h := newHandler(true, fstest.MapFS{
		"index.html": {Data: []byte("<html><head></head><body>x</body></html>")},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `window.__APP_ENV__="dev"`) {
		t.Fatalf("dev script not injected:\n%s", truncate(rec.Body.String()))
	}
}

// Vite asset names carry content hashes → immutable long cache. index.html
// must not, or a new deploy serves an index referencing dead chunks.
func TestHashedAssetsGetImmutableCache(t *testing.T) {
	h := newHandler(false, fstest.MapFS{
		"index.html":            {Data: []byte("<html><head></head><body>x</body></html>")},
		"assets/main-abc123.js": {Data: []byte("console.log(1)")},
		"favicon.ico":           {Data: []byte("i")},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/main-abc123.js", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset Cache-Control = %q, want immutable", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("unhashed asset must not be immutable, got %q", cc)
	}
}

// Handler must never panic, whatever is (or is not) embedded — that is the whole
// point of the fallback.
func TestHandlerDoesNotPanic(t *testing.T) {
	for _, dev := range []bool{false, true} {
		rec := httptest.NewRecorder()
		Handler(dev).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("dev=%v status = %d, want 200", dev, rec.Code)
		}
	}
}

// Built() is what the daemon logs on. It must agree with what Handler serves:
// an unbuilt tree reports false and yields the fallback page.
func TestBuiltAgreesWithServedPage(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	servedFallback := strings.Contains(rec.Body.String(), "WebUI not built")

	if Built() == servedFallback {
		t.Fatalf("Built()=%v but servedFallback=%v — the startup log would lie",
			Built(), servedFallback)
	}
}

// The embedded fallback must be a real page, not an empty file someone truncated.
func TestFallbackAssetIsUsable(t *testing.T) {
	if len(fallbackIndex) == 0 {
		t.Fatal("fallback.html is empty — every unbuilt binary would serve a blank page")
	}
	for _, want := range []string{"<!doctype html>", "</head>", "make ui"} {
		if !strings.Contains(string(fallbackIndex), want) {
			t.Fatalf("fallback.html missing %q", want)
		}
	}
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
