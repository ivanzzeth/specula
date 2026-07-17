package verify

import (
	"context"
	"fmt"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ChecksumVerifier confirms transport integrity via the streaming sha256 digest
// computed while writing to quarantine. It compares art.Digest (computed
// during streaming) against ref.Digest (the expected reference value).
//
// Lowest tier — NEVER a standalone supply-chain control (DESIGN-REVIEW C1).
// When no reference digest is available (ref.Digest is empty, e.g. a mutable
// tag path), there is nothing to compare the streaming digest against, so the
// verifier self-gates out and returns StatusSkip with a note.
type ChecksumVerifier struct{}

// NewChecksumVerifier constructs a ChecksumVerifier.
func NewChecksumVerifier() *ChecksumVerifier { return &ChecksumVerifier{} }

// Compile-time assertion that ChecksumVerifier satisfies Verifier.
var _ Verifier = (*ChecksumVerifier)(nil)

func (v *ChecksumVerifier) Name() string        { return "checksum" }
func (v *ChecksumVerifier) Tier() artifact.Tier { return artifact.TierChecksum }

// Verify compares the streaming-computed art.Digest against the reference
// ref.Digest. Never re-reads the artifact file; the digest is already
// available from the streaming write (fix C3).
func (v *ChecksumVerifier) Verify(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if art.Digest == "" {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierChecksum,
			Message: "checksum: artifact digest is empty (streaming digest not computed)",
		}, nil
	}

	if ref.Digest == "" {
		// No reference digest is available (e.g. mutable-ref path where the
		// caller has not yet resolved the tag to a digest). A digest was
		// computed, but with nothing known-good to compare it against this
		// verifier has formed no opinion about the artifact — that is a skip,
		// not a pass.
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("checksum: transport digest %s (no reference to compare)", art.Digest),
		}, nil
	}

	if art.Digest != ref.Digest {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("checksum: digest mismatch: got %s, expected %s", art.Digest, ref.Digest),
		}, nil
	}

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierChecksum,
		Message: fmt.Sprintf("checksum: digest verified %s", art.Digest),
	}, nil
}
