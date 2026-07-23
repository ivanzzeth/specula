package cargo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestUpstreamForPath(t *testing.T) {
	h := NewHandler(newCargoTestCache(),
		WithRegistries(RegistriesFromSpecs([]RegistrySpec{
			{Name: "crates-io", BaseURL: "https://example.com/index"},
			{Name: "rsproxy", BaseURL: "https://example.com/rsproxy"},
		})),
	)

	ups, fetch, ok := h.upstreamForPath("crates-io/config.json")
	require.True(t, ok)
	require.Len(t, ups, 1)
	assert.Equal(t, "https://example.com/index", ups[0].BaseURL)
	assert.Equal(t, "config.json", fetch)

	ups, fetch, ok = h.upstreamForPath("rsproxy/se/rd/serde")
	require.True(t, ok)
	require.Len(t, ups, 1)
	assert.Equal(t, "https://example.com/rsproxy", ups[0].BaseURL)
	assert.Equal(t, "se/rd/serde", fetch)

	_, _, ok = h.upstreamForPath("evil-registry/config.json")
	assert.False(t, ok)
}

func TestRegistryStripOnFetch(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"dl":"https://static.crates.io","api":"https://crates.io"}`)
	}))
	t.Cleanup(up.Close)

	h := NewHandler(newCargoTestCache(),
		WithPathPrefix("/cargo"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "idx", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRegistries(RegistriesFromSpecs([]RegistrySpec{
			{Name: "crates-io", BaseURL: up.URL},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cargo/index/crates-io/config.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "path hit: %s", gotPath)
	assert.Equal(t, "/config.json", gotPath)
}

func TestUnknownRegistryRejected(t *testing.T) {
	h := NewHandler(newCargoTestCache(),
		WithPathPrefix("/cargo"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "idx", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRegistries(RegistriesFromSpecs([]RegistrySpec{
			{Name: "crates-io", BaseURL: "http://127.0.0.1:1"},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cargo/index/evil-registry/config.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
