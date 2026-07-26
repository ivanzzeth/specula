package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestParseRemoteName(t *testing.T) {
	regs := RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
		{Host: "codeberg.org"},
		{Host: "ghcr.io", BaseURL: "https://ghcr.io"},
	})

	host, repo, ok := parseRemoteName("codeberg.org/forgejo/forgejo", regs)
	require.True(t, ok)
	assert.Equal(t, "codeberg.org", host)
	assert.Equal(t, "forgejo/forgejo", repo)

	_, _, ok = parseRemoteName("library/nginx", regs)
	assert.False(t, ok)

	_, _, ok = parseRemoteName("evil.example/foo/bar", regs)
	assert.False(t, ok)

	_, _, ok = parseRemoteName("GHCR.IO/org/img", regs)
	assert.True(t, ok)
}

func TestLooksLikeRegistryHost(t *testing.T) {
	assert.True(t, looksLikeRegistryHost("codeberg.org/forgejo/forgejo"))
	assert.False(t, looksLikeRegistryHost("library/nginx"))
	assert.False(t, looksLikeRegistryHost("nginx"))
}

func TestRemotePullStripsHostFromUpstreamPath(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/manifests/latest") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
			_, _ = io.WriteString(w, `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:`+strings.Repeat("b", 64)+`","size":1},"layers":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)

	cm := newStoringFakeCache()
	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hub", BaseURL: "http://127.0.0.1:1", Priority: 1}, // must not be used
		}),
		WithRemoteRegistries(RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
			{Host: "codeberg.org", BaseURL: up.URL},
		})),
		WithQuarantineDir(t.TempDir()),
	)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/codeberg.org/forgejo/forgejo/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body path hit: %s", gotPath)
	assert.Equal(t, "/v2/forgejo/forgejo/manifests/latest", gotPath,
		"upstream path must strip registry host")
}

func TestRemoteRegistriesFromSpecsAppendsOriginFallback(t *testing.T) {
	regs := RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
		{Host: "ghcr.io", BaseURL: "https://ghcr.m.daocloud.io"},
		{Host: "codeberg.org"}, // no mirror → origin only
	})
	require.Len(t, regs["ghcr.io"], 2)
	assert.Equal(t, "https://ghcr.m.daocloud.io", regs["ghcr.io"][0].BaseURL)
	assert.False(t, regs["ghcr.io"][0].Official)
	assert.Equal(t, "https://ghcr.io", regs["ghcr.io"][1].BaseURL)
	assert.True(t, regs["ghcr.io"][1].Official)
	require.Len(t, regs["codeberg.org"], 1)
	assert.Equal(t, "https://codeberg.org", regs["codeberg.org"][0].BaseURL)
	assert.True(t, regs["codeberg.org"][0].Official)
}

func TestRemoteRegistriesFromSpecsMultiMirrorChain(t *testing.T) {
	// CN default: DaoCloud → 1ms → origin. Same class of fix as Hub's multi-upstream
	// chain — one allowlist denial must not wedge niche images.
	regs := RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
		{
			Host: "ghcr.io",
			Upstreams: []RemoteUpstreamSpec{
				{Name: "daocloud", BaseURL: "https://ghcr.m.daocloud.io", Priority: 1},
				{Name: "1ms", BaseURL: "https://ghcr.1ms.run", Priority: 2},
			},
		},
	})
	require.Len(t, regs["ghcr.io"], 3)
	assert.Equal(t, "https://ghcr.m.daocloud.io", regs["ghcr.io"][0].BaseURL)
	assert.Equal(t, "https://ghcr.1ms.run", regs["ghcr.io"][1].BaseURL)
	assert.Equal(t, "https://ghcr.io", regs["ghcr.io"][2].BaseURL)
	assert.True(t, regs["ghcr.io"][2].Official)
	assert.False(t, regs["ghcr.io"][0].Official)
}

func TestRemoteRegistriesFromSpecsBaseURLPlusUpstreams(t *testing.T) {
	regs := RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
		{
			Host:    "quay.io",
			BaseURL: "https://quay.m.daocloud.io",
			Upstreams: []RemoteUpstreamSpec{
				{Name: "1ms", BaseURL: "https://quay.1ms.run", Priority: 1},
			},
		},
	})
	require.Len(t, regs["quay.io"], 3)
	assert.Equal(t, "https://quay.m.daocloud.io", regs["quay.io"][0].BaseURL)
	assert.Equal(t, "https://quay.1ms.run", regs["quay.io"][1].BaseURL)
	assert.Equal(t, "https://quay.io", regs["quay.io"][2].BaseURL)
}

func TestRemotePullFallsBackAcrossMultiMirrorChain(t *testing.T) {
	var hits1, hits2, hitsOrigin atomic.Int32
	m1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits1.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errors":[{"code":"DENIED"}]}`)
	}))
	t.Cleanup(m1.Close)
	m2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits2.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errors":[{"code":"DENIED"}]}`)
	}))
	t.Cleanup(m2.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsOrigin.Add(1)
		if strings.HasSuffix(r.URL.Path, "/manifests/v1") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
			_, _ = io.WriteString(w, `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:`+strings.Repeat("b", 64)+`","size":1},"layers":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(origin.Close)

	regs := RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
		{
			Host: "ghcr.io",
			Upstreams: []RemoteUpstreamSpec{
				{Name: "daocloud", BaseURL: m1.URL, Priority: 1},
				{Name: "1ms", BaseURL: m2.URL, Priority: 2},
			},
		},
	})
	// Replace auto-appended https://ghcr.io with the test origin server.
	chain := regs["ghcr.io"]
	require.Len(t, chain, 3)
	chain[2].BaseURL = origin.URL

	h := NewHandler(newStoringFakeCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hub", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRemoteRegistries(RemoteRegistryMap{"ghcr.io": chain}),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/ghcr.io/org/img/manifests/v1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, hits1.Load(), int32(1))
	assert.GreaterOrEqual(t, hits2.Load(), int32(1))
	assert.GreaterOrEqual(t, hitsOrigin.Load(), int32(1))
}

func TestRemotePullFallsBackToOriginWhenMirror403(t *testing.T) {
	// DaoCloud-style allowlist denial on the CN mirror must NOT wedge the pull —
	// Specula must fall through to the official registry (same as Hub's
	// daocloud→docker-hub chain). Live incident: crazygit/cert-manager-alidns-webhook
	// is absent from DaoCloud allowlist → 403 DENIED on ghcr.m.daocloud.io.
	var mirrorHits, originHits atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errors":[{"code":"DENIED","message":"not in the allowlist"}]}`)
	}))
	t.Cleanup(mirror.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/manifests/0.1.5") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
			_, _ = io.WriteString(w, `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"sha256:`+strings.Repeat("b", 64)+`","size":1},"layers":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(origin.Close)

	cm := newStoringFakeCache()
	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hub", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRemoteRegistries(RemoteRegistryMap{
			"ghcr.io": {
				{Name: "remote:ghcr.io:mirror", BaseURL: mirror.URL, Priority: 1},
				{Name: "remote:ghcr.io", BaseURL: origin.URL, Priority: 2, Official: true},
			},
		}),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/ghcr.io/crazygit/cert-manager-alidns-webhook/manifests/0.1.5")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"CN mirror 403 must fall through to official origin")
	assert.GreaterOrEqual(t, mirrorHits.Load(), int32(1), "mirror must be tried first")
	assert.GreaterOrEqual(t, originHits.Load(), int32(1), "origin must serve after mirror denial")
}

func TestUnknownRemoteHostRejected(t *testing.T) {
	cm := newStoringFakeCache()
	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hub", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRemoteRegistries(RemoteRegistriesFromSpecs([]RemoteRegistrySpec{
			{Host: "codeberg.org"},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/evil.example/foo/bar/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOfflineMissReturns404(t *testing.T) {
	upCalls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upCalls++
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)

	h := NewHandler(newStoringFakeCache(),
		WithUpstream(upstream.NewOfflineClient(), []upstream.Upstream{
			{Name: "hub", BaseURL: up.URL, Priority: 1},
		}),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/library/nginx/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, 0, upCalls, "offline must not contact upstream")
}
