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
}
