// Package verify defines the streaming verification chain (fix C3): verifiers
// receive an on-disk quarantined *artifact.Artifact (never a []byte blob) whose
// digest was computed while streaming. The Chain runs registered verifiers and
// records the honest tier actually achieved (DESIGN-REVIEW §1.2).
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// errNotImplemented is retained so stub verifiers (e.g. sumdb.go) that have
// not yet been fully implemented can reference it without failing compilation.
var errNotImplemented = errors.New("verify: not implemented")

// Verifier verifies a quarantined artifact in a streaming fashion.
type Verifier interface {
	// Name is the verifier's stable identifier (e.g. "checksum", "sumdb").
	Name() string
	// Tier is the trust tier this verifier can attest.
	Tier() artifact.Tier
	// Verify inspects the on-disk artifact (art.Path) and returns the result.
	Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error)
}

// Chain runs an ordered set of verifiers with short-circuit semantics and
// records the highest tier achieved.
type Chain struct {
	verifiers []Verifier
}

// NewChain builds a verification Chain from an ordered list of verifiers.
func NewChain(verifiers ...Verifier) *Chain {
	return &Chain{verifiers: verifiers}
}

// Verifiers returns the ordered verifiers registered in the chain.
func (c *Chain) Verifiers() []Verifier {
	return c.verifiers
}

// Verify runs the chain over the quarantined artifact and returns the aggregate
// result. Short-circuits on the first StatusFail (including verifier errors).
// The Tier in the returned Result reflects:
//   - On PASS/WARN: the highest Tier reached across all verifiers.
//   - On FAIL: the Tier of the failing verifier.
func (c *Chain) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if len(c.verifiers) == 0 {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "chain: no verifiers registered",
		}, nil
	}

	var (
		highestTier = artifact.TierChecksum
		warnSeen    bool
		msgs        = make([]string, 0, len(c.verifiers))
	)

	for _, v := range c.verifiers {
		res, err := v.Verify(ctx, ref, art)
		if err != nil {
			// Treat a verifier error as an immediate failure at that tier.
			return artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    v.Tier(),
				Message: fmt.Sprintf("chain: verifier %q error: %v", v.Name(), err),
			}, fmt.Errorf("chain: verifier %q: %w", v.Name(), err)
		}
		if res.Status == artifact.StatusFail {
			// Short-circuit: return failing tier and message immediately.
			return artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    v.Tier(),
				Message: res.Message,
			}, nil
		}

		// PASS or WARN: advance the highest tier reached.
		if res.Tier > highestTier {
			highestTier = res.Tier
		}
		if res.Status == artifact.StatusWarn {
			warnSeen = true
		}
		msgs = append(msgs, fmt.Sprintf("[%s] %s", v.Name(), res.Message))
	}

	status := artifact.StatusPass
	if warnSeen {
		status = artifact.StatusWarn
	}
	return artifact.Result{
		Status:  status,
		Tier:    highestTier,
		Message: strings.Join(msgs, "; "),
	}, nil
}
