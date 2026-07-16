package verify

import (
	"context"
	"fmt"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TofuStore is the minimal storage dependency injected into TofuVerifier.
// Production callers provide a concrete adapter (e.g. wrapping meta.MetadataStore
// via MutableEntry); tests use the in-memory fakeTofuStore.
//
// The interface is deliberately narrow: TofuVerifier only needs to read and
// write a single string digest per lookup key.
type TofuStore interface {
	// GetPin returns the previously pinned digest for key, or ("", nil) if no
	// pin exists yet.
	GetPin(ctx context.Context, key string) (string, error)
	// SetPin stores the first-seen digest for key. Implementations should be
	// idempotent when called with the same (key, digest) pair.
	SetPin(ctx context.Context, key, digest string) error
}

// tofuKey builds the stable lookup key used to pin a TOFU digest. The key
// encodes protocol, name, and version so each immutable artifact version gets
// its own independent pin.
func tofuKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + "@" + ref.Version
}

// TofuVerifier locks the digest on first fetch (first-lock, StatusWarn) and
// returns StatusFail with a clear change-alert message when a later fetch
// presents a different digest for the same immutable version — detecting
// force-push, history rewrite, and version rewrite attacks (DESIGN-REVIEW §5).
//
// Mutable refs (tags, indexes, git refs whose digest legitimately changes over
// time) are skipped with StatusPass so the mutable tier is not incorrectly
// pinned.
type TofuVerifier struct {
	store TofuStore
}

// NewTofuVerifier constructs a TofuVerifier backed by the supplied TofuStore.
func NewTofuVerifier(store TofuStore) *TofuVerifier {
	return &TofuVerifier{store: store}
}

// Compile-time assertion that TofuVerifier satisfies Verifier.
var _ Verifier = (*TofuVerifier)(nil)

func (v *TofuVerifier) Name() string        { return "tofu" }
func (v *TofuVerifier) Tier() artifact.Tier { return artifact.TierTofu }

// Verify implements TOFU pinning for immutable artifact versions.
//
//   - Mutable ref → StatusPass (skipped; digest legitimately changes).
//   - First sight  → pin digest, return StatusWarn (first-lock notification).
//   - Same digest  → StatusPass (confirmed).
//   - Changed digest → StatusFail with tamper-alert message (fail-closed).
func (v *TofuVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if ref.Mutable {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierTofu,
			Message: "tofu: skipped for mutable ref",
		}, nil
	}

	key := tofuKey(ref)

	pinned, err := v.store.GetPin(ctx, key)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierTofu,
			Message: fmt.Sprintf("tofu: store read error for %q: %v", key, err),
		}, fmt.Errorf("tofu: store.GetPin(%q): %w", key, err)
	}

	if pinned == "" {
		// First sight of this immutable version: pin the digest now.
		if err := v.store.SetPin(ctx, key, art.Digest); err != nil {
			return artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    artifact.TierTofu,
				Message: fmt.Sprintf("tofu: failed to pin digest for %q: %v", key, err),
			}, fmt.Errorf("tofu: store.SetPin(%q, %q): %w", key, art.Digest, err)
		}
		return artifact.Result{
			Status:  artifact.StatusWarn,
			Tier:    artifact.TierTofu,
			Message: fmt.Sprintf("tofu: first-lock pinned %s → %s", key, art.Digest),
		}, nil
	}

	if pinned != art.Digest {
		// Digest changed for a version that was previously pinned.
		// This is a strong signal of tampering, force-push, or version rewrite.
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierTofu,
			Message: fmt.Sprintf(
				"tofu: DIGEST CHANGED for %s — was %s, now %s — possible tampering, force-push, or version rewrite",
				key, pinned, art.Digest,
			),
		}, nil
	}

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierTofu,
		Message: fmt.Sprintf("tofu: digest confirmed %s → %s", key, art.Digest),
	}, nil
}
