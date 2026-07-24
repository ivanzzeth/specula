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
	// IdentityMode selects what mirrors vote on:
	//   IdentityCAS (default) — votes must match art.Digest (sha256 CAS; OCI/PyPI)
	//   IdentityContentID — mirrors quorum on an advertised content ID (npm
	//     integrity / cargo cksum), then the quarantine body is bound to that ID.
	//     Never compares sha512 integrity to art.Digest.
	IdentityMode string
}

// ConsensusVerifier attests the cross-source consensus tier (TierConsensus,
// DESIGN-REVIEW §1.2): it fetches ONLY metadata identities from N independent
// mirrors in parallel. In CAS mode it PASSes when >= Quorum mirrors agree with
// art.Digest. In Content-ID mode it PASSes when >= Quorum mirrors agree on the
// same advertised ID and the quarantine body matches that ID (SSRI / checksum).
// When an official source is configured it acts as an authoritative witness —
// any disagreement there fails closed.
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
// artifact's identity and applies the quorum rule. All mirror fetches (including
// the optional origin check) are issued in parallel; results are aggregated
// after all goroutines complete.
//
//   - Skipped (StatusSkip) for mutable/undigested refs.
//   - Fail-closed (error) when no digest fetcher is wired.
//   - StatusFail when the official source disagrees, body/content-id bind fails,
//     or fewer than Quorum independent mirrors agree.
//   - StatusPass (TierConsensus) when quorum (and origin/body bind) succeed.
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
	total := len(v.cfg.Mirrors)
	if hasOrigin {
		total++
	}
	resultCh := make(chan mirrorFetchResult, total)

	if hasOrigin {
		originMirror := ConsensusMirror{Name: "origin", BaseURL: v.cfg.OriginCheck.URL}
		go func() {
			got, err := v.fetcher.FetchDigest(ctx, originMirror, ref)
			resultCh <- mirrorFetchResult{mirror: originMirror, digest: got, err: err, isOrigin: true}
		}()
	}

	for _, m := range v.cfg.Mirrors {
		m := m
		go func() {
			got, err := v.fetcher.FetchDigest(ctx, m, ref)
			resultCh <- mirrorFetchResult{mirror: m, digest: got, err: err, isOrigin: false}
		}()
	}

	var (
		originDigest    string
		originReachable bool
		votes           []mirrorFetchResult
	)
	for i := 0; i < total; i++ {
		r := <-resultCh
		if r.isOrigin {
			if r.err == nil {
				originReachable = true
				originDigest = r.digest
			}
			continue
		}
		if r.err != nil {
			continue
		}
		votes = append(votes, r)
	}

	if v.cfg.IdentityMode == IdentityContentID {
		return v.verifyContentIDQuorum(ref, art, votes, quorum, hasOrigin, originReachable, originDigest)
	}
	return v.verifyCASQuorum(ref, art, votes, quorum, hasOrigin, originReachable, originDigest)
}

// verifyCASQuorum is the OCI/PyPI path: mirror digests must match art.Digest
// (streaming sha256). digestsEqual only normalizes the sha256: prefix — it must
// never be used to equate npm sha512 integrity with CAS digests.
func (v *ConsensusVerifier) verifyCASQuorum(
	ref artifact.ArtifactRef,
	art *artifact.Artifact,
	votes []mirrorFetchResult,
	quorum int,
	hasOrigin, originReachable bool,
	originDigest string,
) (artifact.Result, error) {
	agree := 0
	polled := len(votes)
	var disagreements []string
	for _, r := range votes {
		if digestsEqual(r.digest, art.Digest) {
			agree++
		} else {
			disagreements = append(disagreements, fmt.Sprintf("%s=%s", r.mirror.Name, r.digest))
		}
	}

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

// verifyContentIDQuorum is the npm/cargo path: mirrors must agree on an
// advertised content ID among themselves; the body must match that ID; origin
// (if reachable) must match. art.Digest (CAS sha256) is never used as the
// comparison target.
func (v *ConsensusVerifier) verifyContentIDQuorum(
	ref artifact.ArtifactRef,
	art *artifact.Artifact,
	votes []mirrorFetchResult,
	quorum int,
	hasOrigin, originReachable bool,
	originDigest string,
) (artifact.Result, error) {
	counts := map[string]int{}
	voters := map[string][]string{}
	for _, r := range votes {
		id := r.digest
		counts[id]++
		voters[id] = append(voters[id], r.mirror.Name)
	}

	var candidates []string
	for id, n := range counts {
		if n >= quorum {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		detail := make([]string, 0, len(votes))
		for _, r := range votes {
			detail = append(detail, fmt.Sprintf("%s=%s", r.mirror.Name, r.digest))
		}
		if len(detail) == 0 {
			detail = []string{"none"}
		}
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: content-id quorum not met for %s: need %d agreeing mirrors; polled %d (%s)",
				refKey(ref), quorum, len(votes), strings.Join(detail, ", "),
			),
		}, nil
	}
	if len(candidates) > 1 {
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: content-id split brain for %s: multiple IDs met quorum %d (%s)",
				refKey(ref), quorum, strings.Join(candidates, ", "),
			),
		}, nil
	}
	winning := candidates[0]

	if hasOrigin && originReachable && !contentIDsEqual(originDigest, winning) {
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: OFFICIAL SOURCE disagrees for %s: origin=%s content-id=%s — possible mirror poisoning",
				refKey(ref), originDigest, winning,
			),
		}, nil
	}

	if err := verifyBodyContentID(art.Path, winning); err != nil {
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierConsensus,
			Message: fmt.Sprintf(
				"consensus: body does not match content-id %s for %s: %v",
				winning, refKey(ref), err,
			),
		}, nil
	}

	return artifact.Result{
		Status: artifact.StatusPass,
		Tier:   artifact.TierConsensus,
		Message: fmt.Sprintf(
			"consensus: %d/%d mirrors agreed on integrity=%s for %s (voters: %s; quorum %d; cas=%s)",
			counts[winning], len(v.cfg.Mirrors), winning, refKey(ref),
			strings.Join(voters[winning], ","), quorum, art.Digest,
		),
	}, nil
}

// digestsEqual compares two CAS sha256 digest strings, tolerating an absent
// "sha256:" algorithm prefix on either side. It must NOT be used for npm SSRI
// (sha512-…) or other non-sha256 content IDs.
func digestsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	// Refuse to "equal" SSRI-form identities via sha256: stripping — that would
	// let a sha512 integrity accidentally compare equal to a CAS digest.
	if strings.Contains(a, "-") && !strings.HasPrefix(a, "sha256:") {
		return false
	}
	if strings.Contains(b, "-") && !strings.HasPrefix(b, "sha256:") {
		return false
	}
	return strings.TrimPrefix(a, "sha256:") == strings.TrimPrefix(b, "sha256:")
}

// refKey renders a compact identity for messages.
func refKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + "@" + ref.Version
}
