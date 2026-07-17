package gomod

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/mod/module"

	"github.com/ivanzzeth/specula/internal/verify"
)

// SumDBHandler is the /sumdb/{name}/... passthrough sub-handler. It transparently
// proxies checksum-database requests to the configured upstream sumdb (a GOPROXY
// "/sumdb/" base or sum.golang.google.cn) so the go command can perform its own
// signed tree-head + inclusion/consistency verification through Specula.
//
// Security seam (DESIGN-REVIEW H5): a lookup for a PRIVATE module (matching a
// GONOSUMDB glob) is rejected with 403 and NEVER forwarded to the public sumdb
// (Athens NoSumPatterns). GOSUMDB is never disabled.
//
// URL convention (BUG A): the sumdb name in "/sumdb/{name}/..." is a routing
// token of the module proxy protocol, NOT necessarily part of the upstream's URL
// space. Resolution is delegated to verify.SumDBEndpoint — the SAME resolver the
// sumdb verifier uses — so a direct sumdb host (sum.golang.google.cn) and a
// GOPROXY "/sumdb" base (goproxy.cn/sumdb) both work, and the two callers cannot
// disagree about the convention.
type SumDBHandler struct {
	endpoint verify.SumDBEndpoint  // upstream URL resolver (direct or proxy shape)
	name     string                // sumdb name this passthrough will serve
	private  verify.PrivateMatcher // GONOSUMDB private-module matcher
	client   *http.Client          // upstream HTTP client (nil = http.DefaultClient)
	log      *slog.Logger
}

// SumDBOption configures a SumDBHandler.
type SumDBOption func(*SumDBHandler)

// WithSumDBPrivateMatcher sets the GONOSUMDB private-module matcher. Lookups for
// matching modules return 403 and are never forwarded upstream.
func WithSumDBPrivateMatcher(m verify.PrivateMatcher) SumDBOption {
	return func(s *SumDBHandler) { s.private = m }
}

// WithSumDBHTTPClient overrides the HTTP client used for upstream passthrough.
func WithSumDBHTTPClient(c *http.Client) SumDBOption {
	return func(s *SumDBHandler) { s.client = c }
}

// WithSumDBLogger injects a structured logger.
func WithSumDBLogger(l *slog.Logger) SumDBOption {
	return func(s *SumDBHandler) { s.log = l }
}

// WithSumDBName sets the checksum-database name this passthrough serves — the
// name in the configured verifier key. Requests for any other database are
// refused rather than misrouted to an upstream that does not host it.
// Defaults to verify.DefaultSumDBName ("sum.golang.org").
func WithSumDBName(name string) SumDBOption {
	return func(s *SumDBHandler) {
		if name = strings.TrimSpace(name); name != "" {
			s.name = name
		}
	}
}

