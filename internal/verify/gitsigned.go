package verify

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// GitSignedVerifier verifies a signed git tag or commit against an
// allowed-signers file (ARCHITECTURE §9, DESIGN-REVIEW §5 — the optional git
// "signed" anchor). git objects are already a SHA-256/SHA-1 Merkle DAG
// (inherent checksum tier); this verifier lifts a ref to TierSigned when the
// tag/commit at its tip carries a valid signature from an allowed signer.
//
//	allowed-signers file  →  set of trusted principals + public keys (SSH/GPG)
//	signed tag/commit     →  `git verify-tag` / `git verify-commit` against them
//
// Unlike the apt/helm keyrings this uses git's own machinery via os/exec (no
// OpenPGP dependency): `git -c gpg.ssh.allowedSignersFile=<path> verify-tag`.
//
// # Tier
//
// Tier() is TierSigned. A ref whose tip is unsigned is a degrade to tofu, not a
// hard failure (signed refs are opt-in per PRD §信任模型).
//
// # Self-gating
//
// Verify is a no-op StatusPass for any ref whose Protocol is not "git".
//
// # Implementation status (contract skeleton)
//
// The allowed-signers path is validated in the constructor. The actual
// verify-tag/verify-commit invocation against the bare mirror is NOT yet
// implemented; Verify returns errNotImplemented so the Chain fails closed if
// this verifier is registered before the git handler agent completes it.
type GitSignedVerifier struct {
	// allowedSigners is the path to the git allowed-signers file (SSH format) or
	// GPG keyring used to authorise tag/commit signatures.
	allowedSigners string
	// gitBin is the git executable used for verify-tag/verify-commit.
	gitBin string
}

// NewGitSignedVerifier returns a verifier that authorises signed tags/commits
// against the allowed-signers file at allowedSignersPath. An empty path is a
// fatal wiring error. The git binary is resolved from PATH.
func NewGitSignedVerifier(allowedSignersPath string) (*GitSignedVerifier, error) {
	if allowedSignersPath == "" {
		return nil, fmt.Errorf("gitsigned: allowed-signers path is required for signed-ref verification")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("gitsigned: git binary not found on PATH: %w", err)
	}
	return &GitSignedVerifier{allowedSigners: allowedSignersPath, gitBin: gitBin}, nil
}

// Compile-time assertion that GitSignedVerifier satisfies Verifier.
var _ Verifier = (*GitSignedVerifier)(nil)

func (v *GitSignedVerifier) Name() string        { return "gitsigned" }
func (v *GitSignedVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// Verify checks the signature on the tag/commit referenced by ref.
//
// Skipped (StatusPass at TierChecksum) for any non-git ref. For git refs the
// verify-tag/verify-commit invocation is not yet implemented; Verify returns
// errNotImplemented so the Chain fails closed rather than attesting an
// unreached tier.
func (v *GitSignedVerifier) Verify(_ context.Context, ref artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
	if ref.Protocol != "git" {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "gitsigned: skipped (not a git artifact)",
		}, nil
	}
	return artifact.Result{
		Status:  artifact.StatusFail,
		Tier:    artifact.TierSigned,
		Message: "gitsigned: signed tag/commit verification (allowed-signers) not implemented",
	}, errNotImplemented
}
