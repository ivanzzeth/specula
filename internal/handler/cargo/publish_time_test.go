package cargo

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

func TestEnrichPublishTime_FromCrateAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/crates/serde/1.0.0", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":{"created_at":"2015-01-01T12:00:00.000000Z"}}`)
	}))
	defer srv.Close()

	prev := defaultAPIBase
	defaultAPIBase = srv.URL
	t.Cleanup(func() { defaultAPIBase = prev })

	h := NewHandler(newCargoTestCache(), WithUpstream(upstream.NewClient(), nil))
	umeta := artifact.UpstreamMeta{}
	h.enrichPublishTime(context.Background(), crateRef("serde", "1.0.0"), &umeta)

	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2015, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.January, umeta.PublishedAt.UTC().Month())
}
