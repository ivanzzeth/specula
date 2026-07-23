package conda

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
	h := NewHandler(newCondaTestCache(),
		WithChannels(ChannelsFromSpecs([]ChannelSpec{
			{Name: "conda-forge", BaseURL: "https://example.com/conda-forge"},
			{Name: "bioconda", BaseURL: "https://example.com/bioconda"},
		})),
	)

	ups, fetch, ok := h.upstreamForPath("conda-forge/linux-64/repodata.json")
	require.True(t, ok)
	require.Len(t, ups, 1)
	assert.Equal(t, "https://example.com/conda-forge", ups[0].BaseURL)
	assert.Equal(t, "linux-64/repodata.json", fetch)

	_, _, ok = h.upstreamForPath("evil-channel/linux-64/repodata.json")
	assert.False(t, ok)
}

func TestChannelStripOnFetch(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"packages":{}}`)
	}))
	t.Cleanup(up.Close)

	h := NewHandler(newCondaTestCache(),
		WithPathPrefix("/conda"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "cloud", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithChannels(ChannelsFromSpecs([]ChannelSpec{
			{Name: "conda-forge", BaseURL: up.URL},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/conda/conda-forge/linux-64/repodata.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "path hit: %s", gotPath)
	assert.Equal(t, "/linux-64/repodata.json", gotPath)
}

func TestUnknownChannelRejected(t *testing.T) {
	h := NewHandler(newCondaTestCache(),
		WithPathPrefix("/conda"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "cloud", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithChannels(ChannelsFromSpecs([]ChannelSpec{
			{Name: "conda-forge", BaseURL: "http://127.0.0.1:1"},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/conda/evil/linux-64/repodata.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
