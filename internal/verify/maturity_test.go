package verify_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/verify"
)

func TestMaturityVerifierYoungWarn(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	v := verify.NewMaturityVerifier(map[string]verify.MaturitySpec{
		"npm": {MinAge: 72 * time.Hour, Policy: verify.MaturityWarn},
	})
	v.Now = func() time.Time { return now }

	art := &artifact.Artifact{
		Digest: "sha256:abc",
		Meta:   artifact.UpstreamMeta{PublishedAt: now.Add(-24 * time.Hour)},
	}
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "npm", Name: "left-pad", Version: "1.0.0",
	}, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusWarn, res.Status)
	assert.Contains(t, res.Message, "too young")
}

func TestMaturityVerifierYoungEnforce(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	v := verify.NewMaturityVerifier(map[string]verify.MaturitySpec{
		"pypi": {MinAge: 72 * time.Hour, Policy: verify.MaturityEnforce},
	})
	v.Now = func() time.Time { return now }

	art := &artifact.Artifact{
		Meta: artifact.UpstreamMeta{PublishedAt: now.Add(-1 * time.Hour)},
	}
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "pypi", Name: "requests", Version: "99.0.0",
	}, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
}

func TestMaturityVerifierOldEnough(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	v := verify.NewMaturityVerifier(map[string]verify.MaturitySpec{
		"cargo": {MinAge: 24 * time.Hour, Policy: verify.MaturityEnforce},
	})
	v.Now = func() time.Time { return now }

	art := &artifact.Artifact{
		Meta: artifact.UpstreamMeta{PublishedAt: now.Add(-48 * time.Hour)},
	}
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "cargo", Name: "serde", Version: "1.0.0",
	}, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
}

func TestMaturityVerifierLastModifiedFallback(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	v := verify.NewMaturityVerifier(map[string]verify.MaturitySpec{
		"npm": {MinAge: 72 * time.Hour, Policy: verify.MaturityWarn},
	})
	v.Now = func() time.Time { return now }

	art := &artifact.Artifact{
		Meta: artifact.UpstreamMeta{
			LastModified: now.Add(-100 * time.Hour).UTC().Format(http.TimeFormat),
		},
	}
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "npm", Name: "x", Version: "1.0.0",
	}, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Contains(t, res.Message, "Last-Modified")
}

func TestMaturityVerifierNoTimeSkips(t *testing.T) {
	v := verify.NewMaturityVerifier(map[string]verify.MaturitySpec{
		"npm": {MinAge: 72 * time.Hour},
	})
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "npm", Name: "x", Version: "1.0.0",
	}, &artifact.Artifact{})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusSkip, res.Status)
}
