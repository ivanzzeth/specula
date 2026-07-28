package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Per-upstream proxying.
//
// The transport template already sets http.ProxyFromEnvironment, so HTTPS_PROXY
// has always worked — for every upstream at once. That is the problem this solves:
// the mirrors are where the bytes are, and routing them through a metered proxy
// pays for exactly the traffic the mirrors exist to avoid. A proxy configured on
// one upstream applies to that upstream alone, and the natural place for it is the
// official origin, which the chain only reaches after every mirror has failed.

// newUpstreamHTTPClientProxied builds a client whose transport sends everything
// through proxyURL. A malformed proxy is an error rather than a silent fallback to
// a direct dial: an origin only reachable through a proxy would otherwise fail as
// if the origin itself were down.
func newUpstreamHTTPClientProxied(dial, tlsHS, headerTimeout time.Duration, proxyURL string) (*http.Client, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("upstream: parse proxy %q: %w", proxyURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream: proxy %q needs a scheme and host "+
			"(http://host:port, https://host:port or socks5://host:port)", proxyURL)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("upstream: proxy %q: unsupported scheme %q "+
			"(want http, https, socks5)", proxyURL, u.Scheme)
	}

	c := newUpstreamHTTPClientWith(dial, tlsHS, headerTimeout)
	// The client's Transport is the User-Agent wrapper around the real transport,
	// so reach through it rather than replacing it — CN mirrors 403 Go's default UA.
	if ua, ok := c.Transport.(*userAgentRoundTripper); ok {
		if base, ok := ua.base.(*http.Transport); ok {
			base.Proxy = http.ProxyURL(u)
			return c, nil
		}
	}
	return nil, fmt.Errorf("upstream: internal: unexpected transport shape for proxied client")
}

// proxyClientKey identifies a cached proxied client. Fast and patient budgets are
// different clients, so the key carries which one it is.
func proxyClientKey(proxyURL string, fast bool) string {
	if fast {
		return "fast|" + proxyURL
	}
	return "patient|" + proxyURL
}

// httpForUpstream returns the client to fetch up with: the shared fast/patient
// client when up has no proxy, or a cached per-proxy client when it does.
//
// remainingAfter mirrors httpFor: >0 means mirrors remain after this hop, which
// gets the short dial/TLS/header budgets so a dead upstream yields in seconds.
func (c *fallbackClient) httpForUpstream(up Upstream, remainingAfter int) *http.Client {
	if up.Proxy == "" {
		return c.httpFor(remainingAfter)
	}
	fast := remainingAfter > 0 && c.httpFast != nil
	key := proxyClientKey(up.Proxy, fast)

	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxyClients == nil {
		c.proxyClients = make(map[string]*http.Client)
	}
	if hc, ok := c.proxyClients[key]; ok {
		return hc
	}

	var (
		hc  *http.Client
		err error
	)
	if fast {
		hc, err = newUpstreamHTTPClientProxied(fastDialTimeout, fastTLSHandshakeTimeout,
			fastResponseHeaderTimeout, up.Proxy)
	} else {
		hc, err = newUpstreamHTTPClientProxied(defaultDialTimeout, defaultTLSHandshakeTimeout,
			defaultResponseHeaderTimeout, up.Proxy)
	}
	if err != nil {
		// Validation rejects a malformed proxy at config load, so reaching here means
		// a value that got past it. Fetching direct would quietly undo the operator's
		// intent, so fail this upstream instead and let the chain move on.
		c.proxyClients[key] = brokenProxyClient(up.Proxy, err)
		return c.proxyClients[key]
	}
	c.proxyClients[key] = hc
	return hc
}

// brokenProxyClient fails every request with the proxy error, so the chain records
// this upstream as failed rather than silently bypassing the proxy.
func brokenProxyClient(proxyURL string, cause error) *http.Client {
	return &http.Client{Transport: failingRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("upstream: proxy %q is unusable: %w", proxyURL, cause)
	})}
}

// failingRoundTripper turns every request into the same error.
type failingRoundTripper func(*http.Request) (*http.Response, error)

func (f failingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
