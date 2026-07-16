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
// This is the contract skeleton: the private-module 403 guard is implemented;
// the actual upstream proxying returns 501 until the verifier/passthrough agent
// wires it (streaming reverse-proxy to upstreamURL).
type SumDBHandler struct {
	upstreamURL string                // sumdb passthrough base (no trailing slash)
	private     verify.PrivateMatcher // GONOSUMDB private-module matcher
	client      *http.Client          // upstream HTTP client (nil = http.DefaultClient)
	log         *slog.Logger
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

// NewSumDBHandler constructs a sumdb passthrough sub-handler targeting
// upstreamURL (e.g. "https://sum.golang.google.cn" or "https://goproxy.cn/sumdb").
func NewSumDBHandler(upstreamURL string, opts ...SumDBOption) *SumDBHandler {
	s := &SumDBHandler{
		upstreamURL: strings.TrimRight(upstreamURL, "/"),
		client:      http.DefaultClient,
		log:         slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
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

	// Streaming reverse-proxy to the upstream sumdb.
	// The go client verifies the signed responses (tree head, inclusion/consistency
	// proofs) itself; we are transparent intermediaries.
	targetURL := s.upstreamURL + "/" + sub

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
