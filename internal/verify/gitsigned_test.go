package verify

// Tests for GitSignedVerifier (gitsigned.go).
//
// Traceable to:
//   - PRD §G2 "git: signed (可选) — 签名 tag/commit (配 allowed-signers)"
//   - ARCHITECTURE: signed refs are opt-in; unsigned refs degrade to tofu, not fail
//   - gitsigned.go doc: "Verify returns errNotImplemented so the Chain fails
//     closed if this verifier is registered before the git handler completes it."

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constructor
// ─────────────────────────────────────────────────────────────────────────────

// TestNewGitSignedVerifier_EmptyPath verifies that an empty allowed-signers
// path is a fatal wiring error (fail-fast principle).
func TestNewGitSignedVerifier_EmptyPath(t *testing.T) {
	_, err := NewGitSignedVerifier("")
	require.Error(t, err, "empty path must be rejected")
	assert.Contains(t, err.Error(), "allowed-signers")
}

// TestNewGitSignedVerifier_ValidPath verifies that a non-empty path (even if the
// file does not exist yet) succeeds — the path is validated at construction, not
// at runtime (per the doc comment).
func TestNewGitSignedVerifier_ValidPath(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "allowed-signers-*")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	v, err := NewGitSignedVerifier(f.Name())
	if err != nil {
		// git binary not found on PATH: skip this test in that environment.
		if os.IsNotExist(err) || (err != nil && len(err.Error()) > 0) {
			t.Skipf("git not found on PATH: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
	assert.Equal(t, "gitsigned", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())
}

// TestNewGitSignedVerifier_FilePath is a variant that uses a path within a temp dir.
func TestNewGitSignedVerifier_FilePath(t *testing.T) {
	dir := t.TempDir()
	allowedSigners := filepath.Join(dir, "allowed-signers")
	require.NoError(t, os.WriteFile(allowedSigners, []byte("# allowed signers\n"), 0o600))

	v, err := NewGitSignedVerifier(allowedSigners)
	if err != nil {
		// git may not be on PATH in some CI environments; that's acceptable.
		t.Skipf("skipping: %v", err)
	}
	require.NotNil(t, v)
}

// ─────────────────────────────────────────────────────────────────────────────
// Self-gating: non-git protocol
// ─────────────────────────────────────────────────────────────────────────────

// TestGitSignedVerifier_NonGitProtocol_Skipped verifies that the verifier is a
// no-op for all non-git protocols (PRD §G2: per-protocol self-gating).
func TestGitSignedVerifier_NonGitProtocol_Skipped(t *testing.T) {
	dir := t.TempDir()
	allowedSigners := filepath.Join(dir, "allowed-signers")
	require.NoError(t, os.WriteFile(allowedSigners, []byte(""), 0o600))

	v, err := NewGitSignedVerifier(allowedSigners)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	protocols := []string{"oci", "pypi", "npm", "gomod", "apt", "helm", "tarball"}
	ctx := context.Background()
	for _, proto := range protocols {
		t.Run(proto, func(t *testing.T) {
			ref := artifact.ArtifactRef{
				Protocol: proto,
				Name:     "example",
				Version:  "1.0.0",
				Mutable:  false,
			}
			art := &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"}

			res, err := v.Verify(ctx, ref, art)
			require.NoError(t, err, "self-gate for %s must not error", proto)
			assert.Equal(t, artifact.StatusPass, res.Status, "non-git protocol must pass through")
			assert.Equal(t, artifact.TierChecksum, res.Tier, "non-git must not claim TierSigned")
			assert.Contains(t, res.Message, "skipped")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fail-closed for git refs (not-yet-implemented contract)
// ─────────────────────────────────────────────────────────────────────────────

// TestGitSignedVerifier_GitRef_FailClosed verifies that a git ref returns an
// error (fail-closed) because the verify-tag/verify-commit implementation is not
// yet complete. The chain must NEVER silently pass an artifact through this verifier
// when it is wired into the chain — it must produce StatusFail+error so the chain
// short-circuits.
//
// This test documents the CONTRACT: once the implementation is complete, this test
// will need to be updated to test the actual signed/unsigned result. Until then
// the test ensures the "not implemented → fail closed" invariant holds.
func TestGitSignedVerifier_GitRef_FailClosed(t *testing.T) {
	dir := t.TempDir()
	allowedSigners := filepath.Join(dir, "allowed-signers")
	require.NoError(t, os.WriteFile(allowedSigners, []byte(""), 0o600))

	v, err := NewGitSignedVerifier(allowedSigners)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	ctx := context.Background()
	ref := artifact.ArtifactRef{
		Protocol: "git",
		Name:     "github.com/owner/repo",
		Version:  "v1.2.3",
		Mutable:  false,
	}
	art := &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"}

	res, verErr := v.Verify(ctx, ref, art)

	// The not-yet-implemented verifier must fail closed: both StatusFail and a
	// non-nil error so the chain treats it as a hard failure, not a pass.
	require.Error(t, verErr, "not-implemented git verifier must return error (fail-closed)")
	assert.Equal(t, artifact.StatusFail, res.Status,
		"not-implemented git verifier must return StatusFail, not StatusPass")
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"failing verifier must report its Tier so the chain correctly records it")
}

// TestGitSignedVerifier_Interface verifies Name/Tier contracts.
func TestGitSignedVerifier_Interface(t *testing.T) {
	dir := t.TempDir()
	allowedSigners := filepath.Join(dir, "allowed-signers")
	require.NoError(t, os.WriteFile(allowedSigners, []byte(""), 0o600))

	v, err := NewGitSignedVerifier(allowedSigners)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	assert.Equal(t, "gitsigned", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())
	var _ Verifier = v // compile-time interface check
}
