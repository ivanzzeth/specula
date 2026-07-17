package verify

// sumdb_endpoint_test.go — BUG A regression, verifier side.
//
// The verifier builds  baseURL + path  (path = "/lookup/..." | "/tile/...").
// That is correct for a DIRECT sumdb host (sum.golang.google.cn) but wrong for a
// GOPROXY "/sumdb" base (goproxy.cn/sumdb), which routes on "/<sumdb-name>/...".
// Both shapes are documented values of the single `sumdb.url` config key, so
// both must work.
//
// Measured against the real hosts (2026-07):
//
//	404  https://goproxy.cn/sumdb/lookup/rsc.io/quote@v1.5.2         <- what the verifier builds
//	200  https://goproxy.cn/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	xsumdb "golang.org/x/mod/sumdb"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// proxyBase re-serves the test sumdb behind a GOPROXY-style "/sumdb/<name>/"
// prefix — the goproxy.cn shape — and returns the base URL to configure
// (i.e. "<host>/sumdb"). Anything not under "/sumdb/<name>/" is 404, as on the
// real host.
func (db *testSumDB) proxyBase(t *testing.T) string {
	t.Helper()
	inner := xsumdb.NewServer(db.srv)
	prefix := "/sumdb/" + db.name
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		inner.ServeHTTP(w, r2)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/sumdb"
}

// TestSumDBVerifier_ProxyStyleBaseURL is the verifier-side RED test for BUG A:
// with sumdb.url set to a GOPROXY "/sumdb" base, verification must succeed.
func TestSumDBVerifier_ProxyStyleBaseURL(t *testing.T) {
	db := newTestSumDB(t)
	const modPath, version = "example.com/proxyshape", "v1.0.0"
	goMod := "module example.com/proxyshape\n"
	modFile, modH1 := writeGoMod(t, goMod)
	db.registerMod(modPath, version, "", modH1)

	cfg := db.makeCfg(newMemTreeSizeStore(), PolicyEnforce)
	cfg.URL = db.proxyBase(t) // "<host>/sumdb" — the goproxy.cn shape

	v := NewSumDBVerifier(cfg)
	res, err := v.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: protocolGo, Name: modPath, Version: version + ".mod"},
		&artifact.Artifact{Path: modFile})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusPass, res.Status,
		"BUG A: sumdb.url=https://goproxy.cn/sumdb is a documented CN value, but the verifier "+
			"builds baseURL+\"/lookup/...\" which 404s on a proxy base (the name segment is "+
			"missing) — with policy:enforce that hard-fails every module. Got: "+res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// TestSumDBVerifier_DirectStyleBaseURL_StillWorks pins the other shape
// (sum.golang.google.cn: sumdb served at the host root).
func TestSumDBVerifier_DirectStyleBaseURL_StillWorks(t *testing.T) {
	db := newTestSumDB(t)
	const modPath, version = "example.com/directshape", "v1.0.0"
	goMod := "module example.com/directshape\n"
	modFile, modH1 := writeGoMod(t, goMod)
	db.registerMod(modPath, version, "", modH1)

	cfg := db.makeCfg(newMemTreeSizeStore(), PolicyEnforce) // URL = httpSrv root

	v := NewSumDBVerifier(cfg)
	res, err := v.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: protocolGo, Name: modPath, Version: version + ".mod"},
		&artifact.Artifact{Path: modFile})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)
}
