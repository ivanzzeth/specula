package verify

// Real-binary compatibility test for the Helm provenance verifier: proves the
// PRODUCTION HelmProvVerifier accepts a `.prov` produced by the ACTUAL `helm`
// CLI signing with a REAL GPG key — not a fixture built with the same openpgp
// library the verifier uses.
//
// WHY THIS IS SEPARATE FROM helmprov_test.go
// ------------------------------------------
// helmprov_test.go builds every .prov with ProtonMail/go-crypto's clearsign
// package — the SAME library HelmProvVerifier decodes with. That proves the
// verifier is self-consistent, but it cannot prove compatibility with what helm
// actually emits: helm signs with helm.sh/helm/v3/pkg/provenance over a body
// whose exact shape (a YAML Chart.yaml block, a `...` document separator, then a
// `files:` block) and clear-sign hash (SHA512) are helm's choices, not ours. If
// any of those diverged from what our parser/verifier expects, the synthetic
// tests would stay green while production silently rejected real charts.
//
// This test closes that gap: real `gpg` key → real `helm package --sign` →
// production HelmProvVerifier over the real .prov and the real chart digest.
//
// SCOPE NOTE (kept deliberately honest): this proves OUR VERIFIER accepts a
// .prov produced by real helm. It says NOTHING about whether any real upstream
// mirror serves .prov files — the CN helm mirror (mirror.azure.cn) publishes
// none (see scripts/trust-oracle.sh + PRD §G2 helm row). Those are different
// claims and must not be blurred.
//
// Skipped unless helm AND gpg are available and not -short.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// helmAndGPG returns the helm and gpg binary paths, or "" if either is missing.
func helmAndGPG() (string, string) {
	helm, herr := exec.LookPath("helm")
	gpg, gerr := exec.LookPath("gpg")
	if herr != nil || gerr != nil {
		return "", ""
	}
	return helm, gpg
}

// realHelmProv generates a passphraseless GPG key in an isolated GNUPGHOME, uses
// the real `helm` CLI to package + sign a fresh chart, and returns the armored
// public key path, the .prov bytes, the chart .tgz filename and its sha256 hex.
func realHelmProv(t *testing.T, helm, gpg string) (pubKeyPath string, provBytes []byte, chartFile, chartSHA256 string) {
	t.Helper()
	dir := t.TempDir()
	gnupg := filepath.Join(dir, "gnupg")
	require.NoError(t, os.MkdirAll(gnupg, 0o700))
	env := append(os.Environ(), "GNUPGHOME="+gnupg)

	run := func(bin string, args ...string) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "%s %v failed: %s", bin, args, string(out))
	}

	// 1. Passphraseless RSA key.
	params := filepath.Join(dir, "keyparams")
	require.NoError(t, os.WriteFile(params, []byte(
		"%no-protection\nKey-Type: RSA\nKey-Length: 3072\n"+
			"Name-Real: Specula Helm Test\nName-Email: helm-test@specula.local\n"+
			"Expire-Date: 0\n%commit\n"), 0o600))
	run(gpg, "--batch", "--gen-key", params)

	// 2. Export public (armored, for the verifier keyring) + secret (binary, for helm).
	pubKeyPath = filepath.Join(dir, "pub.asc")
	secring := filepath.Join(dir, "secring.gpg")
	runOut := func(bin, outPath string, args ...string) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		require.NoErrorf(t, err, "%s %v failed", bin, args)
		require.NoError(t, os.WriteFile(outPath, out, 0o600))
	}
	runOut(gpg, pubKeyPath, "--armor", "--export", "helm-test@specula.local")
	runOut(gpg, secring, "--export-secret-keys", "helm-test@specula.local")

	// 3. Real helm chart, packaged AND signed. helm reads the signing key from the
	//    keyring path (here the exported secret keyring).
	run(helm, "create", "mychart")
	run(helm, "package", "--sign", "--key", "helm-test@specula.local", "--keyring", secring, "mychart")

	chartFile = "mychart-0.1.0.tgz"
	chartPath := filepath.Join(dir, chartFile)
	tgz, err := os.ReadFile(chartPath)
	require.NoError(t, err)
	sum := sha256.Sum256(tgz)
	chartSHA256 = hex.EncodeToString(sum[:])

	provBytes, err = os.ReadFile(chartPath + ".prov")
	require.NoError(t, err, "helm must have produced a .prov")
	return pubKeyPath, provBytes, chartFile, chartSHA256
}

