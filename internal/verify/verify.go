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
	"github.com/ivanzzeth/specula/internal/metrics"
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
		// A chain with no verifiers claims TierChecksum while having compared
		// no hash at all. That claim is not honest, and it is deliberately NOT
		// counted into specula_verification_total: no verifier ran, so there is
		// no verification to report, and emitting tier="checksum" here would put
		// a fabricated tier on /metrics.
		//
		// The claim itself is left in place rather than changed here: it is
		// unreachable in production (cmd/specula always registers at least
		// ChecksumVerifier + TofuVerifier, so an empty chain is a degenerate
		// configuration no operator can construct), and PRD §G2's four tiers have
		// no vocabulary for "nothing was checked" — the honest repair is a spec
		// question, not a silent code change. Reported rather than enshrined.
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
			metrics.RecordVerification(ref.Protocol, v.Name(), v.Tier(), artifact.StatusFail)
			metrics.RecordVerification(ref.Protocol, metrics.CheckChain, v.Tier(), artifact.StatusFail)
			return artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    v.Tier(),
				Message: fmt.Sprintf("chain: verifier %q error: %v", v.Name(), err),
			}, fmt.Errorf("chain: verifier %q: %w", v.Name(), err)
		}
		if res.Status == artifact.StatusFail {
			// Short-circuit: return failing tier and message immediately.
			metrics.RecordVerification(ref.Protocol, v.Name(), v.Tier(), artifact.StatusFail)
			metrics.RecordVerification(ref.Protocol, metrics.CheckChain, v.Tier(), artifact.StatusFail)
			return artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    v.Tier(),
				Message: res.Message,
			}, nil
		}
		if res.Status == artifact.StatusSkip {
			// The verifier self-gated out: it reached no tier and attests
			// nothing. Emit NO series — absence is how this metric says "that
			// check did not run here" — and do not let it touch the aggregate.
			continue
		}

		// PASS or WARN: the verifier really ran. Record the tier IT reached
		// (res.Tier, the honest outcome), never v.Tier(), which is only the
		// tier the verifier is capable of attesting at best.
		metrics.RecordVerification(ref.Protocol, v.Name(), res.Tier, res.Status)

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
	// The chain-level series. highestTier is the exact value cache.Store writes
	// to CacheEntry.Tier, so this counter and the tier column in the metadata
	// store are two renderings of one number — which is what makes them
	// cross-checkable, and what makes a disagreement between them a real signal.
	metrics.RecordVerification(ref.Protocol, metrics.CheckChain, highestTier, status)
	return artifact.Result{
		Status:  status,
		Tier:    highestTier,
		Message: strings.Join(msgs, "; "),
	}, nil
}
