package hf

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestPassthroughAuth(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		require.Equal(t, "/api/models/bert-base-uncased", r.URL.Path)
		_, _ = io.WriteString(w, `{"private":true}`)
	}))
	t.Cleanup(up.Close)

	h := NewHandler(nil,
		WithPathPrefix("/hf"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hf", BaseURL: up.URL, Priority: 1},
		}),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/hf/api/models/bert-base-uncased", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "Bearer secret-token", gotAuth)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"private":true`)
}

func TestRewriteJSONURLs(t *testing.T) {
	in := []byte(`{"url":"https://huggingface.co/api/models/foo","nested":{"link":"https://hf.co/datasets/bar?rev=main"}}`)
	out := rewriteJSONURLs(in, "http://127.0.0.1:7732", "/hf")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	assert.Equal(t, "http://127.0.0.1:7732/hf/api/models/foo", doc["url"])
	nested := doc["nested"].(map[string]any)
	assert.Equal(t, "http://127.0.0.1:7732/hf/datasets/bar?rev=main", nested["link"])
}

func TestRewriteJSONURLsPreservesOtherHosts(t *testing.T) {
	in := []byte(`{"url":"https://example.com/foo"}`)
	out := rewriteJSONURLs(in, "http://127.0.0.1:7732", "/hf")
	var doc map[string]string
	require.NoError(t, json.Unmarshal(out, &doc))
	assert.Equal(t, "https://example.com/foo", doc["url"])
}

func TestRewriteJSONURLsInvalidJSON(t *testing.T) {
	in := []byte(`not json`)
	out := rewriteJSONURLs(in, "http://127.0.0.1:7732", "/hf")
	assert.Equal(t, in, out)
}

func TestIsMutablePath(t *testing.T) {
	assert.True(t, isMutablePath("api/models/foo"))
	assert.True(t, isMutablePath("api/models/foo/revision/main"))
	assert.False(t, isMutablePath("org/model/resolve/main/config.json"),
		"resolve downloads are immutable even when the file is .json")
	assert.False(t, isMutablePath("org/model/resolve/abc123/pytorch_model.bin"))
	assert.True(t, isMutablePath("org/model/config.json"),
		"bare .json metadata paths remain mutable")
}
