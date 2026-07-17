package verify

// Regression tests for Bug 3: apt only InRelease reaches TierSigned; other files
// listed in the InRelease SHA256 section (Translation-*, DEP11, Contents-*, etc.)
// stay at TierTofu because verifyDists' default case returned TierChecksum without
// checking suiteSHA256s.
//
// Real apt-get update requests: InRelease, Packages, Translation-en, DEP11
// Components, Contents-amd64, etc. Every one of those files is SHA256-pinned in
// InRelease. After verifying InRelease the chain already holds a GPG-rooted hash
// for each of them; the verifier must use it.
//
// Tier flow (Chain aggregates highest across all verifiers):
//   before fix: ChecksumVerifier(TierChecksum) + TofuVerifier(TierTofu) +
//               GPGVerifier(TierChecksum via default) → TierTofu
//   after  fix: ChecksumVerifier(TierChecksum) + TofuVerifier(TierTofu) +
//               GPGVerifier(TierSigned via InRelease pin) → TierSigned
//
// RED: before the fix, TestGPGVerifier_TranslationPinnedByInRelease_ReachesTierSigned
// asserts TierSigned but the verifier returns TierChecksum — the assertion FAILS.
// GREEN: after the fix the verifier consults suiteSHA256s and returns TierSigned.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestGPGVerifier_TranslationPinnedByInRelease_ReachesTierSigned is the primary
// RED test for Bug 3. After InRelease is GPG-verified, a Translation-en file
// whose SHA256 is listed in InRelease must be verified at TierSigned — not left
// at TierChecksum by the unmodified default case.
func TestGPGVerifier_TranslationPinnedByInRelease_ReachesTierSigned(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// Build the Translation-en file and record its SHA256.
	translationContent := []byte("Package: hello\nDescription-md5: abc\nDescription-en: Hello\n")
	translationPath, translationDigest := writeQuarantine(t, translationContent)
	translationHex := translationDigest[7:] // strip "sha256:"

	// Build InRelease that pins the Translation-en SHA256. The suite-relative path
	// used by apt-get update is "main/i18n/Translation-en".
	sums := []string{
		fmt.Sprintf("%s %d main/i18n/Translation-en", translationHex, len(translationContent)),
	}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	// Step 1: GPG-verify InRelease (populates suiteSHA256s with "main/i18n/Translation-en").
	res1, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status, "InRelease must GPG-verify")
	require.Equal(t, artifact.TierSigned, res1.Tier)

	// Step 2: Verify the Translation-en file.
	// ref.Version mirrors what the apt handler produces: "noble/main/i18n/Translation-en"
	// (the full dists-relative path, including suite prefix).
	//
	// Before fix: the default case fires and returns TierChecksum — this assertion FAILS.
	// After fix:  verifyInReleasePin finds the SHA256 in suiteSHA256s and returns TierSigned.
	res2, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/i18n/Translation-en",
		Digest:  translationDigest, Mutable: true,
	}, &artifact.Artifact{Path: translationPath, Digest: translationDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res2.Status,
		"Translation-en pinned by InRelease must PASS")
	assert.Equal(t, artifact.TierSigned, res2.Tier,
		"Translation-en listed in InRelease SHA256 must reach TierSigned — Bug 3 regression")
}

// TestGPGVerifier_DEP11PinnedByInRelease_ReachesTierSigned covers the other file
// class mentioned in Bug 3: DEP11 / cnf / Contents files that are listed in
// InRelease but are NOT Packages/Sources indices. Same bug, different file name.
func TestGPGVerifier_DEP11PinnedByInRelease_ReachesTierSigned(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	dep11Content := []byte("Format: 0.8\nPackage: hello\n---\n")
	dep11Path, dep11Digest := writeQuarantine(t, dep11Content)
	dep11Hex := dep11Digest[7:]

	cnfContent := []byte("cnf index placeholder\n")
	cnfPath, cnfDigest := writeQuarantine(t, cnfContent)
	cnfHex := cnfDigest[7:]

	sums := []string{
		fmt.Sprintf("%s %d main/dep11/Components-amd64.yml.xz", dep11Hex, len(dep11Content)),
		fmt.Sprintf("%s %d main/cnf/Commands-amd64", cnfHex, len(cnfContent)),
	}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	res1, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status)

	for _, tc := range []struct {
		version string
		path    string
		digest  string
	}{
		{"noble/main/dep11/Components-amd64.yml.xz", dep11Path, dep11Digest},
		{"noble/main/cnf/Commands-amd64", cnfPath, cnfDigest},
	} {
		t.Run(tc.version, func(t *testing.T) {
			res, verErr := v.Verify(ctx, artifact.ArtifactRef{
				Protocol: "apt", Name: "ubuntu",
				Version: tc.version, Digest: tc.digest, Mutable: true,
			}, &artifact.Artifact{Path: tc.path, Digest: tc.digest})
			require.NoError(t, verErr)
			assert.Equal(t, artifact.StatusPass, res.Status)
			assert.Equal(t, artifact.TierSigned, res.Tier,
				"%s pinned by InRelease must reach TierSigned", tc.version)
		})
	}
}

