package verify

// gpg_byhash_test.go — BUG B regression: apt Acquire-By-Hash defeats the signed
// chain, degrading PRD §G2's "end-to-end gold standard" to tofu.
//
// Real `apt-get update` against a mirror advertising `Acquire-By-Hash: yes`
// fetches indices by CONTENT ADDRESS:
//
//	GET dists/noble/main/binary-amd64/by-hash/SHA256/37cb57f1…
//
// while InRelease lists the canonical path (`main/binary-amd64/Packages.xz`) and
// ZERO by-hash paths. The verifier's literal `sums[relPath]` lookup therefore
// misses, listed=false, and the file falls to TierChecksum → tofu — even though
// the requested hash 37cb57f1… IS pinned inside the GPG-verified InRelease.
//
// DESIGN-REVIEW §3: by-hash content-addresses index files precisely so apt
// fetches exactly the index the signature covers. The requested hash IS the pin.
//
// The existing regression suite (gpg_inrelease_pin_regression_test.go) only ever
// exercises literal paths ("noble/main/i18n/Translation-en"); by-hash appeared in
// no test at all, which is why this shipped green.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// aptByHashFixture builds a GPG-verified InRelease pinning an UNCOMPRESSED
// Packages index at its canonical path, and returns the verifier plus the
// index's bytes/digest.
//
// The compression axis is deliberately held constant here so these tests isolate
// by-hash RESOLUTION. Real mirrors serve .xz, and that combination — by-hash +
// xz, which is what every real `apt-get update` actually drives — is covered by
// TestGPGVerifier_XzPackages_ChainVerifiesAndPinsPool in gpg_decompress_test.go.
// (This comment previously claimed the fixture pinned "Packages.xz"; it never
// did, and no test exercised .xz at all until that one was added.)
type aptByHashFixture struct {
	v            *GPGVerifier
	indexPath    string
	indexDigest  string
	indexHex     string
	poolDeb      []byte
	poolDebPath  string
	poolDebHex   string
	poolFilename string
}

func newAptByHashFixture(t *testing.T) *aptByHashFixture {
	t.Helper()
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	// A real Packages index pinning one pool .deb.
	poolFilename := "pool/main/h/hello/hello_2.10-3_amd64.deb"
	poolDeb := []byte("fake .deb payload for by-hash chain test\n")
	poolDebPath, poolDebDigest := writeQuarantine(t, poolDeb)
	poolDebHex := poolDebDigest[7:]

	indexContent := buildPackagesContent(poolFilename, poolDebHex)
	indexPath, indexDigest := writeQuarantine(t, indexContent)
	indexHex := indexDigest[7:]

	// InRelease pins ONLY the canonical path — exactly like every real mirror.
	sums := []string{
		fmt.Sprintf("%s %d main/binary-amd64/Packages", indexHex, len(indexContent)),
	}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	res, err := v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, "InRelease must GPG-verify")
	require.Equal(t, artifact.TierSigned, res.Tier)

	return &aptByHashFixture{
		v: v, indexPath: indexPath, indexDigest: indexDigest, indexHex: indexHex,
		poolDeb: poolDeb, poolDebPath: poolDebPath, poolDebHex: poolDebHex,
		poolFilename: poolFilename,
	}
}

// TestGPGVerifier_ByHashIndex_ReachesTierSigned is the primary RED test for BUG B.
func TestGPGVerifier_ByHashIndex_ReachesTierSigned(t *testing.T) {
	f := newAptByHashFixture(t)

	// This is the request real apt actually issues under Acquire-By-Hash.
	res, err := f.v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/by-hash/SHA256/" + f.indexHex,
		Digest:  f.indexDigest, Mutable: true,
	}, &artifact.Artifact{Path: f.indexPath, Digest: f.indexDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusPass, res.Status, res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"BUG B: the requested by-hash SHA256 IS the InRelease pin — an index fetched at "+
			"by-hash/SHA256/<hex> whose <hex> is among the suite's GPG-signed SHA256 sums is "+
			"verified at signed. Leaving it at TierChecksum degrades apt (PRD §G2's end-to-end "+
			"gold standard) to tofu for everything real apt fetches.")
}

