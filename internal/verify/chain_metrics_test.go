package verify

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
)

// These tests assert against the REAL process-wide Prometheus counters in
// internal/metrics — never a fake recorder injected into the Chain. That is
// deliberate. A test that asserts "the counter went up" against a double the
// test itself wired is worthless: it proves the double works. The increments
// below can only come from Chain.Verify's own code path.
//
// The counters are process-global and other tests in this package move them, so
// every assertion is a before/after DELTA on a label combination made unique to
// its test via a distinct protocol name.

func chainCount(protocol, check, tier, result string) float64 {
	return testutil.ToFloat64(metrics.VerificationTotal.WithLabelValues(protocol, check, tier, result))
}

// stubVerifier is a verifier with a fixed, caller-chosen outcome. It is a stub
// for the VERIFIER, not for the metric: it lets a test state "a verifier that
// really ran and reached tier T" versus "a verifier that self-gated out", which
// is exactly the distinction under test. The metric it feeds is the real one.
type stubVerifier struct {
	name string
	tier artifact.Tier // the tier this verifier is CAPABLE of attesting
	res  artifact.Result
	err  error
}

func (s stubVerifier) Name() string        { return s.name }
func (s stubVerifier) Tier() artifact.Tier { return s.tier }
func (s stubVerifier) Verify(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error) {
	return s.res, s.err
}

// TestChainSkippedVerifierEmitsNoSeries is the load-bearing honesty test.
//
// A verifier that self-gated out reached NO tier and attests NOTHING. If Chain
// counted it, /metrics would report that the gpg check passed — at tier
// "checksum", result "pass" — on every npm tarball Specula ever served, because
// that is literally the Result value a self-gate used to carry.
func TestChainSkippedVerifierEmitsNoSeries(t *testing.T) {
	const proto = "test-skip-proto"

	// A gpg-like verifier that declines: not its protocol.
	skipper := stubVerifier{
		name: "gpg",
		tier: artifact.TierSigned,
		res: artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "gpg: skipped (protocol out of scope)",
		},
	}
	chain := NewChain(skipper)

	// Every label combination a skip could plausibly be miscounted into.
	before := map[string]float64{
		"checksum/pass": chainCount(proto, "gpg", "checksum", "pass"),
		"signed/pass":   chainCount(proto, "gpg", "signed", "pass"),
		"checksum/skip": chainCount(proto, "gpg", "checksum", "skip"),
		"signed/skip":   chainCount(proto, "gpg", "signed", "skip"),
	}

	res, err := chain.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: proto, Name: "lodash"},
		&artifact.Artifact{Digest: "sha256:abc"})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, "a skip must not fail the chain")

	assert.Equal(t, before["checksum/pass"], chainCount(proto, "gpg", "checksum", "pass"),
		"a SKIPPED gpg check must never be counted as a checksum-tier PASS — this is the exact lie the metric exists to prevent")
	assert.Equal(t, before["signed/pass"], chainCount(proto, "gpg", "signed", "pass"),
		"a skipped verifier must never be counted at the tier it is merely CAPABLE of")
	assert.Equal(t, before["checksum/skip"], chainCount(proto, "gpg", "checksum", "skip"),
		"a skip must emit no series at all, not even a result=skip one")
	assert.Equal(t, before["signed/skip"], chainCount(proto, "gpg", "signed", "skip"))
}

