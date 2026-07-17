package verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fake MirrorDigestFetcher
// ─────────────────────────────────────────────────────────────────────────────

// mirrorResponse holds the canned response for one named mirror.
type mirrorResponse struct {
	digest string
	err    error
	// delay simulates network latency to exercise parallel-fetch ordering.
	delay time.Duration
}

// fakeMirrorFetcher is a controllable, in-memory MirrorDigestFetcher.
// Results are keyed by ConsensusMirror.Name. An unconfigured mirror name
// returns an error (simulates a mirror not in scope for the test).
type fakeMirrorFetcher struct {
	responses map[string]mirrorResponse
}

func newFakeFetcher(responses map[string]mirrorResponse) *fakeMirrorFetcher {
	return &fakeMirrorFetcher{responses: responses}
}

func (f *fakeMirrorFetcher) FetchDigest(ctx context.Context, mirror ConsensusMirror, _ artifact.ArtifactRef) (string, error) {
	r, ok := f.responses[mirror.Name]
	if !ok {
		return "", errors.New("fakeMirrorFetcher: no response configured for mirror: " + mirror.Name)
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return r.digest, r.err
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	goodDigest  = "sha256:aabbccddee112233445566778899aabb"
	otherDigest = "sha256:ffeeddccbb9988776655443322110099"
)

func immutableRef(digest string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     "requests",
		Version:  "2.31.0",
		Digest:   digest,
		Mutable:  false,
	}
}

func mutableRef() artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "nginx",
		Version:  "latest",
		Mutable:  true,
	}
}

func artWith(digest string) *artifact.Artifact {
	return &artifact.Artifact{Path: "/quarantine/blob", Digest: digest, Size: 1024}
}

func mirrors(names ...string) []ConsensusMirror {
	ms := make([]ConsensusMirror, len(names))
	for i, n := range names {
		ms[i] = ConsensusMirror{Name: n, BaseURL: "https://" + n + ".example.com"}
	}
	return ms
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface
// ─────────────────────────────────────────────────────────────────────────────

func TestConsensusVerifier_Interface(t *testing.T) {
	v := NewConsensusVerifier(ConsensusConfig{Quorum: 1, Mirrors: mirrors("m1")}, nil)
	assert.Equal(t, "consensus", v.Name())
	assert.Equal(t, artifact.TierConsensus, v.Tier())
}

// ─────────────────────────────────────────────────────────────────────────────
// Self-gating
// ─────────────────────────────────────────────────────────────────────────────

func TestConsensusVerifier_MutableRef_Skipped(t *testing.T) {
	v := NewConsensusVerifier(ConsensusConfig{Quorum: 1, Mirrors: mirrors("m1")}, nil)
	res, err := v.Verify(context.Background(), mutableRef(), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusSkip, res.Status)
	assert.Equal(t, artifact.TierChecksum, res.Tier, "must self-gate at TierChecksum, not TierConsensus")
	assert.Contains(t, res.Message, "skipped")
}

func TestConsensusVerifier_EmptyArtDigest_Skipped(t *testing.T) {
	v := NewConsensusVerifier(ConsensusConfig{Quorum: 1, Mirrors: mirrors("m1")}, nil)
	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(""))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusSkip, res.Status)
	assert.Equal(t, artifact.TierChecksum, res.Tier)
	assert.Contains(t, res.Message, "skipped")
}

// ─────────────────────────────────────────────────────────────────────────────
// Nil fetcher: fail-closed
// ─────────────────────────────────────────────────────────────────────────────

func TestConsensusVerifier_NilFetcher_FailClosed(t *testing.T) {
	v := NewConsensusVerifier(ConsensusConfig{Quorum: 1, Mirrors: mirrors("m1")}, nil)
	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.Error(t, err, "nil fetcher must return an error so the chain fails closed")
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "fail-closed")
}

// ─────────────────────────────────────────────────────────────────────────────
// Quorum met
// ─────────────────────────────────────────────────────────────────────────────

func TestConsensusVerifier_QuorumMet_ThreeAgree(t *testing.T) {
	// 3 mirrors, all return goodDigest, quorum=2 → PASS at TierConsensus.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: goodDigest},
		"m2": {digest: goodDigest},
		"m3": {digest: goodDigest},
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2", "m3"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "agreed")
}

