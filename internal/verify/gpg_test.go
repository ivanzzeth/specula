package verify

// Tests for GPGVerifier (gpg.go) — the apt end-to-end GPG signature chain.
//
// Traceable to:
//   - PRD §G2 "apt: signed — 发行版 keyring (预置，离线可验)"
//   - DESIGN-REVIEW §1.1: "local keyring → InRelease sig → Packages SHA256 →
//     .deb SHA256. 恶意镜像无发行方私钥，无法伪造。"
//   - DESIGN-REVIEW C1: only attesting TierSigned when the full chain holds
//   - The three-link chain: each link must be independently breakable.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test infrastructure
// ─────────────────────────────────────────────────────────────────────────────

// aptTestKey holds a generated GPG entity and the path to the armored public
// keyring file that NewGPGVerifier can load.
type aptTestKey struct {
	entity  *openpgp.Entity
	keyFile string
}

// newAptTestKey generates a fresh RSA GPG entity and writes the armored public
// key to a temp file.
func newAptTestKey(t *testing.T) *aptTestKey {
	t.Helper()
	entity, err := openpgp.NewEntity("AptPublisher", "Test", "apt@example.com", nil)
	require.NoError(t, err)

	var pubBuf bytes.Buffer
	aw, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(aw))
	require.NoError(t, aw.Close())

	f, err := os.CreateTemp(t.TempDir(), "apt-keyring-*.gpg")
	require.NoError(t, err)
	_, err = f.Write(pubBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return &aptTestKey{entity: entity, keyFile: f.Name()}
}

// sha256Hex computes the SHA256 hex of data (no prefix).
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// writeQuarantine writes content to a temp file and returns its path and sha256
// hex digest (as "sha256:<hex>").
func writeQuarantine(t *testing.T, content []byte) (path, digestFull string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "quarantine-*")
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name(), "sha256:" + sha256Hex(content)
}

// signInRelease creates a clear-signed InRelease document that pins the provided
// suite-relative file SHA256s. The format matches what GPGVerifier.verifyInRelease
// expects: a clear-signed RFC2822 paragraph with a "SHA256:" multi-line field.
//
// Each entry in sums is "<sha256hex> <size> <relpath>".
func signInRelease(t *testing.T, key *aptTestKey, sums []string) []byte {
	t.Helper()
	body := "Suite: noble\nCodename: noble\nSHA256:\n"
	for _, s := range sums {
		body += " " + s + "\n"
	}

	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, key.entity.PrivateKey, nil)
	require.NoError(t, err)
	_, err = fmt.Fprint(w, body)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// buildPackagesContent builds a Debian Packages file (RFC2822 stanzas) with one
// entry for the given pool .deb file, pinning its SHA256 digest.
func buildPackagesContent(poolPath, debSHA256Hex string) []byte {
	pkg := fmt.Sprintf("Package: hello\nVersion: 1.0.0\nArchitecture: amd64\nMaintainer: Test <test@example.com>\nFilename: %s\nSize: 1234\nSHA256: %s\nDescription: Hello\n", poolPath, debSHA256Hex)
	return []byte(pkg)
}

// ─────────────────────────────────────────────────────────────────────────────
// NewGPGVerifier — constructor errors
// ─────────────────────────────────────────────────────────────────────────────

func TestNewGPGVerifier_EmptyPath(t *testing.T) {
	_, err := NewGPGVerifier("")
	require.Error(t, err, "empty keyring path must be rejected")
	assert.Contains(t, err.Error(), "keyring path is required")
}

func TestNewGPGVerifier_MissingFile(t *testing.T) {
	_, err := NewGPGVerifier("/nonexistent/path/keyring.gpg")
	require.Error(t, err, "missing keyring file must be rejected")
}

func TestNewGPGVerifier_InvalidKeyring(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bad-keyring-*")
	require.NoError(t, err)
	_, err = f.WriteString("this is not a gpg keyring")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = NewGPGVerifier(f.Name())
	require.Error(t, err, "invalid keyring must be rejected")
}

