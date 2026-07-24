package pypi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
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
	ref := fileRef("pkg", "pkg-1.0-py3-none-any.whl")
	h.enrichPublishTime(context.Background(), ref, &umeta)

	require.False(t, umeta.PublishedAt.IsZero())
	assert.Equal(t, 2021, umeta.PublishedAt.UTC().Year())
	assert.Equal(t, time.June, umeta.PublishedAt.UTC().Month())
}