// TestGPGVerifier_InReleasePinnedFile_DigestMismatch_Fail verifies that a dists
// file listed in InRelease but with a wrong SHA256 returns StatusFail at TierSigned.
// This is the tamper-detection path for the new code.
func TestGPGVerifier_InReleasePinnedFile_DigestMismatch_Fail(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	goodContent := []byte("Translation: good\n")
	goodHex := sha256Hex(goodContent)

	sums := []string{fmt.Sprintf("%s %d main/i18n/Translation-en", goodHex, len(goodContent))}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	res1, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status)

	// Serve a TAMPERED Translation-en (different content, different SHA256).
	tamperedContent := []byte("Translation: tampered by attacker\n")
	tamperedPath, tamperedDigest := writeQuarantine(t, tamperedContent)

	res2, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/i18n/Translation-en",
		Digest:  tamperedDigest, Mutable: true,
	}, &artifact.Artifact{Path: tamperedPath, Digest: tamperedDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res2.Status,
		"Translation-en with SHA256 mismatch vs InRelease must FAIL")
	assert.Equal(t, artifact.TierSigned, res2.Tier,
		"tamper detection must operate at TierSigned")
}

// TestGPGVerifier_FileNotInInRelease_StaysAtTierChecksum verifies that after
// InRelease is verified, files NOT listed in its SHA256 section (e.g., Release.gpg)
// still pass through at TierChecksum — the existing behaviour is preserved.
func TestGPGVerifier_FileNotInInRelease_StaysAtTierChecksum(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// InRelease only pins Translation-en; Release.gpg is not listed.
	goodHex := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	sums := []string{fmt.Sprintf("%s 100 main/i18n/Translation-en", goodHex)}
	inReleaseData := signInRelease(t, key, sums)
	inReleasePath, inReleaseDigest := writeQuarantine(t, inReleaseData)

	res1, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res1.Status)

	// Release.gpg is NOT listed in InRelease SHA256 → must stay TierChecksum PASS.
	res2, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/Release.gpg", Mutable: true,
	}, &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res2.Status,
		"unlisted dists file must still pass through")
	assert.Equal(t, artifact.TierChecksum, res2.Tier,
		"dists file not in InRelease SHA256 must stay at TierChecksum")
}

// TestGPGVerifier_InReleaseNotYetSeen_DefaultCaseStaysAtTierChecksum verifies that
// with a fresh verifier (no InRelease verified yet), ALL files in the default case
// still return TierChecksum PASS — the existing pass-through is fully preserved.
// This also confirms the existing TestGPGVerifier_OtherDistsFiles_PassThrough test
// still holds after the fix.
func TestGPGVerifier_InReleaseNotYetSeen_DefaultCaseStaysAtTierChecksum(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)

	// No InRelease verified — completely fresh verifier.
	for _, version := range []string{
		"noble/Release",
		"noble/Release.gpg",
		"noble/Translation-en",
		"noble/main/i18n/Translation-en",
		"noble/main/dep11/Components-amd64.yml.xz",
		"noble/main/cnf/Commands-amd64",
	} {
		t.Run(version, func(t *testing.T) {
			res, verErr := v.Verify(context.Background(), artifact.ArtifactRef{
				Protocol: "apt", Name: "ubuntu", Version: version, Mutable: true,
			}, &artifact.Artifact{Path: "/dev/null", Digest: "sha256:abc"})
			require.NoError(t, verErr)
			assert.Equal(t, artifact.StatusPass, res.Status)
			assert.Equal(t, artifact.TierChecksum, res.Tier,
				"without prior InRelease verification, %q must stay at TierChecksum", version)
		})
	}
}
