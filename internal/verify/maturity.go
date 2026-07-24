package verify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// MaturityPolicy is the cool-down gate action for versions younger than MinAge.
type MaturityPolicy string

const (
	MaturityWarn    MaturityPolicy = "warn"
	MaturityEnforce MaturityPolicy = "enforce"
)

// MaturitySpec is the per-protocol cool-down configuration consumed by
// MaturityVerifier. MinAge <= 0 disables the gate for that protocol.
type MaturitySpec struct {
	MinAge time.Duration
	Policy MaturityPolicy // warn (default) | enforce
}

// MaturityVerifier is a policy gate (NOT a trust tier) that rejects or warns on
// package versions younger than a configured min_age. It self-gates by
// ref.Protocol using Specs. Missing publish time → StatusSkip (honest).
//
// Target ecosystems: npm, pypi, cargo (and any future protocol that sets
// UpstreamMeta.PublishedAt or a usable Last-Modified).
type MaturityVerifier struct {
	Specs map[string]MaturitySpec // protocol → spec
	Now   func() time.Time        // injectable clock; nil → time.Now
}

// NewMaturityVerifier builds a verifier from a protocol→spec map. Empty/nil
// Specs yields a no-op verifier that always Skips.
func NewMaturityVerifier(specs map[string]MaturitySpec) *MaturityVerifier {
	return &MaturityVerifier{Specs: specs}
}

var _ Verifier = (*MaturityVerifier)(nil)

func (v *MaturityVerifier) Name() string        { return "maturity" }
func (v *MaturityVerifier) Tier() artifact.Tier { return artifact.TierChecksum }

func (v *MaturityVerifier) Verify(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	spec, ok := v.Specs[strings.ToLower(strings.TrimSpace(ref.Protocol))]
	if !ok || spec.MinAge <= 0 {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "maturity: not configured for protocol",
		}, nil
	}
	if ref.Mutable {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "maturity: skipped for mutable ref",
		}, nil
	}

	published, src, ok := resolvePublishTime(art)
	if !ok {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "maturity: no publish time available — skipped (honest)",
		}, nil
	}

	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	age := now.Sub(published)
	if age >= spec.MinAge {
		return artifact.Result{
			Status: artifact.StatusPass,
			Tier:   artifact.TierChecksum,
			Message: fmt.Sprintf("maturity: age %s >= min_age %s (%s)",
				age.Round(time.Second), spec.MinAge, src),
		}, nil
	}

	msg := fmt.Sprintf(
		"maturity: version too young — age %s < min_age %s (published %s via %s; policy gate, not cryptographic)",
		age.Round(time.Second), spec.MinAge, published.UTC().Format(time.RFC3339), src,
	)
	pol := spec.Policy
	if pol != MaturityEnforce {
		pol = MaturityWarn
	}
	if pol == MaturityEnforce {
		return artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierChecksum, Message: msg}, nil
	}
	return artifact.Result{Status: artifact.StatusWarn, Tier: artifact.TierChecksum, Message: msg}, nil
}

func resolvePublishTime(art *artifact.Artifact) (t time.Time, source string, ok bool) {
	if art == nil {
		return time.Time{}, "", false
	}
	if !art.Meta.PublishedAt.IsZero() {
		return art.Meta.PublishedAt.UTC(), "PublishedAt", true
	}
	if lm := strings.TrimSpace(art.Meta.LastModified); lm != "" {
		if parsed, err := http.ParseTime(lm); err == nil {
			return parsed.UTC(), "Last-Modified", true
		}
	}
	return time.Time{}, "", false
}
