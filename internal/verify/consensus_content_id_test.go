package verify

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

func npmIntegrityFor(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func writeContentIDArtifact(t *testing.T, body []byte) (*artifact.Artifact, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	sum := sha256.Sum256(body)
	art := &artifact.Artifact{
		Path:   path,
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size:   int64(len(body)),
	}
	return art, npmIntegrityFor(body)
}

func npmTarballRef() artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "npm",
		Name:     "left-pad",
		Version:  "left-pad-1.3.0.tgz",
		Mutable:  false,
	}
}

func TestDigestsEqual_NeverEquatesSSRItoCAS(t *testing.T) {
	body := []byte("npm-tarball-bytes")
	cas := "sha256:" + hex.EncodeToString(sha256Sum(body))
	ssri := npmIntegrityFor(body)
	assert.False(t, digestsEqual(ssri, cas), "sha512 integrity must never equal CAS sha256")
	assert.False(t, digestsEqual(cas, ssri))
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestConsensusVerifier_NPMContentID_Agree(t *testing.T) {
	body := []byte("agree-body-content")
	art, integrity := writeContentIDArtifact(t, body)

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1":     {digest: integrity},
		"m2":     {digest: integrity},
		"origin": {digest: integrity},
	})
	v := NewConsensusVerifier(ConsensusConfig{
		Quorum:       2,
		Mirrors:      mirrors("m1", "m2"),
		OriginCheck:  OriginCheck{URL: "https://registry.npmjs.org"},
		IdentityMode: IdentityContentID,
	}, fetcher)

	res, err := v.Verify(context.Background(), npmTarballRef(), art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "integrity="+integrity)
	assert.NotContains(t, res.Message, "agreed on "+art.Digest+" for")
}

func TestConsensusVerifier_NPMContentID_PoisonedMirror(t *testing.T) {
	body := []byte("poison-test-body")
	art, integrity := writeContentIDArtifact(t, body)
	poisoned := npmIntegrityFor([]byte("attacker-payload"))

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: integrity},
		"m2": {digest: poisoned},
	})
	v := NewConsensusVerifier(ConsensusConfig{
		Quorum:       2,
		Mirrors:      mirrors("m1", "m2"),
		IdentityMode: IdentityContentID,
	}, fetcher)

	res, err := v.Verify(context.Background(), npmTarballRef(), art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Contains(t, res.Message, "quorum not met")
}

func TestConsensusVerifier_NPMContentID_BodyMismatch(t *testing.T) {
	body := []byte("actual-body")
	art, _ := writeContentIDArtifact(t, body)
	// Mirrors advertise integrity for different bytes than the quarantine file.
	wrongIntegrity := npmIntegrityFor([]byte("different-bytes"))

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: wrongIntegrity},
		"m2": {digest: wrongIntegrity},
	})
	v := NewConsensusVerifier(ConsensusConfig{
		Quorum:       2,
		Mirrors:      mirrors("m1", "m2"),
		IdentityMode: IdentityContentID,
	}, fetcher)

	res, err := v.Verify(context.Background(), npmTarballRef(), art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Contains(t, res.Message, "body does not match content-id")
}

func TestConsensusVerifier_NPMContentID_OriginDisagrees(t *testing.T) {
	body := []byte("origin-disagree")
	art, integrity := writeContentIDArtifact(t, body)
	other := npmIntegrityFor([]byte("official-says-other"))

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1":     {digest: integrity},
		"m2":     {digest: integrity},
		"origin": {digest: other},
	})
	v := NewConsensusVerifier(ConsensusConfig{
		Quorum:       2,
		Mirrors:      mirrors("m1", "m2"),
		OriginCheck:  OriginCheck{URL: "https://registry.npmjs.org"},
		IdentityMode: IdentityContentID,
	}, fetcher)

	res, err := v.Verify(context.Background(), npmTarballRef(), art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Contains(t, res.Message, "OFFICIAL SOURCE disagrees")
}

func TestConsensusVerifier_CargoContentID_Agree(t *testing.T) {
	body := []byte("crate-bytes")
	dir := t.TempDir()
	path := filepath.Join(dir, "crate")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	sum := sha256.Sum256(body)
	cksum := hex.EncodeToString(sum[:])
	art := &artifact.Artifact{
		Path:   path,
		Digest: "sha256:" + cksum, // CAS happens to equal cksum for sha256 bodies — still Content-ID path
		Size:   int64(len(body)),
	}

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: cksum},
	})
	v := NewConsensusVerifier(ConsensusConfig{
		Quorum:       1,
		Mirrors:      mirrors("m1"),
		IdentityMode: IdentityContentID,
	}, fetcher)

	ref := artifact.ArtifactRef{Protocol: "cargo", Name: "serde", Version: "1.0.0"}
	res, err := v.Verify(context.Background(), ref, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Contains(t, res.Message, "integrity="+cksum)
}

func TestVerifyBodyContentID_SSRI(t *testing.T) {
	body := []byte("ssri-check")
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	require.NoError(t, verifyBodyContentID(path, npmIntegrityFor(body)))
	require.Error(t, verifyBodyContentID(path, npmIntegrityFor([]byte("nope"))))
}
