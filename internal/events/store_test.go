package events_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/events"
)

func TestMemoryRingDropsOldest(t *testing.T) {
	m := events.NewMemory(3)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		m.Record(ctx, events.Event{Protocol: "oci", Artifact: "a", Result: "fail", Detail: string(rune('0' + i))})
	}
	got := m.List(ctx, 10)
	require.Len(t, got, 3)
	// Newest first.
	assert.Equal(t, int64(5), got[0].ID)
	assert.Equal(t, int64(4), got[1].ID)
	assert.Equal(t, int64(3), got[2].ID)
}

func TestFromVerify(t *testing.T) {
	e := events.FromVerify(
		artifact.ArtifactRef{Protocol: "apt", Name: "ubuntu", Version: "jammy/InRelease"},
		"sha256:abc",
		artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierSigned, Message: "bad sig"},
	)
	assert.Equal(t, "fail", e.Result)
	assert.Equal(t, "apt", e.Protocol)
	assert.Equal(t, "ubuntu:jammy/InRelease", e.Artifact)
	assert.Equal(t, "bad sig", e.Detail)
}
