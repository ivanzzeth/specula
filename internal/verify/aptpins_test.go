package verify

// Tests for the apt pin store's KEY and LIFETIME rules — the two places where a
// wrong answer is a trust bug rather than a 502.
//
// Every test runs against the REAL sqlite-backed store (real goose schema), for
// the reason given on newSharedPinStore.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Key: trust-anchor scoping
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_PinsAreScopedToTrustAnchor is the trust-bug guard the task
// calls out: a pin must not be readable by a verifier anchored on a DIFFERENT
// keyring. Otherwise one repo's InRelease would vouch for another repo's bytes —
// far worse than a 502.
//
// Instance A (keyring K1) verifies a full chain and pins the .deb. Instance B
// shares the same store but is anchored on K2. It must refuse: nothing K2 trusts
// has ever said anything about those bytes.
func TestGPGVerifier_PinsAreScopedToTrustAnchor(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)

	vA, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, vA, "ubuntu")

	// A different distro keyring, same store.
	otherKey := newAptTestKey(t)
	vB, err := NewGPGVerifier(otherKey.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	res := f.downloadDeb(t, vB)

	assert.Equal(t, artifact.StatusFail, res.Status,
		"a pin made under keyring K1 must NOT be honoured by a verifier anchored on K2: %s", res.Message)
}

// TestKeyringScope_StableAndDistinct pins the two properties the scope key must
// have to be usable at all: identical for two verifiers loading the same keyring
// (or the chain could never span replicas), and different for different anchors
// (or the scoping above is decorative).
func TestKeyringScope_StableAndDistinct(t *testing.T) {
	k1 := newAptTestKey(t)
	k2 := newAptTestKey(t)

	a, err := NewGPGVerifier(k1.keyFile)
	require.NoError(t, err)
	b, err := NewGPGVerifier(k1.keyFile)
	require.NoError(t, err)
	c, err := NewGPGVerifier(k2.keyFile)
	require.NoError(t, err)

	assert.Equal(t, a.scope, b.scope,
		"two instances loading the SAME keyring must derive the same scope — otherwise no pin ever crosses replicas")
	assert.NotEqual(t, a.scope, c.scope,
		"different keyrings must derive different scopes — otherwise anchors share a pin namespace")
	assert.NotEmpty(t, a.scope)
}

// ─────────────────────────────────────────────────────────────────────────────
// Lifetime: what a new InRelease does to old pins
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_SupersededInRelease_ReplacesIndexPins asserts index pins are
// REPLACED, not merged, when a newer InRelease is verified for a suite.
//
// Why replace: InRelease is the mutable-tier root of its suite (ARCHITECTURE §3).
// If old index pins lingered, a superseded-but-genuine signed index could be
// served at `signed` forever — the index-rollback vector PRD §G2 assigns to
// anti-rollback.
func TestGPGVerifier_SupersededInRelease_ReplacesIndexPins(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)
	ctx := context.Background()

	v, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, v, "ubuntu")

	// A new InRelease for the same suite that no longer lists the old Packages.
	newPackages := buildPackagesContent("pool/main/n/new/new_2.0_amd64.deb", sha256Hex([]byte("new deb")))
	newInRelease := signInRelease(t, f.key, []string{
		sha256Hex(newPackages) + " " + itoa(len(newPackages)) + " " + f.packagesRel + ".new",
	})
	irPath, irDigest := writeQuarantine(t, newInRelease)
	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: f.suite + "/InRelease", Mutable: true,
	}, &artifact.Artifact{Path: irPath, Digest: irDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status)

	// The OLD Packages index must no longer be chain-verifiable: the newest
	// signed InRelease does not list it.
	pkgPath, pkgDigest := writeQuarantine(t, f.packagesRaw)
	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: f.suite + "/" + f.packagesRel, Mutable: true,
	}, &artifact.Artifact{Path: pkgPath, Digest: pkgDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusFail, res.Status,
		"a Packages index the newest InRelease no longer lists must not verify: %s", res.Message)
	assert.Contains(t, res.Message, "not listed in InRelease",
		"failure must be 'not listed' — proving the old pin was REPLACED, not merged: %s", res.Message)
}

