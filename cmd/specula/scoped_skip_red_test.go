package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// A verifier scoped to one protocol must report StatusSkip — not StatusPass —
// for refs of any other protocol. StatusPass here manufactures presence in
// specula_verification_total: the counter would claim the check RAN and PASSED
// on every artifact of every protocol it was never configured for.
func TestProtocolScopedVerifier_OutOfScopeSkips(t *testing.T) {
	v := newProtocolScopedVerifier(stubVerifier{}, "pypi")
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{Protocol: "apt"}, nil)
	require.NoError(t, err)
	require.Equal(t, artifact.StatusSkip, res.Status,
		"out-of-scope protocol must SKIP, not PASS — PASS lies in the verification metric")
}

type stubVerifier struct{}

func (stubVerifier) Name() string        { return "consensus" }
func (stubVerifier) Tier() artifact.Tier { return artifact.TierConsensus }
func (stubVerifier) Verify(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error) {
	return artifact.Result{Status: artifact.StatusPass, Tier: artifact.TierConsensus}, nil
}