func TestNewGPGVerifier_EmptyKeyring(t *testing.T) {
	// An armored PGP file with no actual key data.
	f, err := os.CreateTemp(t.TempDir(), "empty-keyring-*")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = NewGPGVerifier(f.Name())
	require.Error(t, err, "keyring with no keys must be rejected")
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface
// ─────────────────────────────────────────────────────────────────────────────

func TestGPGVerifier_Interface(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	assert.Equal(t, "gpg", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())
	var _ Verifier = v
}

// ─────────────────────────────────────────────────────────────────────────────
// Self-gating: non-apt protocols
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_NonAptProtocol_Skipped verifies that the GPG verifier is a
// no-op for all non-apt protocols — it must not interfere with OCI/Go/npm/etc.
func TestGPGVerifier_NonAptProtocol_Skipped(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	for _, proto := range []string{"oci", "pypi", "npm", "gomod", "helm", "tarball", "git"} {
		t.Run(proto, func(t *testing.T) {
			ref := artifact.ArtifactRef{
				Protocol: proto,
				Name:     "example",
				Version:  "1.0.0",
			}
			art := &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"}
			res, err := v.Verify(context.Background(), ref, art)
			require.NoError(t, err)
			assert.Equal(t, artifact.StatusPass, res.Status)
			assert.Equal(t, artifact.TierChecksum, res.Tier, "non-apt must not claim TierSigned")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseInReleaseSHA256Field (internal helper)
// ─────────────────────────────────────────────────────────────────────────────

func TestParseInReleaseSHA256Field(t *testing.T) {
	t.Run("standard field", func(t *testing.T) {
		value := "abc123 1234 main/binary-amd64/Packages\ndef456 5678 main/binary-amd64/Packages.gz"
		got := parseInReleaseSHA256Field(value)
		assert.Equal(t, "abc123", got["main/binary-amd64/Packages"])
		assert.Equal(t, "def456", got["main/binary-amd64/Packages.gz"])
	})

	t.Run("empty field", func(t *testing.T) {
		got := parseInReleaseSHA256Field("")
		assert.Empty(t, got)
	})

	t.Run("malformed lines ignored", func(t *testing.T) {
		value := "only-one-field\nabc 123 valid/path"
		got := parseInReleaseSHA256Field(value)
		assert.Equal(t, 1, len(got), "malformed lines must be ignored")
		assert.Equal(t, "abc", got["valid/path"])
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// isPackagesFile (internal helper)
// ─────────────────────────────────────────────────────────────────────────────

func TestIsPackagesFile(t *testing.T) {
	assert.True(t, isPackagesFile("main/binary-amd64/Packages"))
	assert.True(t, isPackagesFile("main/binary-amd64/Packages.gz"))
	assert.True(t, isPackagesFile("main/binary-amd64/Packages.xz"))
	assert.True(t, isPackagesFile("main/binary-amd64/Packages.bz2"))
	assert.True(t, isPackagesFile("main/source/Sources"))
	assert.True(t, isPackagesFile("main/source/Sources.gz"))
	assert.False(t, isPackagesFile("InRelease"))
	assert.False(t, isPackagesFile("noble/InRelease"))
	assert.False(t, isPackagesFile("Release"))
	assert.False(t, isPackagesFile("pool/main/h/hello/hello_1.0.0_amd64.deb"))
}

// ─────────────────────────────────────────────────────────────────────────────
// Chain link 1: InRelease GPG verification
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_InRelease_BadSignature_Fail verifies that an InRelease file
// with an invalid/missing GPG signature returns StatusFail.
// "Break each link independently and assert FAIL." (task spec)
func TestGPGVerifier_InRelease_BadSignature_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	// Write a corrupt (unsigned) InRelease file — not a valid clear-signed document.
	inReleaseData := []byte("Suite: noble\nCodename: noble\nSHA256:\n abc123 100 main/Packages\n")
	path, digest := writeQuarantine(t, inReleaseData)

	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/InRelease",
		Digest:   digest,
		Mutable:  true,
	}
	art := &artifact.Artifact{Path: path, Digest: digest}

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr, "bad sig should produce StatusFail result, not a Go error")
	assert.Equal(t, artifact.StatusFail, res.Status, "unsigned InRelease must FAIL")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// TestGPGVerifier_InRelease_WrongKey_Fail verifies that an InRelease signed with
// a key NOT in the keyring fails (attacker cannot forge with their own key).
func TestGPGVerifier_InRelease_WrongKey_Fail(t *testing.T) {
	trustedKey := newAptTestKey(t)  // keyring has this key
	attackerKey := newAptTestKey(t) // InRelease is signed with this (untrusted) key

	v, err := NewGPGVerifier(trustedKey.keyFile)
	require.NoError(t, err)

	// Sign InRelease with attacker key.
	inReleaseData := signInRelease(t, attackerKey, []string{"abc123 100 main/binary-amd64/Packages"})
	path, digest := writeQuarantine(t, inReleaseData)

	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/InRelease",
		Digest:   digest,
		Mutable:  true,
	}
	art := &artifact.Artifact{Path: path, Digest: digest}

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status, "wrong key InRelease must FAIL")
}

// TestGPGVerifier_InRelease_ValidSig_Pass verifies that a properly signed
// InRelease file yields StatusPass/TierSigned and populates the chain state for
// subsequent Packages verification.
func TestGPGVerifier_InRelease_ValidSig_Pass(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	packagesDigestHex := "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	sums := []string{fmt.Sprintf("%s 100 main/binary-amd64/Packages", packagesDigestHex)}
	inReleaseData := signInRelease(t, key, sums)
	path, digest := writeQuarantine(t, inReleaseData)

	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/InRelease",
		Digest:   digest,
		Mutable:  true,
	}
	art := &artifact.Artifact{Path: path, Digest: digest}

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status, "valid InRelease must PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Chain link 2: Packages SHA256 verification
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_Packages_WithoutInRelease_Fail verifies that Packages
// verification fails if InRelease has not been verified first (incomplete chain).
func TestGPGVerifier_Packages_WithoutInRelease_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	// Directly try to verify Packages without first processing InRelease.
	packagesContent := buildPackagesContent("pool/main/h/hello/hello_1.0.0_amd64.deb", "abc123")
	path, digest := writeQuarantine(t, packagesContent)

	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/main/binary-amd64/Packages",
		Digest:   digest,
		Mutable:  true,
	}
	art := &artifact.Artifact{Path: path, Digest: digest}

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"Packages without prior InRelease must FAIL — incomplete chain")
	assert.Contains(t, res.Message, "InRelease not yet verified")
}

// TestGPGVerifier_Packages_CorrectChain_Pass verifies the full
// InRelease → Packages chain: after a valid InRelease, the matching Packages
// file passes.
func TestGPGVerifier_Packages_CorrectChain_Pass(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// Build Packages content and compute its SHA256 for the InRelease.
	debSHA256Hex := "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"
	packagesContent := buildPackagesContent("pool/main/h/hello/hello_1.0.0_amd64.deb", debSHA256Hex)
	packagesPath, packagesDigest := writeQuarantine(t, packagesContent)
	packagesHex := packagesDigest[7:] // strip "sha256:" prefix

	// Build and verify InRelease that pins the Packages digest.
	sums := []string{fmt.Sprintf("%s %d main/binary-amd64/Packages", packagesHex, len(packagesContent))}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	inReleaseRef := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/InRelease",
		Digest:   inReleaseDigest,
		Mutable:  true,
	}
	res1, err := v.Verify(ctx, inReleaseRef, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status, "InRelease must pass before testing Packages")

	// Now verify Packages — should pass since its SHA256 matches InRelease.
	packagesRef := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "ubuntu",
		Version:  "noble/main/binary-amd64/Packages",
		Digest:   packagesDigest,
		Mutable:  true,
	}
	res2, err := v.Verify(ctx, packagesRef, &artifact.Artifact{Path: packagesPath, Digest: packagesDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res2.Status, "Packages with correct SHA256 must PASS")
	assert.Equal(t, artifact.TierSigned, res2.Tier)
}

