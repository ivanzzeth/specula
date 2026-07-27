package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webui "github.com/ivanzzeth/specula/web"
)

// Everything now shares one port, which puts the WebUI's "/" catch-all in the same
// mux as /v2/. That combination has a specific, nasty failure mode, already seen
// once on the old control-plane port: the SPA answers GET /v2/ with 200 and an HTML
// body, `docker login` reads 200-without-a-challenge as "registry reachable, no auth
// needed", and prints **Login Succeeded** for an entirely bogus password. Only a
// later push fails, somewhere else, confusingly.
//
// These tests pin the routing contract of the merged mux directly, without booting
// a daemon: /v2/ must reach a registry-shaped handler, and the SPA must only get
// what nothing else claimed.

// registryStub stands in for the OCI handler: a real one answers /v2/ with 401 and a
// WWW-Authenticate challenge when auth is on.
func registryStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://example/token",service="specula"`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED"}]}`))
	})
}

// mergedMux mirrors the registration order used by run(): protocols first, probes and
// API next, SPA last.
func mergedMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/v2/", registryStub())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/v1/traffic", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"protocols": []any{}})
	})
	mux.Handle("/", webui.Handler(false))
	return mux
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The regression that matters: /v2/ must NOT be served by the SPA.
func TestSinglePortV2IsNotTheSPA(t *testing.T) {
	rec := get(t, mergedMux(), "/v2/")

	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /v2/ returned HTML (Content-Type %q) — the SPA swallowed the registry "+
			"endpoint, which makes `docker login` succeed with any password", ct)
	}
	if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "<!doctype html") {
		t.Fatalf("GET /v2/ returned an HTML document:\n%s", body)
	}
}

// A 200 on /v2/ is what `docker login` reads as "no auth required". Only a
// challenge-carrying 401 (or a real 200 from an authenticated registry) is acceptable
// — never a 200 produced by the SPA fallback.
func TestSinglePortV2AnswersAChallengeNotABlank200(t *testing.T) {
	rec := get(t, mergedMux(), "/v2/")

	if rec.Code == http.StatusOK && rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("GET /v2/ answered 200 with no WWW-Authenticate — `docker login` treats that " +
			"as an open registry and prints Login Succeeded for a bogus password")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stub registry should answer 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("401 must carry a Bearer challenge, got %q", rec.Header().Get("WWW-Authenticate"))
	}
}

// Probes, the Admin API and the WebUI all answer on the same mux — that is the point
// of the change.
func TestSinglePortServesEverythingOnOneMux(t *testing.T) {
	mux := mergedMux()

	if rec := get(t, mux, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	rec := get(t, mux, "/api/v1/traffic")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/traffic = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("/api/v1/traffic Content-Type = %q, want JSON", ct)
	}
	// The SPA gets what nothing else claimed, including deep links.
	for _, p := range []string{"/", "/cache/oci"} {
		rec := get(t, mux, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want the SPA", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want HTML", p, ct)
		}
	}
}

// Registering a pattern twice panics, which is exactly what merging two muxes that
// both mounted /healthz and /token would do. Guard the shape.
func TestSinglePortNoDuplicateRegistration(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("duplicate registration panicked: %v", r)
		}
	}()
	mux := mergedMux()
	mux.Handle("/token", http.NotFoundHandler()) // must be mounted exactly once
	_ = get(t, mux, "/token")
}
