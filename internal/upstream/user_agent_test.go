package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamClientSetsSpeculaUserAgent(t *testing.T) {
	// Regression: tuna (and some other CN mirrors) return HTTP 403 to Go's
	// default "Go-http-client/1.1" User-Agent while accepting curl/wget —
	// Specula apt pool fetches then 502 forever with "upstream … HTTP 403".
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	c := &fallbackClient{http: newUpstreamHTTPClient()}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/pool/x.deb", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotUA != DefaultUserAgent {
		t.Fatalf("User-Agent=%q want %q", gotUA, DefaultUserAgent)
	}
}

func TestWrapUserAgent_SetsWhenMissing(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := &http.Client{Transport: WrapUserAgent(nil)}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotUA != DefaultUserAgent {
		t.Fatalf("User-Agent=%q want %q", gotUA, DefaultUserAgent)
	}
}