// TestGPGVerifier_Packages_DigestMismatch_Fail verifies chain link 2: if the
// Packages file's actual SHA256 doesn't match what InRelease committed to, FAIL.
func TestGPGVerifier_Packages_DigestMismatch_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// InRelease pins the SHA256 of a DIFFERENT Packages file.
	wrongHex := "deadbeef0000000000000000deadbeef0000000000000000deadbeef00000000"
	sums := []string{fmt.Sprintf("%s 100 main/binary-amd64/Packages", wrongHex)}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	inReleaseRef := artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}
	res1, err := v.Verify(ctx, inReleaseRef, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status)

	// The actual Packages file has a different content/SHA256.
	actualPackages := buildPackagesContent("pool/main/h/hello/hello_1.0.0_amd64.deb", "abc")
	packagesPath, packagesDigest := writeQuarantine(t, actualPackages)

	packagesRef := artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/Packages",
		Digest:  packagesDigest, Mutable: true,
	}
	res2, err := v.Verify(ctx, packagesRef, &artifact.Artifact{Path: packagesPath, Digest: packagesDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res2.Status, "Packages digest mismatch vs InRelease must FAIL")
}

// ─────────────────────────────────────────────────────────────────────────────
// Chain link 3: pool .deb SHA256 verification
// ─────────────────────────────────────────────────────────────────────────────

