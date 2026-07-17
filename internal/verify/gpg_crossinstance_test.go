package verify

// Cross-instance / restart-survival tests for the apt GPG chain.
//
// Traceable to:
//   - PRD §G3 "Specula 实例无状态" — shared state lives ONLY in the blob store +
//     metadata DB. No gossip, no leader election. Two replicas behind a load
//     balancer must be interchangeable.
//   - DESIGN-REVIEW §1.1 — the apt gold-standard chain
//     InRelease → Packages index → pool .deb.
//
// The chain's pinned hashes are REQUIRED CHAIN STATE, not a cache: without them
// a pool .deb cannot reach TierSigned at all. Holding them in one process's heap
// therefore contradicts §G3 — apt breaks in exactly the topology §G3 specifies:
// replica A serves `apt-get update`, replica B receives the `.deb` request.
//
// These tests deliberately use the REAL sqlite MetadataStore, never a hand-rolled
// double: a double that answers whatever the code asks would hide precisely the
// bug under test.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/aptpins"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// aptChainFixture is one coherent, signed apt repository: an InRelease that pins
// a Packages index, which in turn pins a pool .deb.
type aptChainFixture struct {
	key          *aptTestKey
	suite        string
	packagesRel  string
	poolDir      string
	poolFile     string
	poolPath     string
	inReleaseRaw []byte
	packagesRaw  []byte
	debRaw       []byte
}

// newAptChainFixture builds a real three-link chain over freshly generated keys.
func newAptChainFixture(t *testing.T) *aptChainFixture {
	t.Helper()
	key := newAptTestKey(t)

	const (
		suite       = "noble"
		packagesRel = "main/binary-amd64/Packages"
		poolDir     = "main/h/hello"
		poolFile    = "hello_1.0.0_amd64.deb"
	)
	poolPath := "pool/" + poolDir + "/" + poolFile

	deb := []byte("fake .deb payload — the bytes the client actually wants")
	packages := buildPackagesContent(poolPath, sha256Hex(deb))
	inRelease := signInRelease(t, key, []string{
		sha256Hex(packages) + " " + itoa(len(packages)) + " " + packagesRel,
	})

	return &aptChainFixture{
		key:          key,
		suite:        suite,
		packagesRel:  packagesRel,
		poolDir:      poolDir,
		poolFile:     poolFile,
		poolPath:     poolPath,
		inReleaseRaw: inRelease,
		packagesRaw:  packages,
		debRaw:       deb,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ingestUpdate replays what `apt-get update` drives through the verify-on-write
// pipeline on whichever replica happens to serve it: InRelease, then Packages.
func (f *aptChainFixture) ingestUpdate(t *testing.T, v *GPGVerifier, repo string) {
	t.Helper()
	ctx := context.Background()

	irPath, irDigest := writeQuarantine(t, f.inReleaseRaw)
	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: repo, Version: f.suite + "/InRelease", Mutable: true,
	}, &artifact.Artifact{Path: irPath, Digest: irDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, "InRelease must verify: %s", res.Message)

	pkgPath, pkgDigest := writeQuarantine(t, f.packagesRaw)
	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: repo, Version: f.suite + "/" + f.packagesRel, Mutable: true,
	}, &artifact.Artifact{Path: pkgPath, Digest: pkgDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, "Packages must verify: %s", res.Message)
}

// downloadDeb replays `apt-get download <pkg>`: an immutable pool ref arriving at
// whichever replica the load balancer picked.
func (f *aptChainFixture) downloadDeb(t *testing.T, v *GPGVerifier) artifact.Result {
	t.Helper()
	debPath, debDigest := writeQuarantine(t, f.debRaw)
	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: f.poolDir, Version: f.poolFile, Mutable: false,
	}, &artifact.Artifact{Path: debPath, Digest: debDigest})
	require.NoError(t, err)
	return res
}

