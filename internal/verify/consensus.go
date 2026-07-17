package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// MirrorDigestFetcher fetches JUST the digest/manifest identity of an artifact
// from ONE independent upstream mirror — an HTTP HEAD or metadata read, NEVER
// the full blob (DESIGN-REVIEW §1.2: "只比 digest/manifest，不下载全 blob").
//
// It is the injection seam for ConsensusVerifier: production wraps the
// upstream.Client HEAD/metadata path (one request per mirror, no body); tests
// supply an in-memory fake. Keeping it an interface lets the pure quorum logic
// below be unit-tested without any network.
type MirrorDigestFetcher interface {
	// FetchDigest returns the digest the named mirror advertises for ref
	// (e.g. "sha256:...") WITHOUT downloading the blob. An error means the
	// mirror could not be consulted (down/timeout/404); such a mirror simply
	// does not contribute a vote — it is never counted as agreement.
	FetchDigest(ctx context.Context, mirror ConsensusMirror, ref artifact.ArtifactRef) (string, error)
}

// ConsensusMirror is one independent upstream consulted for a digest. Mirrors
// must be genuinely independent (distinct CDN/operator) for the quorum to raise
// the attacker's bar — N copies of the same origin is not consensus.
type ConsensusMirror struct {
	Name    string // logical mirror name (used in messages)
	BaseURL string // mirror base URL
}

// OriginCheck optionally consults the OFFICIAL source directly (through a
// configured egress proxy) as an authoritative witness (DESIGN-REVIEW §1.2
// "官方源比对"). A disagreement with the official source is fatal regardless of
// mirror quorum. An empty URL disables the origin check.
type OriginCheck struct {
	// URL is the official source base (pypi.org / registry.npmjs.org /
	// registry-1.docker.io). Empty disables origin-check.
	URL string
	// ViaProxy is the egress HTTP proxy URL used to reach the official source
	// from within a restricted network. Empty means reach it directly. The
	// production MirrorDigestFetcher implementation is responsible for routing
	// the "origin" mirror through this proxy; the verifier passes it through
	// the OriginCheck config and the fetcher reads it from there.
	ViaProxy string
}

// ConsensusConfig is the runtime configuration for ConsensusVerifier (mapped
// from config.ConsensusConfig by the wiring layer).
type ConsensusConfig struct {
	// Quorum is the minimum number of independent mirrors that must agree on the
	// artifact's digest for a PASS. Must be >= 1.
	Quorum int
	// Mirrors is the set of independent mirrors to poll for a digest.
	Mirrors []ConsensusMirror
	// OriginCheck optionally adds the official source as an authoritative witness.
	OriginCheck OriginCheck
}

// ConsensusVerifier attests the cross-source consensus tier (TierConsensus,
// DESIGN-REVIEW §1.2): it fetches ONLY the digest/manifest from N independent
// mirrors in parallel and PASSes when >= Quorum of them agree with the digest
// computed for the quarantined artifact. When an official source is configured
// it acts as an authoritative witness — any disagreement there fails closed.
//
// This is NOT cryptographic authenticity: it raises the bar so an attacker must
// consistently poison every configured mirror to pass, and is the strongest
// available protection for unsigned ecosystems (npm/PyPI in CN).
//
// # Self-gating
//
// Verify returns StatusSkip when it has nothing to do:
//   - a mutable ref (tag/index/ref) whose digest is not yet resolved, or
//   - the artifact carries no computed digest.
//
// so a single global chain may include it without acting on artifacts the
// consensus tier does not apply to.
//
// # Injection
//
// The digest fetcher is injected (MirrorDigestFetcher). Until a production
// fetcher is wired, a nil fetcher makes Verify fail closed (errNotImplemented)
// so the chain never silently attests consensus it did not actually check.
type ConsensusVerifier struct {
	cfg     ConsensusConfig
	fetcher MirrorDigestFetcher
}

// NewConsensusVerifier constructs a ConsensusVerifier from its runtime config
// and an injected per-mirror digest fetcher (nil is allowed for wiring skeletons
// but makes Verify fail closed).
func NewConsensusVerifier(cfg ConsensusConfig, fetcher MirrorDigestFetcher) *ConsensusVerifier {
	return &ConsensusVerifier{cfg: cfg, fetcher: fetcher}
}

// Compile-time assertion that ConsensusVerifier satisfies Verifier.
var _ Verifier = (*ConsensusVerifier)(nil)

func (v *ConsensusVerifier) Name() string        { return "consensus" }
func (v *ConsensusVerifier) Tier() artifact.Tier { return artifact.TierConsensus }

// mirrorFetchResult is the outcome of a single parallel mirror fetch.
type mirrorFetchResult struct {
	mirror   ConsensusMirror
	digest   string
	err      error
	isOrigin bool
}