// TestGPGVerifier_Pool_WithoutPackages_Fail verifies that .deb verification fails
// if the Packages index has not been verified first (incomplete chain).
func TestGPGVerifier_Pool_WithoutPackages_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	debContent := []byte("fake deb content")
	debPath, debDigest := writeQuarantine(t, debContent)

	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "hello",
		Version:  "hello_1.0.0_amd64.deb",
		Digest:   debDigest,
		Mutable:  false, // pool files are immutable
	}
	art := &artifact.Artifact{Path: debPath, Digest: debDigest}

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"pool .deb without prior Packages verification must FAIL")
	assert.Contains(t, res.Message, "not found in verified Packages index")
}

// TestGPGVerifier_FullChain_Pass verifies the complete three-link chain:
// InRelease → Packages → .deb. All three must hold for TierSigned.
// "Break each link independently and assert FAIL" — this tests the golden path.
func TestGPGVerifier_FullChain_Pass(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// 1. Build the .deb content and its SHA256.
	debContent := []byte("fake debian package binary content for testing")
	debPath, debDigest := writeQuarantine(t, debContent)
	debSHA256Hex := debDigest[7:] // strip "sha256:"

	const poolPath = "pool/main/h/hello/hello_1.0.0_amd64.deb"

	// 2. Build Packages content and compute its SHA256.
	packagesContent := buildPackagesContent(poolPath, debSHA256Hex)
	packagesPath, packagesDigest := writeQuarantine(t, packagesContent)
	packagesHex := packagesDigest[7:]

	// 3. Build InRelease that pins Packages SHA256.
	sums := []string{fmt.Sprintf("%s %d main/binary-amd64/Packages", packagesHex, len(packagesContent))}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	// Step 1: Verify InRelease (populates suiteSHA256s cache).
	inReleaseRef := artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}
	res1, err := v.Verify(ctx, inReleaseRef, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status, "chain step 1: InRelease must pass")
	require.Equal(t, artifact.TierSigned, res1.Tier)

	// Step 2: Verify Packages (checks SHA256 against InRelease; populates poolSHA256s).
	packagesRef := artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/Packages",
		Digest:  packagesDigest, Mutable: true,
	}
	res2, err := v.Verify(ctx, packagesRef, &artifact.Artifact{Path: packagesPath, Digest: packagesDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res2.Status, "chain step 2: Packages must pass")

	// Step 3: Verify pool .deb (checks SHA256 against Packages).
	// verifyPool constructs poolPath = "pool/" + ref.Name + "/" + ref.Version, so
	// ref.Name must be the component path ("main/h/hello") so the lookup key
	// matches the "Filename:" field in the Packages stanza.
	debRef := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "main/h/hello",
		Version:  "hello_1.0.0_amd64.deb",
		Digest:   debDigest,
		Mutable:  false,
	}
	res3, err := v.Verify(ctx, debRef, &artifact.Artifact{Path: debPath, Digest: debDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res3.Status,
		"chain step 3: pool .deb must PASS when all three links hold")
	assert.Equal(t, artifact.TierSigned, res3.Tier,
		"full chain pass must reach TierSigned")
}