// TestGPGVerifier_ByHashPackages_PinsPoolDebs asserts the chain CONTINUES through
// a by-hash-fetched Packages index: its pool .deb hashes must be pinned, so the
// .deb itself still reaches signed. Recognising by-hash only for the tier label
// while skipping the index parse would leave every .deb fail-closed.
func TestGPGVerifier_ByHashPackages_PinsPoolDebs(t *testing.T) {
	f := newAptByHashFixture(t)
	ctx := context.Background()

	_, err := f.v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/by-hash/SHA256/" + f.indexHex,
		Digest:  f.indexDigest, Mutable: true,
	}, &artifact.Artifact{Path: f.indexPath, Digest: f.indexDigest})
	require.NoError(t, err)

	// pool .deb: name+version are joined as "pool/" + Name + "/" + Version.
	debDigest := "sha256:" + f.poolDebHex
	res, err := f.v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "main/h/hello", Version: "hello_2.10-3_amd64.deb",
		Digest: debDigest, Mutable: false,
	}, &artifact.Artifact{Path: f.poolDebPath, Digest: debDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusPass, res.Status,
		"BUG B: a Packages index fetched by-hash must still be PARSED so its pool .deb "+
			"SHA256s are pinned — otherwise every .deb fails closed. Got: "+res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// TestGPGVerifier_ByHashUnknownHash_StaysAtTierChecksum keeps the fix honest: a
// by-hash request whose hex is NOT among the signed sums must NOT be promoted.
// This is the assertion that fails if the fix "recognises by-hash" by trusting
// the requested hash instead of checking it against InRelease.
func TestGPGVerifier_ByHashUnknownHash_StaysAtTierChecksum(t *testing.T) {
	f := newAptByHashFixture(t)

	rogue := []byte("attacker-supplied index not pinned by InRelease\n")
	roguePath, rogueDigest := writeQuarantine(t, rogue)
	rogueHex := rogueDigest[7:]

	res, err := f.v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/by-hash/SHA256/" + rogueHex,
		Digest:  rogueDigest, Mutable: true,
	}, &artifact.Artifact{Path: roguePath, Digest: rogueDigest})
	require.NoError(t, err)

	assert.NotEqual(t, artifact.TierSigned, res.Tier,
		"a by-hash hex that InRelease does not pin must never reach signed — the pin comes "+
			"from the GPG-verified InRelease, not from the requester")
}

// TestGPGVerifier_ByHashContentMismatch_Fails: the mirror returned bytes that do
// not hash to the requested (and signed) by-hash address → tamper, fail closed.
func TestGPGVerifier_ByHashContentMismatch_Fails(t *testing.T) {
	f := newAptByHashFixture(t)

	wrong := []byte("wrong bytes served for a signed by-hash address\n")
	wrongPath, wrongDigest := writeQuarantine(t, wrong)

	res, err := f.v.Verify(context.Background(), artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		// Address is the SIGNED index hex, but the body is something else.
		Version: "noble/main/binary-amd64/by-hash/SHA256/" + f.indexHex,
		Digest:  wrongDigest, Mutable: true,
	}, &artifact.Artifact{Path: wrongPath, Digest: wrongDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusFail, res.Status,
		"content not matching the signed by-hash address must fail closed: "+res.Message)
}

// ── Compressed Packages indices ──────────────────────────────────────────────
//
// Real mirrors serve ONLY compressed indices — mirrors.aliyun.com/ubuntu returns
// 404 for dists/noble/main/binary-amd64/Packages and 200 for Packages.xz and
// Packages.gz — and apt prefers .xz. isPackagesFile deliberately matches
// "Packages.gz"/"Packages.xz"/"Packages.bz2" ("compressed or not"), but
// verifyPackages fed the still-compressed bytes straight to ParseBinaryIndex.
//
// This bug pre-dates the by-hash fix but was DORMANT: by-hash requests never
// reached verifyPackages (they fell through to the TierChecksum pass-through), so
// the parse never ran. Fixing by-hash routes real apt traffic into verifyPackages
// and wakes it up — a live `apt-get update` 502s:
//
//	Err:2 http://127.0.0.1:18432/apt/ubuntu noble/main amd64 Packages
//	  502  Bad Gateway
//	ERROR "apt: store mutable dists" err="... tier=signed status=fail:
//	       gpg: parse Packages index \"main/binary-amd64/Packages.xz\": ..."

// TestGPGVerifier_GzPackages_ChainVerifiesAndPinsPool: a gzipped Packages index
// must chain-verify AND have its pool .deb hashes pinned.
func TestGPGVerifier_GzPackages_ChainVerifiesAndPinsPool(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	poolFilename := "pool/main/h/hello/hello_2.10-3_amd64.deb"
	poolDeb := []byte("fake .deb payload\n")
	poolDebPath, poolDebDigest := writeQuarantine(t, poolDeb)

	// Gzip the Packages index, exactly as a real mirror serves it.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, err = zw.Write(buildPackagesContent(poolFilename, poolDebDigest[7:]))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	idxPath, idxDigest := writeQuarantine(t, gz.Bytes())
	sums := []string{fmt.Sprintf("%s %d main/binary-amd64/Packages.gz", idxDigest[7:], gz.Len())}
	inRelPath, inRelDigest := writeQuarantine(t, signInRelease(t, key, sums))

	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inRelDigest, Mutable: true,
	}, &artifact.Artifact{Path: inRelPath, Digest: inRelDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status)

	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/main/binary-amd64/Packages.gz",
		Digest: idxDigest, Mutable: true,
	}, &artifact.Artifact{Path: idxPath, Digest: idxDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"a gzipped Packages index must chain-verify: real mirrors serve ONLY compressed "+
			"indices (uncompressed Packages is 404 on aliyun). Got: "+res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)

	// The chain must continue: the .deb it pins must reach signed.
	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "main/h/hello", Version: "hello_2.10-3_amd64.deb",
		Digest: poolDebDigest, Mutable: false,
	}, &artifact.Artifact{Path: poolDebPath, Digest: poolDebDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"a compressed index must still pin its pool .deb hashes: "+res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier)
}