func TestConsensusVerifier_QuorumMet_ExactlyAtThreshold(t *testing.T) {
	// 3 mirrors: 2 agree, 1 down; quorum=2 → PASS.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: goodDigest},
		"m2": {digest: goodDigest},
		"m3": {err: errors.New("timeout")},
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2", "m3"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Quorum not met
// ─────────────────────────────────────────────────────────────────────────────

func TestConsensusVerifier_QuorumNotMet_TooFewAgree(t *testing.T) {
	// 3 mirrors: only 1 agrees (other 2 return different digests), quorum=2 → FAIL.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: goodDigest},
		"m2": {digest: otherDigest},
		"m3": {digest: otherDigest},
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2", "m3"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "quorum not met")
	assert.Contains(t, res.Message, "1/3")
}

func TestConsensusVerifier_QuorumNotMet_AllDown(t *testing.T) {
	// All mirrors unreachable → 0 agree, quorum=1 → FAIL.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {err: errors.New("connection refused")},
		"m2": {err: errors.New("connection refused")},
	})
	cfg := ConsensusConfig{
		Quorum:  1,
		Mirrors: mirrors("m1", "m2"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "quorum not met")
}

// ─────────────────────────────────────────────────────────────────────────────
// Single-mirror disagreement (poison mirror detected)
// ─────────────────────────────────────────────────────────────────────────────

// TestConsensusVerifier_PoisonMirrorDetected verifies that a single poisoned
// mirror returning a different digest does not block a PASS when the quorum is
// met by the remaining mirrors. The disagreement is surfaced in the message so
// operators can detect and investigate the anomaly.
func TestConsensusVerifier_PoisonMirrorDetected(t *testing.T) {
	// m1 and m2 agree; m3 is a poison mirror returning a different digest.
	// quorum=2 → PASS despite the single disagreement.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: goodDigest},
		"m2": {digest: goodDigest},
		"m3": {digest: otherDigest}, // poisoned / injected digest
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2", "m3"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, "quorum met despite one poisoned mirror")
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "agreed", "message should confirm quorum agreement")
}

// TestConsensusVerifier_PoisonMirrorCausesQuorumFailure verifies that a
// poison mirror that tips the agreement count below quorum causes a FAIL.
func TestConsensusVerifier_PoisonMirrorCausesQuorumFailure(t *testing.T) {
	// 2 mirrors: 1 agrees, 1 poisoned; quorum=2 → FAIL.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: goodDigest},
		"m2": {digest: otherDigest}, // poisoned
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status, "poisoned mirror breaks quorum")
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "quorum not met")
	assert.Contains(t, res.Message, "m2="+otherDigest, "disagreeing mirror must appear in the message")
}

// ─────────────────────────────────────────────────────────────────────────────
// Origin-check
// ─────────────────────────────────────────────────────────────────────────────

// TestConsensusVerifier_OriginCheck_Mismatch verifies that a reachable official
// source that returns a different digest causes an immediate FAIL regardless of
// mirror quorum (DESIGN-REVIEW §1.2 authoritative-witness rule).
func TestConsensusVerifier_OriginCheck_Mismatch(t *testing.T) {
	// Origin returns otherDigest (disagrees); mirrors m1+m2 agree with goodDigest.
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"origin": {digest: otherDigest}, // official source disagrees
		"m1":     {digest: goodDigest},
		"m2":     {digest: goodDigest},
	})
	cfg := ConsensusConfig{
		Quorum:  1,
		Mirrors: mirrors("m1", "m2"),
		OriginCheck: OriginCheck{
			URL: "https://pypi.org",
		},
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res.Status, "origin disagreement must fail closed")
	assert.Equal(t, artifact.TierConsensus, res.Tier)
	assert.Contains(t, res.Message, "OFFICIAL SOURCE disagrees")
	assert.Contains(t, res.Message, "possible mirror poisoning")
}

// TestConsensusVerifier_OriginCheck_Agreement verifies that an agreeing official
// source + quorum-met mirrors produce a PASS.
func TestConsensusVerifier_OriginCheck_Agreement(t *testing.T) {
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"origin": {digest: goodDigest},
		"m1":     {digest: goodDigest},
		"m2":     {digest: goodDigest},
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2"),
		OriginCheck: OriginCheck{
			URL: "https://pypi.org",
		},
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Equal(t, artifact.TierConsensus, res.Tier)
}

