package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
