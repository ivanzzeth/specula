package gomod

// sumdb_endpoint_test.go — BUG A regression: the verifier and the /sumdb/
// passthrough must agree on the URL convention, for BOTH real-world upstream
// shapes, so that a go client can verify THROUGH Specula (no client-side
// GOSUMDB override).
//
// The doubles here are deliberately STRICT: each models exactly one real host's
// wire shape and 404s everything else. The previous double
// (fakeSumDBUpstream, keyed on responses[r.URL.Path]) answered whatever path the
// handler happened to build, so every URL shape "passed" — that is what let the
// 404-in-CN bug ship green.
//
// Measured against the real hosts (2026-07):
//
//	200  https://sum.golang.google.cn/latest
//	200  https://sum.golang.google.cn/lookup/rsc.io/quote@v1.5.2
//	404  https://sum.golang.google.cn/supported
//	404  https://sum.golang.google.cn/sum.golang.org/lookup/rsc.io/quote@v1.5.2
//	200  https://goproxy.cn/sumdb/sum.golang.org/supported
//	200  https://goproxy.cn/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2
//	404  https://goproxy.cn/sumdb/lookup/rsc.io/quote@v1.5.2

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSumDBName = "sum.golang.org"

// quoteRecord is the real go.sum record served by sum.golang.google.cn for
// rsc.io/quote@v1.5.2 (see the curl output above).
const quoteRecord = "942\n" +
	"rsc.io/quote v1.5.2 h1:w5fcysjrx7yqtD/aO+QwRjYZOKnaM9Uh2b40tElTs3Y=\n" +
	"rsc.io/quote v1.5.2/go.mod h1:LzX7hefJvL54yjefDEDHNONDjII0t9xZLPXsUe+TKr0=\n"

// directSumDBUpstream models sum.golang.google.cn: a checksum database served at
// the ROOT of its host. It serves /latest, /lookup/<mod>@<v> and /tile/...  and
// NOTHING else. In particular there is NO /supported endpoint (that is a module
// proxy endpoint, not a sumdb one) and NO /<name>/ path prefix.
func directSumDBUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/latest":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "go.sum database tree\n1\nhash=\n")
		case strings.HasPrefix(p, "/lookup/"), strings.HasPrefix(p, "/tile/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, quoteRecord)
		default:
			// Real behaviour: /supported and /<name>/... are 404 here.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// proxySumDBUpstream models goproxy.cn/sumdb: a GOPROXY module-proxy sumdb base
// that expects "/<sumdb-name>/<endpoint>" and serves /<name>/supported.
// Its base URL for config purposes is srv.URL + "/sumdb".
func proxySumDBUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/sumdb/"+testSumDBName)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case rest == "/supported":
			w.WriteHeader(http.StatusOK)
		case rest == "/latest":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "go.sum database tree\n1\nhash=\n")
		case strings.HasPrefix(rest, "/lookup/"), strings.HasPrefix(rest, "/tile/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, quoteRecord)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mountPassthrough wires a SumDBHandler behind the real gomod handler, exactly
// as production does, and returns the client-facing base URL.
func mountPassthrough(t *testing.T, upstreamURL string, opts ...SumDBOption) string {
	t.Helper()
	h := NewSumDBHandler(upstreamURL, opts...)
	gh := NewHandler(newGomodTestCache(), WithSumDB(h))
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSumDBPassthrough_DirectUpstream_Lookup is the primary RED test for BUG A.
//
// With the documented CN config (sumdb.url = https://sum.golang.google.cn), the
// go client's request  GET /sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2
// must reach the upstream as  GET /lookup/rsc.io/quote@v1.5.2  — the sumdb NAME
// is a routing token of the module-proxy protocol, not part of the direct
// sumdb's own URL space.
func TestSumDBPassthrough_DirectUpstream_Lookup(t *testing.T) {
	up := directSumDBUpstream(t)
	base := mountPassthrough(t, up.URL)

	resp, err := http.Get(base + "/sumdb/" + testSumDBName + "/lookup/rsc.io/quote@v1.5.2")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"BUG A: with sumdb.url=https://sum.golang.google.cn (a DIRECT sumdb, the documented "+
			"CN value that the verifier needs), the passthrough must strip the sumdb name and "+
			"request /lookup/... upstream. Requesting /<name>/lookup/... 404s on the real host, "+
			"so the go client falls back to sum.golang.org — which is blocked in CN.")
	assert.Equal(t, quoteRecord, string(body))
}

// TestSumDBPassthrough_DirectUpstream_Supported asserts /supported is answered
// by Specula itself. Per the module proxy protocol, GOPROXY/sumdb/<db>/supported
// returns 200 iff THE PROXY will proxy for <db> — it is a statement about the
// proxy, not about the upstream. A direct sumdb has no /supported endpoint, so
// forwarding it yields 404 and the go client disables passthrough entirely.
func TestSumDBPassthrough_DirectUpstream_Supported(t *testing.T) {
	up := directSumDBUpstream(t)
	base := mountPassthrough(t, up.URL)

	resp, err := http.Get(base + "/sumdb/" + testSumDBName + "/supported")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"BUG A: /sumdb/<db>/supported must be answered by Specula (200). A direct sumdb "+
			"(sum.golang.google.cn) 404s /supported; forwarding it makes the go client give up "+
			"on passthrough and go direct to sum.golang.org, which is blocked in CN.")
}

// TestSumDBPassthrough_ProxyUpstream_Lookup asserts the OTHER real shape keeps
// working: a GOPROXY "/sumdb" base needs the name kept in the path.
func TestSumDBPassthrough_ProxyUpstream_Lookup(t *testing.T) {
	up := proxySumDBUpstream(t)
	base := mountPassthrough(t, up.URL+"/sumdb")

	resp, err := http.Get(base + "/sumdb/" + testSumDBName + "/lookup/rsc.io/quote@v1.5.2")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"with sumdb.url=https://goproxy.cn/sumdb (a PROXY base), the passthrough must keep "+
			"the sumdb name in the upstream path")
	assert.Equal(t, quoteRecord, string(body))
}

func TestSumDBPassthrough_ProxyUpstream_Supported(t *testing.T) {
	up := proxySumDBUpstream(t)
	base := mountPassthrough(t, up.URL+"/sumdb")

	resp, err := http.Get(base + "/sumdb/" + testSumDBName + "/supported")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