// TestConsensusVerifier_OriginCheck_Unreachable verifies that an unreachable
// official source is silently ignored (it does not vote), and mirror quorum alone
// governs the decision.
func TestConsensusVerifier_OriginCheck_Unreachable(t *testing.T) {
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"origin": {err: errors.New("connection refused")}, // official source is down
		"m1":     {digest: goodDigest},
		"m2":     {digest: goodDigest},
	})
	cfg := ConsensusConfig{
		Quorum:  2,
		Mirrors: mirrors("m1", "m2"),
		OriginCheck: OriginCheck{
			URL: "https://pypi.org",
		},
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, "unreachable origin must not veto quorum-met mirrors")
	assert.Equal(t, artifact.TierConsensus, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Prefix tolerance (digestsEqual helper)
// ─────────────────────────────────────────────────────────────────────────────

// TestConsensusVerifier_DigestsEqual_PrefixTolerance verifies that mirrors
// returning the bare hex (without "sha256:" prefix) still match an artifact
// digest that carries the full prefix.
func TestConsensusVerifier_DigestsEqual_PrefixTolerance(t *testing.T) {
	const bare = "aabbccddee112233445566778899aabb"
	const prefixed = "sha256:" + bare

	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"m1": {digest: bare}, // returns bare hex, artifact has full sha256:... prefix
	})
	cfg := ConsensusConfig{
		Quorum:  1,
		Mirrors: mirrors("m1"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	res, err := v.Verify(context.Background(), immutableRef(prefixed), artWith(prefixed))

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, "bare hex should match prefixed digest")
	assert.Equal(t, artifact.TierConsensus, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Parallel fetch: staggered latency must not affect correctness
// ─────────────────────────────────────────────────────────────────────────────

// TestConsensusVerifier_ParallelFetch_CorrectUnderLatency runs 4 mirrors with
// staggered artificial delays to confirm that the parallel goroutines collect
// all results correctly regardless of arrival order.
func TestConsensusVerifier_ParallelFetch_CorrectUnderLatency(t *testing.T) {
	fetcher := newFakeFetcher(map[string]mirrorResponse{
		"fast":   {digest: goodDigest, delay: 5 * time.Millisecond},
		"medium": {digest: goodDigest, delay: 20 * time.Millisecond},
		"slow":   {digest: goodDigest, delay: 50 * time.Millisecond},
		"poison": {digest: otherDigest, delay: 10 * time.Millisecond},
	})
	cfg := ConsensusConfig{
		Quorum:  3,
		Mirrors: mirrors("fast", "medium", "slow", "poison"),
	}
	v := NewConsensusVerifier(cfg, fetcher)

	start := time.Now()
	res, err := v.Verify(context.Background(), immutableRef(goodDigest), artWith(goodDigest))
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status, "3 out of 4 should meet quorum=3")
	assert.Equal(t, artifact.TierConsensus, res.Tier)

	// Sanity: parallel fetch should complete well under the sum of all delays
	// (~85 ms sequential). Allow generous margin for CI slowness.
	assert.Less(t, elapsed, 500*time.Millisecond, "parallel fetch should not be slower than sequential sum")
}

// ─────────────────────────────────────────────────────────────────────────────
// digestsEqual unit tests (internal helper)
// ─────────────────────────────────────────────────────────────────────────────

func TestDigestsEqual(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"sha256:abc", "sha256:abc", true},
		{"sha256:abc", "abc", true}, // bare hex matches prefixed
		{"abc", "sha256:abc", true}, // reverse
		{"abc", "abc", true},        // both bare
		{"sha256:abc", "sha256:def", false},
		{"sha256:abc", "def", false},
		{"", "sha256:abc", false}, // empty is never equal
		{"sha256:abc", "", false},
		{"", "", false},
	}

	for _, tc := range tests {
		got := digestsEqual(tc.a, tc.b)
		assert.Equal(t, tc.want, got, "digestsEqual(%q, %q)", tc.a, tc.b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// refKey unit test
// ─────────────────────────────────────────────────────────────────────────────

func TestRefKey(t *testing.T) {
	ref := artifact.ArtifactRef{Protocol: "pypi", Name: "requests", Version: "2.31.0"}
	assert.Equal(t, "pypi:requests@2.31.0", refKey(ref))
}
