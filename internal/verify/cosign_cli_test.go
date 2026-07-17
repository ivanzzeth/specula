package verify

// Real-binary compatibility test: proves the PRODUCTION OCISignatureFetcher +
// CosignVerifier accept a signature produced by the ACTUAL `cosign` CLI — not a
// hand-built fixture that merely claims to be cosign's format.
//
// WHY THIS IS SEPARATE FROM cosign_fetcher_realregistry_test.go
// -------------------------------------------------------------
// That file builds the signature image itself (right annotation, right media
// type, real ECDSA round-trip) and is the deterministic coverage/RED vehicle for
// the fetcher. But a fixture we build can only ever prove "our verifier accepts
// what WE think cosign emits". If cosign changes its simple-signing layout,
// annotation key, signature encoding, or default hash, that fixture would keep
// passing while production silently broke. This test closes that gap by driving
// the real binary end to end: `cosign generate-key-pair` → `cosign sign
// --key --tlog-upload=false` (the CN-offline keyed mode: no Fulcio, no Rekor) →
// our fetcher/verifier over the same in-process registry.
//
// It is skipped unless a cosign binary is available (SPECULA_COSIGN_BIN or PATH),
// so `go test -short` and CI without cosign stay green; the bash behavioural gate
// (scripts/trust-oracle-signed.sh) runs it against a real Specula too.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// cosignBin resolves the cosign binary: SPECULA_COSIGN_BIN wins, else PATH.
// Returns "" when unavailable so the caller can t.Skip.
func cosignBin() string {
	if p := os.Getenv("SPECULA_COSIGN_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("cosign"); err == nil {
		return p
	}
	return ""
}

// runCosign executes cosign with COSIGN_PASSWORD="" (unencrypted key, no tty) in
// dir, failing the test on error with the combined output.
func runCosign(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"COSIGN_PASSWORD=",
		"COSIGN_YES=true",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "cosign %v failed: %s", args, string(out))
}

// TestCosign_RealBinary_KeyedSign_Signed drives the real cosign binary: it
// generates a keypair, signs a pushed image with the transparency log DISABLED,
// and asserts the production fetcher+verifier reach TierSigned. A PASS here means
// our understanding of cosign's on-registry format matches the shipped binary's.
func TestCosign_RealBinary_KeyedSign_Signed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-cosign integration test in -short mode")
	}
	bin := cosignBin()
	if bin == "" {
		t.Skip("cosign binary not available (set SPECULA_COSIGN_BIN or install cosign)")
	}

	dir := t.TempDir()
	// 1. Real cosign keypair. Writes cosign.key + cosign.pub into dir.
	runCosign(t, bin, dir, "generate-key-pair")
	pubPath := filepath.Join(dir, "cosign.pub")
	keyPath := filepath.Join(dir, "cosign.key")
	require.FileExists(t, pubPath)
	require.FileExists(t, keyPath)

	// 2. Push a subject image to an in-process (plain-HTTP) registry.
	host := startInProcessRegistry(t)
	const repo = "team/cosign-cli"
	digest := pushRandomImage(t, host, repo)
	imageRef := host + "/" + repo + "@" + digest

	// 3. Real cosign keyed sign, tlog DISABLED (CN-offline mode). --allow-http-
	//    registry because the in-process registry is plain HTTP.
	runCosign(t, bin, dir,
		"sign", "--key", keyPath,
		"--tlog-upload=false",
		"--allow-http-registry=true",
		"--yes",
		imageRef,
	)

	// 4. Production fetcher + verifier over the SAME registry, anchored on the
	//    cosign-generated public key.
	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{pubPath}}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"a signature from the REAL cosign binary must verify and PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"real cosign keyed signature must reach TierSigned through the production path")
}

// TestCosign_RealBinary_WrongKey_Fail proves a real cosign signature is REFUSED
// when the verifier is anchored on a different key — the "malicious mirror
// re-signs with its own key" case, using the real binary's output.
func TestCosign_RealBinary_WrongKey_Fail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-cosign integration test in -short mode")
	}
	bin := cosignBin()
	if bin == "" {
		t.Skip("cosign binary not available (set SPECULA_COSIGN_BIN or install cosign)")
	}

	// Signer's keypair (used to sign) in signDir.
	signDir := t.TempDir()
	runCosign(t, bin, signDir, "generate-key-pair")
	signerKey := filepath.Join(signDir, "cosign.key")

	// A DIFFERENT publisher keypair — only its public half anchors the verifier.
	pubDir := t.TempDir()
	runCosign(t, bin, pubDir, "generate-key-pair")
	publisherPub := filepath.Join(pubDir, "cosign.pub")

	host := startInProcessRegistry(t)
	const repo = "team/cosign-wrongkey"
	digest := pushRandomImage(t, host, repo)
	imageRef := host + "/" + repo + "@" + digest

	runCosign(t, bin, signDir,
		"sign", "--key", signerKey,
		"--tlog-upload=false", "--allow-http-registry=true", "--yes",
		imageRef,
	)

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{publisherPub}}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"a real cosign signature from an untrusted key must FAIL")
	assert.NotEqual(t, artifact.TierSigned, tierIfPass(res),
		"an unverified image must never be recorded as signed")
}
