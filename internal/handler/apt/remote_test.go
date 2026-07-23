package apt

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

func TestSelectUpstreams(t *testing.T) {
	h := NewHandler(newAptTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "default", BaseURL: "https://archive.ubuntu.com/ubuntu", Priority: 1},
		}),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "ubuntu", BaseURL: "https://mirrors.example/ubuntu"},
			{Name: "debian", BaseURL: "https://mirrors.example/debian"},
		})),
	)

	ups, ok := h.selectUpstreams("ubuntu")
	require.True(t, ok)
	require.Len(t, ups, 1)
	assert.Equal(t, "https://mirrors.example/ubuntu", ups[0].BaseURL)

	ups, ok = h.selectUpstreams("")
	require.True(t, ok)
	assert.Equal(t, "https://archive.ubuntu.com/ubuntu", ups[0].BaseURL)

	_, ok = h.selectUpstreams("evil")
	assert.False(t, ok)
}

func TestNamedArchiveFetchUsesArchiveRoot(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "InRelease") {
			_, _ = io.WriteString(w, "Origin: Ubuntu\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)

	h := NewHandler(newAptTestCache(),
		WithPathPrefix("/apt"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "default", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "ubuntu", BaseURL: up.URL},
		})),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/apt/ubuntu/dists/jammy/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "path hit: %s", gotPath)
	assert.Equal(t, "/dists/jammy/InRelease", gotPath)
}

func TestUnknownArchiveRejected(t *testing.T) {
	h := NewHandler(newAptTestCache(),
		WithPathPrefix("/apt"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "default", BaseURL: "http://127.0.0.1:1", Priority: 1},
		}),
		WithRepositories(RepositoriesFromSpecs([]RepositorySpec{
			{Name: "ubuntu", BaseURL: "http://127.0.0.1:1"},
		})),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/apt/evil/dists/jammy/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPoolCacheName(t *testing.T) {
	assert.Equal(t, "main/a/foo", poolCacheName("", "main/a/foo"))
	assert.Equal(t, "ubuntu/main/a/foo", poolCacheName("ubuntu", "main/a/foo"))
}
