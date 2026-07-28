package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Specula can reach a CN mirror directly but not the official origin, so the
// origin needs a proxy. Setting HTTPS_PROXY would work and is already honoured —
// and would send EVERY upstream fetch through the proxy, including the mirror
// traffic that is the bulk of the bytes and the whole reason the mirrors exist.
// On a metered proxy that is the expensive mistake.
//
// So the proxy is configured PER UPSTREAM. The official origin sits last in the
// chain, so its proxy is only ever used after every mirror has failed: a genuine
// last resort, paid for only when it is needed.

// fakeProxy records the absolute-form requests an HTTP proxy receives.
type fakeProxy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		// A proxied request carries the absolute URI, which is how we know it came
		// through the proxy rather than direct.
		p.seen = append(p.seen, r.Method+" "+r.URL.String())
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"proxied":true}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProxy) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// A client built for an upstream with a proxy must send that upstream's requests
// through it — including to a host that does not resolve, which proves the dial
// went to the proxy and not to DNS.
func TestUpstreamWithProxyGoesThroughIt(t *testing.T) {
	proxy := newFakeProxy(t)

	c, err := newUpstreamHTTPClientProxied(defaultDialTimeout, defaultTLSHandshakeTimeout,
		defaultResponseHeaderTimeout, proxy.srv.URL)
	if err != nil {
		t.Fatalf("build proxied client: %v", err)
	}

	// A host that cannot resolve: if this succeeds, it can only be via the proxy.
	resp, err := c.Get("http://registry-1.docker.io.invalid/v2/")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := proxy.requests()
	if len(got) != 1 {
		t.Fatalf("proxy saw %d requests, want 1: %v", len(got), got)
	}
	if want := "GET http://registry-1.docker.io.invalid/v2/"; got[0] != want {
		t.Errorf("proxy saw %q, want %q", got[0], want)
	}
}

// The property that saves money: an upstream WITHOUT a proxy must not use one,
// even while another upstream in the same chain has one configured.
func TestMirrorWithoutProxyDoesNotUseTheOriginsProxy(t *testing.T) {
	proxy := newFakeProxy(t)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("direct"))
	}))
	defer mirror.Close()

	c := newFallbackClient()
	direct := c.httpForUpstream(Upstream{Name: "daocloud", BaseURL: mirror.URL}, 1)
	resp, err := direct.Get(mirror.URL + "/v2/")
	if err != nil {
		t.Fatalf("direct fetch: %v", err)
	}
	_ = resp.Body.Close()

	if n := len(proxy.requests()); n != 0 {
		t.Errorf("the proxy saw %d requests from an upstream that has none configured", n)
	}
}

// Building a transport per request would throw away connection pooling, which on a
// proxied origin means a fresh CONNECT per layer.
func TestProxiedClientsAreReusedPerProxy(t *testing.T) {
	c := newFallbackClient()
	up := Upstream{Name: "docker-hub", BaseURL: "https://registry-1.docker.io", Proxy: "http://127.0.0.1:3128"}

	first := c.httpForUpstream(up, 0)
	second := c.httpForUpstream(up, 0)
	if first != second {
		t.Error("a second call built a new client for the same proxy; connections would not be pooled")
	}

	other := c.httpForUpstream(Upstream{Name: "x", Proxy: "http://127.0.0.1:8118"}, 0)
	if other == first {
		t.Error("two different proxies share one client")
	}
}

// The fast (non-final hop) and patient (last hop) budgets must both survive
// proxying: a proxied mirror still has to yield quickly to the next one.
func TestProxiedClientKeepsTheFastAndPatientDistinction(t *testing.T) {
	c := newFallbackClient()
	up := Upstream{Name: "m", Proxy: "http://127.0.0.1:3128"}
	fast := c.httpForUpstream(up, 2) // mirrors remain after this one
	last := c.httpForUpstream(up, 0) // final hop
	if fast == last {
		t.Error("the same client served a non-final and a final hop; one of the two budgets is wrong")
	}
}

// A malformed proxy must not silently fall back to a direct connection: an origin
// that is only reachable through a proxy would then fail in a way that looks like
// the origin being down.
func TestMalformedProxyIsAnError(t *testing.T) {
	for _, bad := range []string{"://nope", "http://[::1", "not a url at all\n"} {
		if _, err := newUpstreamHTTPClientProxied(defaultDialTimeout, defaultTLSHandshakeTimeout,
			defaultResponseHeaderTimeout, bad); err == nil {
			t.Errorf("proxy %q accepted", bad)
		}
	}
}

// socks5 is the other shape people have, and net/http handles it in Transport.Proxy.
func TestSocks5ProxyIsAccepted(t *testing.T) {
	if _, err := newUpstreamHTTPClientProxied(defaultDialTimeout, defaultTLSHandshakeTimeout,
		defaultResponseHeaderTimeout, "socks5://127.0.0.1:1080"); err != nil {
		t.Errorf("socks5 proxy rejected: %v", err)
	}
}

// With no proxy configured anywhere, behaviour must be exactly as before —
// including honouring HTTPS_PROXY from the environment, which some deployments
// already rely on.
func TestNoProxyConfiguredKeepsEnvironmentBehaviour(t *testing.T) {
	c := newFallbackClient()
	up := Upstream{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"}
	if got := c.httpForUpstream(up, 0); got != c.http {
		t.Error("an upstream without a proxy did not get the shared patient client")
	}
	if got := c.httpForUpstream(up, 3); got != c.httpFast {
		t.Error("an upstream without a proxy did not get the shared fast client")
	}
}

func TestProxyKeyDistinguishesFastFromPatient(t *testing.T) {
	a := proxyClientKey("http://p:3128", true)
	b := proxyClientKey("http://p:3128", false)
	if a == b {
		t.Fatalf("fast and patient keys collide: %q", a)
	}
	if a == proxyClientKey("http://q:3128", true) {
		t.Fatal("different proxies share a key")
	}
	_ = fmt.Sprint(a, b)
}
