package verify

import (
	"context"

	"golang.org/x/mod/sumdb/note"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// SumDBVerifier verifies Go module authenticity against a signed sumdb tree head
// (Ed25519) with inclusion/consistency proofs, proxied via goproxy.cn /sumdb/
// or sum.golang.google.cn (fix H5). Highest tier for Go. Stub only.
type SumDBVerifier struct {
	// verifierKey is the sumdb note verifier key (name+base64 public key).
	verifierKey string
}

// NewSumDBVerifier constructs a SumDBVerifier from a note verifier key.
func NewSumDBVerifier(verifierKey string) *SumDBVerifier {
	return &SumDBVerifier{verifierKey: verifierKey}
}

// Compile-time assertion that SumDBVerifier satisfies Verifier.
var _ Verifier = (*SumDBVerifier)(nil)

func (v *SumDBVerifier) Name() string { return "sumdb" }

func (v *SumDBVerifier) Tier() artifact.Tier { return artifact.TierSigned }

func (v *SumDBVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Reference x/mod/sumdb/note so the dependency is pinned; the leaf agent
	// wires the full signed tree-head + proof verification here.
	if _, err := note.NewVerifier(v.verifierKey); err != nil {
		return artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierSigned, Message: err.Error()}, err
	}
	return artifact.Result{}, errNotImplemented
}