// Verify polls the configured mirrors (and optional official source) for the
// artifact's digest and applies the quorum rule. All mirror fetches (including
// the optional origin check) are issued in parallel; results are aggregated
// after all goroutines complete.
//
//   - Skipped (StatusSkip) for mutable/undigested refs.
//   - Fail-closed (error) when no digest fetcher is wired.
//   - StatusFail when the official source disagrees, or fewer than Quorum
//     independent mirrors agree with the artifact digest.
//   - StatusPass (TierConsensus) when >= Quorum mirrors agree and the official
//     source (if configured and reachable) agrees.
func (v *ConsensusVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Self-gate: consensus only applies to a resolved, immutable digest.
	if ref.Mutable || art.Digest == "" {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "consensus: skipped (no resolved digest to cross-check)",
		}, nil
	}

	// Fail closed if the injection seam is not wired — never attest unchecked.
	if v.fetcher == nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierConsensus,
			Message: "consensus: no MirrorDigestFetcher wired (fail-closed)",
		}, fmt.Errorf("consensus: %w (MirrorDigestFetcher is nil)", errNotImplemented)
	}

	quorum := v.cfg.Quorum
	if quorum < 1 {
		quorum = 1
	}

	hasOrigin := v.cfg.OriginCheck.URL != ""

	// Launch all fetches in parallel: origin (if configured) + every mirror.
	// Using a buffered channel the size of all concurrent fetches so goroutines
	// never block on send regardless of receive order.
	total := len(v.cfg.Mirrors)
	if hasOrigin {
		total++
	}
	resultCh := make(chan mirrorFetchResult, total)

	// Origin check goroutine — the official source is an authoritative witness.
	// The production fetcher is responsible for routing through ViaProxy; the
	// verifier passes OriginCheck.URL as the BaseURL of a synthetic mirror named
	// "origin".
	if hasOrigin {
		originMirror := ConsensusMirror{Name: "origin", BaseURL: v.cfg.OriginCheck.URL}
		go func() {
			got, err := v.fetcher.FetchDigest(ctx, originMirror, ref)
			resultCh <- mirrorFetchResult{mirror: originMirror, digest: got, err: err, isOrigin: true}
		}()
	}

	// Per-mirror goroutines — each mirror is polled independently.
	for _, m := range v.cfg.Mirrors {
		m := m // capture for goroutine
		go func() {
			got, err := v.fetcher.FetchDigest(ctx, m, ref)
			resultCh <- mirrorFetchResult{mirror: m, digest: got, err: err, isOrigin: false}
		}()
	}

	// Collect all results from the channel (all goroutines write before
	// returning, so this drains exactly `total` items without a WaitGroup).
	var (
		originDigest    string
		originReachable bool

		agree         int
		polled        int
		disagreements []string
	)

	for i := 0; i < total; i++ {
		r := <-resultCh
		if r.isOrigin {
			if r.err == nil {
				originReachable = true
				originDigest = r.digest
			}
			// Unreachable origin is not a vote — mirror quorum governs.
			continue
		}
		// Mirror result: down/error means no vote.
		if r.err != nil {
			continue
		}
		polled++
		if digestsEqual(r.digest, art.Digest) {
			agree++
		} else {
			disagreements = append(disagreements, fmt.Sprintf("%s=%s", r.mirror.Name, r.digest))
		}
	}

	// Origin-check decision: the official source is authoritative. A reachable
	// disagreement is fatal regardless of mirror quorum (DESIGN-REVIEW §1.2
	// "官方源比对"). An unreachable origin is silently skipped.
	if hasOrigin && originReachable && !digestsEqual(originDigest, art.Digest) {
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: OFFICIAL SOURCE disagrees for %s: origin=%s artifact=%s — possible mirror poisoning",
				refKey(ref), originDigest, art.Digest,
			),
		}, nil
	}

	// Quorum decision: at least Quorum independent mirrors must agree.
	if agree < quorum {
		disagreeStr := strings.Join(disagreements, ", ")
		if disagreeStr == "" {
			disagreeStr = "none"
		}
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: quorum not met for %s: %d/%d mirrors agreed on %s (need %d; polled %d; disagreements: %s)",
				refKey(ref), agree, len(v.cfg.Mirrors), art.Digest, quorum, polled, disagreeStr,
			),
		}, nil
	}

	return artifact.Result{
		Status: artifact.StatusPass,
		Tier:   artifact.TierConsensus,
		Message: fmt.Sprintf(
			"consensus: %d/%d mirrors agreed on %s for %s (quorum %d)",
			agree, len(v.cfg.Mirrors), art.Digest, refKey(ref), quorum,
		),
	}, nil
}

// digestsEqual compares two digest strings, tolerating an absent "sha256:"
// algorithm prefix on either side (some HEAD/metadata paths return the bare hex).
func digestsEqual(a, b string) bool {
	return strings.TrimPrefix(a, "sha256:") == strings.TrimPrefix(b, "sha256:") && a != "" && b != ""
}

// refKey renders a compact identity for messages.
func refKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + "@" + ref.Version
}
