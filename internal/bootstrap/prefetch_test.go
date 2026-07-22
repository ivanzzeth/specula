package bootstrap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

func TestWarmImages_TokenAndManifest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "t0k"})
	})
	mux.HandleFunc("/v2/library/hello-world/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer t0k", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	results, err := bootstrap.WarmImages(context.Background(), bootstrap.PrefetchOptions{
		Addr:   srv.URL,
		Images: []string{"docker.io/library/hello-world:latest", "hello-world"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, r := range results {
		require.NoError(t, r.Err, r.Ref)
		require.Equal(t, 200, r.StatusCode)
		require.Equal(t, "library/hello-world", r.Path)
	}
}

func TestWarmImages_RequiresAddr(t *testing.T) {
	_, err := bootstrap.WarmImages(context.Background(), bootstrap.PrefetchOptions{
		Images: []string{"alpine:latest"},
	})
	require.Error(t, err)
}