// NewSumDBHandler constructs a sumdb passthrough sub-handler targeting
// upstreamURL (e.g. "https://sum.golang.google.cn" or "https://goproxy.cn/sumdb").
// An unparseable upstreamURL is retained as a direct-style base; upstream
// requests then fail closed with 502 rather than panicking at construction.
func NewSumDBHandler(upstreamURL string, opts ...SumDBOption) *SumDBHandler {
	endpoint, err := verify.ParseSumDBEndpoint(upstreamURL)
	s := &SumDBHandler{
		endpoint: endpoint,
		name:     verify.DefaultSumDBName,
		client:   http.DefaultClient,
		log:      slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if err != nil {
		s.log.Error("sumdb passthrough: bad upstream url — passthrough will fail closed",
			"url", upstreamURL, "err", err)
	}
	return s
}

// Compile-time assertion: exposable as a standalone http.Handler.
var _ http.Handler = (*SumDBHandler)(nil)

// ServeHTTP lets the passthrough be mounted directly (e.g. at "/sumdb/"). It
// derives the sumdb sub-path from the request path and delegates to serve.
func (s *SumDBHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	i := strings.Index(p, "/sumdb")
	if i < 0 {
		writeGoError(w, http.StatusNotFound, "not a sumdb path")
		return
	}
	s.serve(w, r, strings.TrimPrefix(p[i:], "/sumdb"))
}

// serve handles a sumdb request whose path has been reduced to the portion after
// "/sumdb" (e.g. "/sum.golang.org/lookup/github.com/foo@v1.0.0"). The go client
// uses these endpoints:
//
//	/{name}/supported
//	/{name}/latest
//	/{name}/lookup/{module}@{version}
//	/{name}/tile/{H}/{L}/{K}[.p/{W}]
func (s *SumDBHandler) serve(w http.ResponseWriter, r *http.Request, sub string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeGoError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	sub = strings.TrimPrefix(sub, "/")
	if sub == "" {
		writeGoError(w, http.StatusNotFound, "missing sumdb name")
		return
	}

	// Private-module guard: never forward a private module name to the public
	// sumdb. Applies to lookup/{module}@{version} requests.
	if mod, ok := moduleFromLookup(sub); ok {
		if canonical, err := module.UnescapePath(mod); err == nil && s.private.IsPrivate(canonical) {
			writeGoError(w, http.StatusForbidden,
				"private module not served by public sumdb (GONOSUMDB): "+canonical)
			return
		}
	}

	// Split "{name}/{endpoint...}". The name identifies WHICH checksum database
	// the go client wants; it is not necessarily part of the upstream's own URL
	// space (see verify.SumDBEndpoint).
	name, elem, ok := splitSumDBName(sub)
	if !ok {
		writeGoError(w, http.StatusNotFound, "missing sumdb endpoint after name")
		return
	}
	if name != s.name {
		// Refuse rather than misroute to an upstream that does not host this db.
		writeGoError(w, http.StatusNotFound,
			"sumdb "+name+" not served by this proxy (configured: "+s.name+")")
		return
	}

	// "/supported" is a MODULE PROXY endpoint, answered by the proxy about
	// itself: per the module proxy protocol, GOPROXY/sumdb/<db>/supported returns
	// 200 iff this proxy will proxy for <db>. It is not a checksum-database
	// endpoint — a direct sumdb host (sum.golang.google.cn) 404s it — so
	// forwarding it made the go client conclude passthrough was unsupported and
	// go direct to sum.golang.org, which is blocked in CN (BUG A).
	if elem == "/supported" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Streaming reverse-proxy to the upstream sumdb.
	// The go client verifies the signed responses (tree head, inclusion/consistency
	// proofs) itself; we are transparent intermediaries.
	targetURL := s.endpoint.URL(name, elem)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, nil)
	if err != nil {
		s.log.Error("sumdb passthrough: build request", "err", err, "target", targetURL)
		writeGoError(w, http.StatusInternalServerError, "sumdb: proxy error: "+err.Error())
		return
	}
	// Forward Accept header; the go client sometimes sets Accept for tile types.
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	httpc := s.client
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		s.log.Error("sumdb passthrough: upstream error", "err", err, "target", targetURL)
		writeGoError(w, http.StatusBadGateway, "sumdb: upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Copy response headers (Content-Type, Cache-Control, etc.).
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// splitSumDBName splits a sumdb sub-path "{name}/{endpoint...}" into the
// database name and the database-relative endpoint path (with leading slash),
// e.g. "sum.golang.org/lookup/rsc.io/quote@v1.5.2" →
// ("sum.golang.org", "/lookup/rsc.io/quote@v1.5.2", true).
//
// The name is the first path segment: sumdb names are hosts (no slash), and the
// module proxy protocol's endpoints all begin with a fixed element
// (supported / latest / lookup / tile).
func splitSumDBName(sub string) (name, elem string, ok bool) {
	i := strings.IndexByte(sub, '/')
	if i <= 0 || i == len(sub)-1 {
		return "", "", false
	}
	return sub[:i], sub[i:], true
}

// moduleFromLookup extracts the escaped module path from a sumdb sub-path of the
// form "{name}/lookup/{module}@{version}". Returns ("", false) for non-lookup
// endpoints (supported / latest / tile).
func moduleFromLookup(sub string) (escapedModule string, ok bool) {
	i := strings.Index(sub, "/lookup/")
	if i < 0 {
		return "", false
	}
	rest := sub[i+len("/lookup/"):]
	// Strip the "@version" suffix; module paths never contain '@'.
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "", false
	}
	mod := rest[:at]
	if mod == "" {
		return "", false
	}
	return mod, true
}