// newSharedPinStore opens a REAL sqlite-backed pin store in a temp dir, schema
// created by the real goose migration. This is the "shared state lives only in
// the DB" substrate of PRD §G3.
//
// It is deliberately not a hand-written double. A double that answered whatever
// the verifier asked would pass every test here while the product still 502s:
// the thing under test IS whether the pins survive a process that never saw the
// InRelease, and only a real store can answer that honestly.
func newSharedPinStore(t *testing.T) AptPinStore {
	t.Helper()
	st, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "pins.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return aptpins.New(st.DB(), aptpins.SQLite)
}

// ─────────────────────────────────────────────────────────────────────────────
// The load-bearing RED: cross-instance chain continuity
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_PoolReachesSigned_OnSecondInstance is the PRD §G3 topology:
// instance A serves `apt-get update` (InRelease + Packages); instance B — a
// separate process sharing ONLY the metadata store — receives the .deb request.
//
// This is the normal path behind a load balancer with >=2 replicas, not an edge
// case. It is also exactly what a single-instance restart or redeploy looks like
// to a client whose apt list is still valid (no `apt-get update` re-runs).
//
// The .deb MUST reach TierSigned on instance B: the chain is proven by the
// distro signature, which instance A already validated and recorded. Nothing
// about that proof is instance-local.
func TestGPGVerifier_PoolReachesSigned_OnSecondInstance(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)

	// Instance A — serves `apt-get update`.
	vA, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, vA, "ubuntu")

	// Instance B — a fresh process. Shares the store and nothing else: no gossip,
	// no leader election, no shared heap (PRD §G3).
	vB, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	res := f.downloadDeb(t, vB)

	assert.Equal(t, artifact.StatusPass, res.Status,
		"pool .deb must PASS on a second instance sharing the store (PRD §G3): %s", res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"pool .deb must reach TierSigned via the persisted InRelease→Packages chain: %s", res.Message)
}

// TestGPGVerifier_UnpinnedPool_StillRefused_OnSecondInstance is the negative that
// keeps persistence honest: persisting pins must never become a way to launder an
// artifact that no verified InRelease ever vouched for.
//
// Instance A verifies a full chain for `hello`; instance B is then asked for a
// DIFFERENT pool file that no InRelease pins. It must fail closed.
func TestGPGVerifier_UnpinnedPool_StillRefused_OnSecondInstance(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)

	vA, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, vA, "ubuntu")

	vB, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	evil := []byte("a .deb no signed InRelease has ever vouched for")
	debPath, debDigest := writeQuarantine(t, evil)
	res, err := vB.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: "main/e/evil", Version: "evil_6.6.6_amd64.deb", Mutable: false,
	}, &artifact.Artifact{Path: debPath, Digest: debDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusFail, res.Status,
		"an unpinned pool file must be refused even with a populated pin store: %s", res.Message)
}

// TestGPGVerifier_TamperedPool_RefusedOnSecondInstance proves the persisted pin
// is enforced, not merely present: instance B is handed the right pool PATH with
// the wrong BYTES. A pin store that only answered "is this path known?" would
// pass this and would be a trust hole.
func TestGPGVerifier_TamperedPool_RefusedOnSecondInstance(t *testing.T) {
	f := newAptChainFixture(t)
	store := newSharedPinStore(t)

	vA, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)
	f.ingestUpdate(t, vA, "ubuntu")

	vB, err := NewGPGVerifier(f.key.keyFile, WithAptPinStore(store))
	require.NoError(t, err)

	tampered := append([]byte{}, f.debRaw...)
	tampered = append(tampered, '!') // same path, different bytes
	debPath, debDigest := writeQuarantine(t, tampered)
	res, err := vB.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: f.poolDir, Version: f.poolFile, Mutable: false,
	}, &artifact.Artifact{Path: debPath, Digest: debDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusFail, res.Status,
		"tampered bytes at a pinned pool path must be refused: %s", res.Message)
	assert.Contains(t, res.Message, "SHA256 mismatch",
		"failure must be the pin mismatch, not a missing pin: %s", res.Message)
}
