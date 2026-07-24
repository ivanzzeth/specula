package npm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
)

func TestEnrichPublishTime_FromCachedPackument(t *testing.T) {
	cm := newNpmTestCache()
	packument := []byte(`{"time":{"1.3.0":"2017-04-11T15:00:00.000Z","created":"2014-01-01T00:00:00.000Z"}}`)
	cm.seed(packumentRef("left-pad"), packument)

	h := NewHandler(cm)
	umeta := artifact.UpstreamMeta{LastModified: "Wed, 01 Jan 2020 00:00:00 GMT"}
	ref := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     "left-pad",
		Version:  "left-pad-1.3.0.tgz",
	}
	h.enrichPublishTime(context.Background(), ref, &umeta)

	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2017, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.April, umeta.PublishedAt.UTC().Month())
}

func TestEnrichPublishTime_FromUpstreamPackument(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/left-pad", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"time":{"1.3.0":"2018-09-01T08:30:00.000Z"}}`)
	}))
	defer up.Close()

	h := NewHandler(newNpmTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "npm", BaseURL: up.URL, Priority: 1}}),
	)
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), artifact.ArtifactRef{
		Protocol: Protocol, Name: "left-pad", Version: "left-pad-1.3.0.tgz",
	}, &umeta)

	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2018, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.September, umeta.PublishedAt.UTC().Month())
}

func TestEnrichPublishTime_UpstreamMiss_NoPublishTime(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer up.Close()

	h := NewHandler(newNpmTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "npm", BaseURL: up.URL, Priority: 1}}),
	)
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), artifact.ArtifactRef{
		Protocol: Protocol, Name: "missing", Version: "missing-1.0.0.tgz",
	}, &umeta)
	assert.True(t, umeta.PublishedAt.IsZero())
}

func TestEnrichPublishTime_NoopWhenAlreadySet(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	umeta := artifact.UpstreamMeta{PublishedAt: want}
	h.enrichPublishTime(context.Background(), artifact.ArtifactRef{
		Protocol: Protocol, Name: "x", Version: "x-1.0.0.tgz",
	}, &umeta)
	assert.Equal(t, want, umeta.PublishedAt)
}

func TestEnrichPublishTime_NilMeta(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	h.enrichPublishTime(context.Background(), artifact.ArtifactRef{
		Protocol: Protocol, Name: "x", Version: "x-1.0.0.tgz",
	}, nil) // must not panic
}
