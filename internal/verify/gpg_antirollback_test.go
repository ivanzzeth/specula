package verify

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

func TestGPGVerifier_AntiRollback_RejectsOlderInRelease(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile, WithAptPinStore(NewMemAptPinStore()))
	require.NoError(t, err)

	sums := []string{sha256Hex([]byte("packages-v1")) + " 12 main/binary-amd64/Packages"}
	newer := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	older := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	ref := artifact.ArtifactRef{Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease", Mutable: true}

	newerPath, newerDig := writeQuarantine(t, signInReleaseAt(t, key, newer, sums))
	res, err := v.Verify(context.Background(), ref, &artifact.Artifact{Path: newerPath, Digest: newerDig, Size: 1})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, res.Message)

	olderPath, olderDig := writeQuarantine(t, signInReleaseAt(t, key, older, sums))
	res, err = v.Verify(context.Background(), ref, &artifact.Artifact{Path: olderPath, Digest: olderDig, Size: 1})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Contains(t, res.Message, "anti-rollback")
}

func TestGPGVerifier_AntiRollback_AllowsEqualOrNewer(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile, WithAptPinStore(NewMemAptPinStore()))
	require.NoError(t, err)

	sums := []string{sha256Hex([]byte("packages-v1")) + " 12 main/binary-amd64/Packages"}
	d1 := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 7, 2, 0, 0, 0, 0, time.UTC)
	ref := artifact.ArtifactRef{Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease", Mutable: true}

	path1, dig1 := writeQuarantine(t, signInReleaseAt(t, key, d1, sums))
	res, err := v.Verify(context.Background(), ref, &artifact.Artifact{Path: path1, Digest: dig1, Size: 1})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status)

	path1b, dig1b := writeQuarantine(t, signInReleaseAt(t, key, d1, sums))
	res, err = v.Verify(context.Background(), ref, &artifact.Artifact{Path: path1b, Digest: dig1b, Size: 1})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, res.Message)

	path2, dig2 := writeQuarantine(t, signInReleaseAt(t, key, d2, sums))
	res, err = v.Verify(context.Background(), ref, &artifact.Artifact{Path: path2, Digest: dig2, Size: 1})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, res.Message)
}

func TestParseInReleaseDate(t *testing.T) {
	got, err := parseInReleaseDate("Sat, 15 Jun 2024 12:00:00 +0000")
	require.NoError(t, err)
	assert.Equal(t, 2024, got.Year())
	_, err = parseInReleaseDate("")
	require.Error(t, err)
}
