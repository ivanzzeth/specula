package helm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestUpstreamForRepo(t *testing.T) {
	h := NewHandler(newHelmTestCache(),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "bitnami", BaseURL: "https://charts.bitnami.com/bitnami"},
			{Name: "prometheus-community", BaseURL: "https://prometheus-community.github.io/helm-charts"},
		})),
	)

	ups, fetch, ok := h.upstreamForRepo("bitnami")
	require.True(t, ok)
	require.Len(t, ups, 1)
	assert.Equal(t, "https://charts.bitnami.com/bitnami", ups[0].BaseURL)
	assert.Equal(t, "", fetch)

	_, _, ok = h.upstreamForRepo("evil")
	assert.False(t, ok)
}

func TestNamedRepoStripsPathOnFetch(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "apiVersion: v1\nentries: {}\n")
	}))
	t.Cleanup(up.Close)

	h := NewHandler(newHelmTestCache(),
		WithPathPrefix("/helm"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "default", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "bitnami", BaseURL: up.URL},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/helm/bitnami/index.yaml")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "path hit: %s", gotPath)
	assert.Equal(t, "/index.yaml", gotPath)
}

func TestUnknownHelmRepoRejected(t *testing.T) {
	h := NewHandler(newHelmTestCache(),
		WithPathPrefix("/helm"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "default", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "bitnami", BaseURL: "http://127.0.0.1:1"},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/helm/evil/index.yaml")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