// TestGPGVerifier_SupersededInRelease_KeepsPoolPins is the other half of the
// invalidation rule, and the one that keeps the bug fixed.
//
// A pool object is immutable-tier: its path embeds version + architecture, so it
// denotes one byte sequence forever. The pin is a signed statement about those
// bytes, and a newer InRelease does not RETRACT it — a path leaving the index
// means the package left the suite, not that its bytes were wrong. apt relies on
// this: a client whose Packages list is still valid legitimately asks for pool
// paths the newest index no longer lists. Expiring pool pins on rotation would
// reintroduce the 502 this whole change exists to fix.
func TestGPGVerifier_SupersededInRelease_KeepsPoolPins(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)
	ctx := context.Background()

	v, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, v, "ubuntu")

	// Rotate InRelease: same suite, completely different contents.
	newPackages := buildPackagesContent("pool/main/n/new/new_2.0_amd64.deb", sha256Hex([]byte("new deb")))
	newInRelease := signInRelease(t, f.key, []string{
		sha256Hex(newPackages) + " " + itoa(len(newPackages)) + " " + f.packagesRel + ".new",
	})
	irPath, irDigest := writeQuarantine(t, newInRelease)
	_, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: f.suite + "/InRelease", Mutable: true,
	}, &artifact.Artifact{Path: irPath, Digest: irDigest})
	require.NoError(t, err)

	// The .deb pinned by the SUPERSEDED InRelease must still reach signed.
	res := f.downloadDeb(t, v)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"a .deb pinned by a superseded-but-genuine InRelease must still serve: %s", res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"and it must still be TierSigned — the distro signature over those bytes has not been retracted: %s", res.Message)
}

// TestGPGVerifier_RepinnedPoolPath_LatestSignedWins asserts that when a repo
// re-pins one pool path to different bytes, the newest signed statement is the
// authority and previously-pinned bytes then fail closed.
func TestGPGVerifier_RepinnedPoolPath_LatestSignedWins(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)
	ctx := context.Background()

	v, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, v, "ubuntu")

	// Same pool path, different bytes, freshly signed.
	newDeb := []byte("rebuilt payload at the same pool path")
	newPackages := buildPackagesContent(f.poolPath, sha256Hex(newDeb))
	newInRelease := signInRelease(t, f.key, []string{
		sha256Hex(newPackages) + " " + itoa(len(newPackages)) + " " + f.packagesRel,
	})
	irPath, irDigest := writeQuarantine(t, newInRelease)
	_, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: f.suite + "/InRelease", Mutable: true,
	}, &artifact.Artifact{Path: irPath, Digest: irDigest})
	require.NoError(t, err)
	pkgPath, pkgDigest := writeQuarantine(t, newPackages)
	_, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: f.suite + "/" + f.packagesRel, Mutable: true,
	}, &artifact.Artifact{Path: pkgPath, Digest: pkgDigest})
	require.NoError(t, err)

	// New bytes verify against the new pin.
	debPath, debDigest := writeQuarantine(t, newDeb)
	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: f.poolDir, Version: f.poolFile, Mutable: false,
	}, &artifact.Artifact{Path: debPath, Digest: debDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"the newest signed pin must be honoured: %s", res.Message)

	// The OLD bytes at that path must now fail closed.
	res = f.downloadDeb(t, v)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"bytes matching only a superseded pin for a RE-PINNED path must fail closed: %s", res.Message)
}

// ─────────────────────────────────────────────────────────────────────────────
// Ambiguity: fail closed rather than guess
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_AmbiguousPoolPin_FailsClosed covers the case a pool ref cannot
// disambiguate: two repositories under the SAME trust anchor pinning one pool
// path to different hashes. A pool ref carries no repo prefix, so there is no
// basis to choose — and choosing would let one repo's InRelease vouch for
// another's bytes. Fail closed.
func TestGPGVerifier_AmbiguousPoolPin_FailsClosed(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)
	ctx := context.Background()

	v, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	// Repo "ubuntu" pins the path to the fixture's bytes.
	f.ingestUpdate(t, v, "ubuntu")

	// Repo "other", same anchor, pins the SAME path to different bytes.
	require.NoError(t, store.PutPoolPins(ctx, v.scope, "other", map[string]string{
		f.poolPath: sha256Hex([]byte("a different payload entirely")),
	}))

	res := f.downloadDeb(t, v)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"conflicting pins for one pool path must fail closed, never pick a winner: %s", res.Message)
	assert.True(t, strings.Contains(res.Message, "trustworthy pin"),
		"the message must name the ambiguity, not masquerade as a missing pin: %s", res.Message)
}

// TestGPGVerifier_AgreeingPoolPins_AcrossRepos_Resolve is the flip side: two
// repositories pinning the same path to the SAME hash is not a conflict. This is
// the normal case when one apt mount is reachable under two URL prefixes, and it
// must not fail closed.
func TestGPGVerifier_AgreeingPoolPins_AcrossRepos_Resolve(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)
	ctx := context.Background()

	v, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	f.ingestUpdate(t, v, "ubuntu")
	require.NoError(t, store.PutPoolPins(ctx, v.scope, "mirror-alias", map[string]string{
		f.poolPath: sha256Hex(f.debRaw),
	}))

	res := f.downloadDeb(t, v)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"two repos agreeing on a hash is not ambiguity: %s", res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)
}
