package artifact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTierString(t *testing.T) {
	require.Equal(t, "signed", TierSigned.String())
	require.Equal(t, "consensus", TierConsensus.String())
	require.Equal(t, "tofu", TierTofu.String())
	require.Equal(t, "checksum", TierChecksum.String())
}

func TestTierOrdering(t *testing.T) {
	require.Greater(t, int(TierSigned), int(TierConsensus))
	require.Greater(t, int(TierConsensus), int(TierTofu))
	require.Greater(t, int(TierTofu), int(TierChecksum))
}

func TestStatusString(t *testing.T) {
	require.Equal(t, "pass", StatusPass.String())
	require.Equal(t, "warn", StatusWarn.String())
	require.Equal(t, "fail", StatusFail.String())
	require.Equal(t, "skip", StatusSkip.String())
}

// TestStatusSkipIsDistinct guards the honesty property StatusSkip exists for:
// "this check did not run" must never render as, or compare equal to, "this
// check passed". specula_verification_total{check,tier,result} depends on the
// distinction — collapsing them would report that the gpg check passed on every
// npm tarball Specula ever served.
func TestStatusSkipIsDistinct(t *testing.T) {
	require.NotEqual(t, StatusPass, StatusSkip, "a skip is not a weak pass")
	require.NotEqual(t, StatusPass.String(), StatusSkip.String())

	// All four statuses must render distinctly.
	seen := map[string]bool{}
	for _, s := range []Status{StatusPass, StatusWarn, StatusFail, StatusSkip} {
		l := s.String()
		require.False(t, seen[l], "status label %q is emitted for two different statuses", l)
		seen[l] = true
	}
}

// TestStringFallbacks covers the out-of-range branches. "unknown" is the honest
// answer for a value outside the enum — and note it is NOT one of PRD §G2's four
// tiers, which is exactly why metrics must only ever be handed a tier the verify
// chain actually produced, never an arbitrary int.
func TestStringFallbacks(t *testing.T) {
	require.Equal(t, "unknown", Tier(99).String())
	require.Equal(t, "unknown", Tier(-1).String())
	require.Equal(t, "unknown", Status(99).String())
	require.Equal(t, "unknown", Status(-1).String())
}