// helmProvArtifact builds an *artifact.Artifact carrying the .prov as
// Attachments[0] and the given chart sha256 as its Digest (as the streaming
// quarantine would compute it).
func helmProvArtifact(sha256hex string, provBytes []byte) *artifact.Artifact {
	art := &artifact.Artifact{Path: "/quarantine/mychart-0.1.0.tgz", Digest: "sha256:" + sha256hex, Size: 4096}
	art.Meta.Attachments = [][]byte{provBytes}
	return art
}

// TestHelmProv_RealHelm_ValidSignature_Signed drives real helm-produced
// provenance through the production verifier and asserts TierSigned. A PASS
// proves our parser + GPG verification match what helm actually emits.
func TestHelmProv_RealHelm_ValidSignature_Signed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-helm integration test in -short mode")
	}
	helm, gpg := helmAndGPG()
	if helm == "" {
		t.Skip("helm and/or gpg not available")
	}

	pubKey, provBytes, chartFile, chartSHA := realHelmProv(t, helm, gpg)

	v, err := NewHelmProvVerifier(pubKey)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	res, verErr := v.Verify(context.Background(), ref, helmProvArtifact(chartSHA, provBytes))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"a real helm-signed .prov with a matching digest must PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"real helm .prov must reach TierSigned through the production verifier")
}

// TestHelmProv_RealHelm_TamperedSignature_Fail proves a real .prov whose signed
// body has been altered is REFUSED (fail-closed at TierSigned), not silently
// downgraded. The tampered bytes reach verifyProv directly — there is no
// upstream that could 404 and hide the tamper (the bcc92b4 lesson: the negative
// must actually reach the verifier).
//
// CRITICAL: the tampered byte is in the Chart.yaml PORTION of the signed body,
// NOT the files: digest. Flipping the digest would make the test pass via the
// digest-mismatch branch even if signature verification were disabled — a
// negative that passes for the wrong reason (verified: a mutation disabling the
// GPG check left this exact test green when the tamper was on the digest). By
// leaving the committed digest intact and matching art.Digest, the ONLY thing
// that can reject this artifact is real GPG signature verification. The mutation
// meta-check re-confirms this test goes red when that check is defeated.
func TestHelmProv_RealHelm_TamperedSignature_Fail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-helm integration test in -short mode")
	}
	helm, gpg := helmAndGPG()
	if helm == "" {
		t.Skip("helm and/or gpg not available")
	}

	pubKey, provBytes, chartFile, chartSHA := realHelmProv(t, helm, gpg)

	// Corrupt one byte in the Chart.yaml metadata inside the clear-signed body —
	// helm's default `description: A Helm chart for Kubernetes`. This alters the
	// signed content (so the GPG signature no longer covers it) WITHOUT touching
	// the files: digest, so the digest binding still matches art.Digest and the
	// signature check is the sole gate.
	tampered := make([]byte, len(provBytes))
	copy(tampered, provBytes)
	needle := []byte("A Helm chart for Kubernetes")
	idx := indexOf(tampered, needle)
	require.GreaterOrEqual(t, idx, 0, "helm's default description must appear in the signed body")
	tampered[idx] = 'a' // 'A' -> 'a': one-byte change to the signed plaintext

	v, err := NewHelmProvVerifier(pubKey)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	res, verErr := v.Verify(context.Background(), ref, helmProvArtifact(chartSHA, tampered))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"a tampered signed body must FAIL — the GPG signature no longer covers it")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// TestHelmProv_RealHelm_DigestMismatch_Fail proves that a real, correctly-signed
// .prov whose committed digest does NOT match the chart Specula actually stored
// is refused. This is the "mirror swapped the chart bytes but kept a valid
// (old) .prov" case: the signature verifies, but the digest binding does not.
func TestHelmProv_RealHelm_DigestMismatch_Fail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-helm integration test in -short mode")
	}
	helm, gpg := helmAndGPG()
	if helm == "" {
		t.Skip("helm and/or gpg not available")
	}

	pubKey, provBytes, chartFile, _ := realHelmProv(t, helm, gpg)

	// Present a DIFFERENT stored digest than the one the (validly signed) .prov
	// commits to.
	const otherSHA = "deadbeef0000000000000000deadbeef0000000000000000deadbeef00000000"
	v, err := NewHelmProvVerifier(pubKey)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	res, verErr := v.Verify(context.Background(), ref, helmProvArtifact(otherSHA, provBytes))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"valid signature but mismatched chart digest must FAIL")
	assert.Contains(t, res.Message, "mismatch")
}

// indexOf is a tiny substring search over bytes (avoids importing bytes solely
// for one call site in this file).
func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := range needle {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
