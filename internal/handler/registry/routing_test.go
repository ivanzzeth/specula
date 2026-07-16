package registry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/handler/registry"
)

// ── upload-session error paths ────────────────────────────────────────────────

// TestUnknownUploadSessionReturns404 verifies every operation on an upload UUID
// that does not exist reports BLOB_UPLOAD_UNKNOWN.
func TestUnknownUploadSessionReturns404(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"patch", http.MethodPatch, "/v2/org1/myrepo/blobs/uploads/deadbeef"},
		{"status", http.MethodGet, "/v2/org1/myrepo/blobs/uploads/deadbeef"},
		{"complete", http.MethodPut, "/v2/org1/myrepo/blobs/uploads/deadbeef?digest=sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, strings.NewReader("")))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if code := parseDistError(rec.Body.Bytes()).Errors[0].Code; code != "BLOB_UPLOAD_UNKNOWN" {
				t.Errorf("error code = %q, want BLOB_UPLOAD_UNKNOWN", code)
			}
		})
	}
}

// TestCrossRepoSessionHijackRejected verifies a session opened under one repo
// cannot be driven from another repo's path — the UUID alone must not be a
// capability that escapes its repo.
func TestCrossRepoSessionHijackRejected(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch,
		"/v2/org1/otherrepo/blobs/uploads/"+uuid, strings.NewReader("data")))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a session must not be reachable from another repo", rec.Code)
	}
}

// TestCompleteUploadRequiresDigest verifies finalising without ?digest= is a
// 400: the registry cannot verify bytes it was never told the digest of.
func TestCompleteUploadRequiresDigest(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/blobs/uploads/"+uuid, strings.NewReader("data")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := parseDistError(rec.Body.Bytes()).Errors[0].Code; code != "DIGEST_INVALID" {
		t.Errorf("error code = %q, want DIGEST_INVALID", code)
	}
}

// ── routing ───────────────────────────────────────────────────────────────────

// TestMethodNotAllowed verifies unsupported methods on each endpoint report 405
// rather than being silently misrouted.
func TestMethodNotAllowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"uploads", http.MethodDelete, "/v2/org1/myrepo/blobs/uploads/abc"},
		{"tags list", http.MethodPost, "/v2/org1/myrepo/tags/list"},
		{"referrers", http.MethodPost, "/v2/org1/myrepo/referrers/sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
		})
	}
}

// TestReadRequestsFallThrough verifies GET/HEAD of blobs and manifests, the /v2/
// version probe, and unrecognised paths all defer to the configured read handler
// (the pull path), rather than being answered by the write handler.
func TestReadRequestsFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"version probe", http.MethodGet, "/v2/"},
		{"manifest get", http.MethodGet, "/v2/org1/myrepo/manifests/v1"},
		{"manifest head", http.MethodHead, "/v2/org1/myrepo/manifests/v1"},
		{"blob get", http.MethodGet, "/v2/org1/myrepo/blobs/sha256:" + strings.Repeat("a", 64)},
		{"blob head", http.MethodHead, "/v2/org1/myrepo/blobs/sha256:" + strings.Repeat("a", 64)},
		{"non-v2 path", http.MethodGet, "/healthz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			read := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusTeapot)
			})
			h := registry.NewHandler(
				&nilCacheManager{},
				newMemRepoStore(),
				newMemTagStore(),
				&allowAuthz{r: testRepo("org1", "org1/myrepo")},
				registry.WithBlobStore(newMemBlobStore()),
				registry.WithReadHandler(read),
			)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if !reached {
				t.Fatalf("read handler not reached for %s %s", tc.method, tc.target)
			}
			if rec.Code != http.StatusTeapot {
				t.Errorf("status = %d, want the read handler's response", rec.Code)
			}
		})
	}
}

// TestReadFallthroughWithoutReadHandler404s verifies that with no read handler
// wired, a read is a clean NAME_UNKNOWN rather than a panic.
func TestReadFallthroughWithoutReadHandler404s(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/manifests/v1", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestEmptyRepoNameRejected verifies a path with no repository name is a 400
// rather than being routed with an empty name.
func TestEmptyRepoNameRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"uploads", http.MethodPost, "/v2//blobs/uploads/"},
		{"manifests", http.MethodPut, "/v2//manifests/v1"},
		{"tags list", http.MethodGet, "/v2//tags/list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestEmptyManifestBodyRejected verifies an empty manifest push is rejected
// rather than stored as a zero-byte "manifest".
func TestEmptyManifestBodyRejected(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/manifests/v1", strings.NewReader("")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestOversizeManifestRejected verifies the manifest body cap is enforced, so an
// authenticated client cannot force an unbounded read.
func TestOversizeManifestRejected(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	huge := strings.NewReader("{" + strings.Repeat("x", 5<<20) + "}")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v2/org1/myrepo/manifests/v1", huge))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a manifest over the 4 MiB cap", rec.Code)
	}
}
