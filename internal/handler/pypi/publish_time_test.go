package pypi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestEnrichPublishTime_FromCachedSimpleJSON(t *testing.T) {
	cm := newPypiTestCache()
	body := []byte(`{
		"files": [
			{"filename": "pkg-1.0-py3-none-any.whl", "upload-time": "2021-06-15T12:00:00.000000Z"}
		]
	}`)
	jsonRef := artifact.ArtifactRef{
		Protocol: Protocol, Name: "pkg", Version: indexVersionJSON, Mutable: true,
	}
	cm.seed(jsonRef, body)

	h := NewHandler(cm)
	umeta := artifact.UpstreamMeta{}
	ref := fileRef("ab/cd", "pkg-1.0-py3-none-any.whl")
	h.enrichPublishTime(context.Background(), ref, &umeta)

	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2021, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.June, umeta.PublishedAt.UTC().Month())
}

func TestEnrichPublishTime_FromUpstreamSimpleJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/simple/flask/", r.URL.Path)
		assert.Contains(t, r.Header.Get("Accept"), ctSimpleJSON)
		w.Header().Set("Content-Type", ctSimpleJSON)
		_, _ = io.WriteString(w, `{
			"files":[{"filename":"flask-2.0.0-py3-none-any.whl","upload-time":"2021-05-11T14:00:00Z"}]
		}`)
	}))
	defer up.Close()

	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: up.URL, Priority: 1}}),
	)
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), fileRef("ab/cd", "flask-2.0.0-py3-none-any.whl"), &umeta)
	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2021, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.May, umeta.PublishedAt.UTC().Month())
}

func TestEnrichPublishTime_FromWarehouseJSON(t *testing.T) {
	// Upstream ignores PEP 691 Accept (returns HTML) so enrich falls through
	// to Warehouse /pypi/<name>/json.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/simple/"):
			w.Header().Set("Content-Type", ctSimpleHTML)
			_, _ = io.WriteString(w, `<!DOCTYPE html><html></html>`)
		case r.URL.Path == "/pypi/flask/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"releases":{"2.0.0":[{
				"filename":"flask-2.0.0-py3-none-any.whl",
				"upload_time_iso_8601":"2021-05-11T14:00:00.000000Z"
			}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: up.URL, Priority: 1}}),
	)
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), fileRef("ab/cd", "flask-2.0.0-py3-none-any.whl"), &umeta)
	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2021, umeta.PublishedAt.UTC().Year())
}

func TestEnrichPublishTime_EmptyFilename(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), fileRef("ab/cd", ""), &umeta)
	assert.True(t, umeta.PublishedAt.IsZero())
}

func TestSimpleIndex_JSONAccept_UpstreamNegotiation(t *testing.T) {
	jsonBody := []byte(`{"meta":{"api-version":"1.0"},"name":"flask","files":[]}`)
	var sawAccept string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAccept = r.Header.Get("Accept")
		if strings.Contains(sawAccept, ctSimpleJSON) {
			w.Header().Set("Content-Type", ctSimpleJSON)
			_, _ = w.Write(jsonBody)
			return
		}
		w.Header().Set("Content-Type", ctSimpleHTML)
		_, _ = io.WriteString(w, `<!DOCTYPE html><html></html>`)
	}))
	defer up.Close()

	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: up.URL, Priority: 1}}),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/simple/flask/", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", ctSimpleJSON)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "json")
	assert.Contains(t, sawAccept, ctSimpleJSON)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, jsonBody, got)

	// Second request hits the simple-json cache slot (no upstream Accept needed).
	sawAccept = ""
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/simple/flask/", nil)
	req2.Header.Set("Accept", ctSimpleJSON)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Contains(t, resp2.Header.Get("Content-Type"), "json")
}

func TestSimpleIndex_JSONAccept_UpstreamHTMLFallback(t *testing.T) {
	htmlBody := []byte(`<!DOCTYPE html><html><body>flask</body></html>`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always HTML — simulates mirrors that ignore PEP 691 Accept.
		w.Header().Set("Content-Type", ctSimpleHTML)
		_, _ = w.Write(htmlBody)
	}))
	defer up.Close()

	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: up.URL, Priority: 1}}),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/simple/flask/", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", ctSimpleJSON)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, htmlBody, got)
}

func TestTryServeJSONIndex_NoUpstream(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/simple/flask/", nil)
	ok := h.tryServeJSONIndex(rec, req, artifact.ArtifactRef{
		Protocol: Protocol, Name: "flask", Version: indexVersionJSON, Mutable: true,
	}, nil)
	assert.False(t, ok)
}