// TestChainRecordsTierActuallyReachedNotTierCapable proves the tier label comes
// from the Result the verifier produced, not from Verifier.Tier() (what it could
// attest at best) and not from anything asserted elsewhere.
func TestChainRecordsTierActuallyReachedNotTierCapable(t *testing.T) {
	const proto = "test-degrade-proto"

	// A verifier CAPABLE of signed that, on this artifact, only reached tofu —
	// e.g. helm with no .prov available. The honest label is tofu.
	degraded := stubVerifier{
		name: "helm-prov",
		tier: artifact.TierSigned, // capability
		res: artifact.Result{
			Status:  artifact.StatusWarn,
			Tier:    artifact.TierTofu, // what was ACTUALLY reached
			Message: "helm-prov: no .prov, degraded",
		},
	}
	chain := NewChain(degraded)

	signedBefore := chainCount(proto, "helm-prov", "signed", "warn")
	tofuBefore := chainCount(proto, "helm-prov", "tofu", "warn")

	res, err := chain.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: proto, Name: "nginx"},
		&artifact.Artifact{Digest: "sha256:abc"})
	require.NoError(t, err)

	assert.Equal(t, signedBefore, chainCount(proto, "helm-prov", "signed", "warn"),
		"must NOT label with the tier the verifier is capable of — that would claim signed for a degraded fetch")
	assert.Equal(t, tofuBefore+1, chainCount(proto, "helm-prov", "tofu", "warn"),
		"must label with the tier actually reached")

	// And the chain aggregate agrees with the Result handed to cache.Store.
	assert.Equal(t, artifact.TierTofu, res.Tier)
	assert.Equal(t, 1.0, chainCount(proto, metrics.CheckChain, "tofu", "warn"))
}

// TestChainMetricMatchesResultPersistedToDB is the unit-level half of the
// real-traffic cross-check: the chain-level series' tier must be the SAME value
// the Chain returns, because that is the value cache.Store persists to
// CacheEntry.Tier. If these two could drift, the metric and the DB could
// disagree and neither would be trustworthy.
func TestChainMetricMatchesResultPersistedToDB(t *testing.T) {
	const proto = "test-agree-proto"

	chain := NewChain(
		stubVerifier{name: "checksum", tier: artifact.TierChecksum,
			res: artifact.Result{Status: artifact.StatusPass, Tier: artifact.TierChecksum, Message: "ok"}},
		stubVerifier{name: "gpg", tier: artifact.TierSigned,
			res: artifact.Result{Status: artifact.StatusPass, Tier: artifact.TierSigned, Message: "verified"}},
	)

	before := chainCount(proto, metrics.CheckChain, "signed", "pass")
	res, err := chain.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: proto, Name: "nginx"},
		&artifact.Artifact{Digest: "sha256:abc"})
	require.NoError(t, err)

	// The Result that cache.Store will write to CacheEntry.Tier ...
	require.Equal(t, artifact.TierSigned, res.Tier)
	// ... is the same tier the chain-level series reports.
	assert.Equal(t, before+1, chainCount(proto, metrics.CheckChain, "signed", "pass"),
		"chain series tier must equal the Result.Tier persisted to the DB")

	// Each real verifier is also individually visible at its own tier.
	assert.Equal(t, 1.0, chainCount(proto, "checksum", "checksum", "pass"))
	assert.Equal(t, 1.0, chainCount(proto, "gpg", "signed", "pass"))
}

// TestChainRecordsFailure proves a failing verifier is counted at its own tier
// with result=fail, on both the per-check and chain series.
func TestChainRecordsFailure(t *testing.T) {
	const proto = "test-fail-proto"

	chain := NewChain(stubVerifier{
		name: "checksum", tier: artifact.TierChecksum,
		res: artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierChecksum, Message: "digest mismatch"},
	})

	res, err := chain.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: proto, Name: "evil"},
		&artifact.Artifact{Digest: "sha256:bad"})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusFail, res.Status)

	assert.Equal(t, 1.0, chainCount(proto, "checksum", "checksum", "fail"))
	assert.Equal(t, 1.0, chainCount(proto, metrics.CheckChain, "checksum", "fail"))
}

// TestChainEmptyEmitsNothing pins the documented decision that a chain with no
// verifiers — which dishonestly claims TierChecksum having compared no hash —
// contributes NO series, so the fabricated tier at least never reaches /metrics.
func TestChainEmptyEmitsNothing(t *testing.T) {
	const proto = "test-empty-proto"
	before := chainCount(proto, metrics.CheckChain, "checksum", "pass")

	res, err := NewChain().Verify(context.Background(),
		artifact.ArtifactRef{Protocol: proto}, &artifact.Artifact{})
	require.NoError(t, err)
	require.Equal(t, artifact.TierChecksum, res.Tier, "documented (dishonest) legacy behaviour")

	assert.Equal(t, before, chainCount(proto, metrics.CheckChain, "checksum", "pass"),
		"an empty chain verified nothing; it must not put a fabricated checksum tier on /metrics")
}