// TestGPGVerifier_Pool_DigestMismatch_Fail verifies chain link 3: if the actual
// .deb SHA256 does not match the Packages-verified value, FAIL.
func TestGPGVerifier_Pool_DigestMismatch_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// Build a .deb and compute its SHA256.
	debContent := []byte("original deb content")
	debSHA256Hex := sha256Hex(debContent)

	const poolPath = "pool/main/h/hello/hello_1.0.0_amd64.deb"
	packagesContent := buildPackagesContent(poolPath, debSHA256Hex)
	packagesPath, packagesDigest := writeQuarantine(t, packagesContent)
	packagesHex := packagesDigest[7:]

	sums := []string{fmt.Sprintf("%s %d main/binary-amd64/Packages", packagesHex, len(packagesContent))}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	// Verify InRelease.
	inReleaseRef := artifact.ArtifactRef{Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease", Digest: inReleaseDigest, Mutable: true}
	res1, _ := v.Verify(ctx, inReleaseRef, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.Equal(t, artifact.StatusPass, res1.Status)

	// Verify Packages.
	packagesRef := artifact.ArtifactRef{Protocol: "apt", Name: "ubuntu", Version: "noble/main/binary-amd64/Packages", Digest: packagesDigest, Mutable: true}
	res2, _ := v.Verify(ctx, packagesRef, &artifact.Artifact{Path: packagesPath, Digest: packagesDigest})
	require.Equal(t, artifact.StatusPass, res2.Status)

	// Now try to serve a TAMPERED .deb (different content → different SHA256).
	tamperedContent := []byte("tampered deb content")
	tamperedPath, tamperedDigest := writeQuarantine(t, tamperedContent)

	// ref.Name is the component path so verifyPool constructs the same poolPath
	// that was stored by verifyPackages from the Packages "Filename:" field.
	debRef := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     "main/h/hello",
		Version:  "hello_1.0.0_amd64.deb",
		Digest:   tamperedDigest,
		Mutable:  false,
	}
	res3, verErr := v.Verify(ctx, debRef, &artifact.Artifact{Path: tamperedPath, Digest: tamperedDigest})
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res3.Status,
		"tampered .deb (SHA256 mismatch vs Packages) must FAIL")
}

// TestGPGVerifier_OtherDistsFiles_PassThrough verifies that mutable dists/ files
// other than InRelease and Packages (e.g. Release, Release.gpg, Translation-*)
// pass through as TierChecksum so they don't block the pipeline.
func TestGPGVerifier_OtherDistsFiles_PassThrough(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	for _, version := range []string{"noble/Release", "noble/Release.gpg", "noble/Translation-en", "noble/Contents-amd64"} {
		t.Run(version, func(t *testing.T) {
			ref := artifact.ArtifactRef{
				Protocol: "apt",
				Name:     "ubuntu",
				Version:  version,
				Mutable:  true,
			}
			art := &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"}
			res, verErr := v.Verify(context.Background(), ref, art)
			require.NoError(t, verErr)
			assert.Equal(t, artifact.StatusPass, res.Status, "%s must pass through", version)
			assert.Equal(t, artifact.TierChecksum, res.Tier)
		})
	}
}
