package verify

// Real-registry integration for the trusted_root / Fulcio-certificate path.
//
// WHY THIS FILE EXISTS
// --------------------
// trustedroot_test.go proves the verification core with a fakeSigFetcher and
// hand-built CosignSignature{CertPEM}. That never exercises:
//
//   1. signaturesFromImage reading `dev.sigstore.cosign/certificate` off the wire
//   2. OCISignatureFetcher → CosignVerifier with TrustedRoot (no Keys)
//   3. the cosign "simple signing" layout that carries BOTH the signature
//      annotation and the Fulcio leaf PEM annotation on the same layer
//
// These tests close that hole the same way cosign_fetcher_realregistry_test.go
// closed it for keyed cosign: in-process OCI registry, real HTTP, production
// fetcher + verifier. The CA/leaf are synthetic (self-signed Fulcio stand-in)
// because a live Fulcio/OIDC dance is out of scope while tlog remains false —
// but the on-registry bytes and production code path are real.
//
// A real `cosign` CLI keyless sign is NOT required here (and is usually
// unreachable offline). If a cosign binary is present, cosign_cli_test.go covers
// keyed layout; certificate-annotation layout is asserted below against the
// same annotation keys cosign itself writes (see sigstore/cosign static.Options).

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// pushCosignSignatureWithCert pushes a cosign-layout signature image that
// includes the Fulcio leaf PEM annotation (and optional chain), matching what
// cosign writes for keyless-style / cert-backed signatures.
func pushCosignSignatureWithCert(t *testing.T, host, repo, imageDigest string, leafKey crypto.Signer, payload, certPEM, chainPEM []byte) {
	t.Helper()

	sv, err := signature.LoadSigner(leafKey, crypto.SHA256)
	require.NoError(t, err)
	sigBytes, err := sv.SignMessage(bytes.NewReader(payload))
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(sigBytes)

	annotations := map[string]string{
		cosignSignatureAnnotation: b64,
	}
	if len(certPEM) > 0 {
		annotations[cosignCertificateAnnotation] = string(certPEM)
	}
	if len(chainPEM) > 0 {
		annotations[cosignChainAnnotation] = string(chainPEM)
	}

	layer := static.NewLayer(payload, types.MediaType(simpleSigningMediaType))
	sigImg, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:       layer,
		Annotations: annotations,
	})
	require.NoError(t, err)
	sigImg = mutate.MediaType(sigImg, types.OCIManifestSchema1)
	sigImg = mutate.ConfigMediaType(sigImg, types.MediaType("application/vnd.oci.image.config.v1+json"))

	sigTag, err := cosignSigTag(imageDigest)
	require.NoError(t, err)
	ref, err := name.NewTag(host + "/" + repo + ":" + sigTag)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, sigImg))
}

// TestOCISignatureFetcher_DiscoversCertificateAnnotation proves the production
// fetcher lifts CertPEM from the registry layer annotation — the seam
// trustedroot_test.go never touches.
func TestOCISignatureFetcher_DiscoversCertificateAnnotation(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	host := startInProcessRegistry(t)
	const repo = "team/cert-annotated"
	digest := pushRandomImage(t, host, repo)
	payload := simpleSigningPayload(digest)
	pushCosignSignatureWithCert(t, host, repo, digest, m.leafKey, payload, m.leafPEM, nil)

	f := NewOCISignatureFetcher([]string{host})
	sigs, err := f.FetchSignatures(context.Background(), ociDigestRef(host, repo, digest))
	require.NoError(t, err)
	require.Len(t, sigs, 1)
	assert.Equal(t, payload, sigs[0].Payload)
	assert.NotEmpty(t, sigs[0].Base64Sig)
	require.NotEmpty(t, sigs[0].CertPEM, "fetcher must discover the Fulcio leaf certificate annotation")
	// Fetcher TrimSpaces annotations; PEM equality is by parseable content.
	gotLeaf, err := parseLeafCertPEM(sigs[0].CertPEM)
	require.NoError(t, err)
	wantLeaf, err := parseLeafCertPEM(m.leafPEM)
	require.NoError(t, err)
	assert.True(t, gotLeaf.Equal(wantLeaf), "discovered leaf must match the pushed Fulcio certificate")
}

// TestCosignVerifier_RealFetcher_TrustedRoot_Signed drives the full production
// path with TrustedRoot only (no Keys): real registry → OCISignatureFetcher
// (including CertPEM) → CosignVerifier → TierSigned.
func TestCosignVerifier_RealFetcher_TrustedRoot_Signed(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	host := startInProcessRegistry(t)
	const repo = "team/trustedroot-signed"
	digest := pushRandomImage(t, host, repo)
	payload := simpleSigningPayload(digest)
	pushCosignSignatureWithCert(t, host, repo, digest, m.leafKey, payload, m.leafPEM, nil)

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: m.rootPath, Tlog: false}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status, "cert-backed over-the-wire signature must PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier, "must reach TierSigned via trusted_root + real fetcher")
	assert.Contains(t, res.Message, "trusted_root")
}

// TestCosignVerifier_RealFetcher_TrustedRoot_WrongCA_Fail: signature + leaf are
// valid for evil's CA, but the verifier trusts a different Fulcio root — must
// FAIL even though the fetcher discovers a complete cert-backed signature.
func TestCosignVerifier_RealFetcher_TrustedRoot_WrongCA_Fail(t *testing.T) {
	good := generateFulcioTestMaterial(t)
	evil := generateFulcioTestMaterial(t)
	host := startInProcessRegistry(t)
	const repo = "team/trustedroot-wrongca"
	digest := pushRandomImage(t, host, repo)
	payload := simpleSigningPayload(digest)
	pushCosignSignatureWithCert(t, host, repo, digest, evil.leafKey, payload, evil.leafPEM, nil)

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: good.rootPath, Tlog: false}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"leaf chaining to an untrusted Fulcio CA must FAIL")
	assert.NotEqual(t, artifact.TierSigned, tierIfPass(res))
}

// TestCosignVerifier_RealFetcher_TrustedRoot_NoCert_Fail: a keyed-layout
// signature (sig annotation only, no certificate) must FAIL when the verifier
// is anchored solely on trusted_root — never silently treat a bare signature as
// Fulcio-attested.
func TestCosignVerifier_RealFetcher_TrustedRoot_NoCert_Fail(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	host := startInProcessRegistry(t)
	const repo = "team/trustedroot-nocert"
	digest := pushRandomImage(t, host, repo)

	// Sign with the leaf key but omit the certificate annotation (keyed layout).
	sv, err := signature.LoadECDSASignerVerifier(m.leafKey, crypto.SHA256)
	require.NoError(t, err)
	pushCosignSignature(t, host, repo, digest, sv, simpleSigningPayload(digest))

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: m.rootPath, Tlog: false}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"signature without Fulcio certificate annotation must FAIL trusted_root-only policy")
	assert.NotEqual(t, artifact.TierSigned, tierIfPass(res))
}

// TestCosignVerifier_RealFetcher_TrustedRootAndKeys_PrefersKey proves that when
// both anchors are configured, a keyed signature (no cert) still reaches signed
// via the public key — trusted_root does not break the existing keyed path.
func TestCosignVerifier_RealFetcher_TrustedRootAndKeys_PrefersKey(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	host := startInProcessRegistry(t)
	const repo = "team/trustedroot-and-keys"
	digest := pushRandomImage(t, host, repo)

	sv, pubPEM := signerAndPubPEM(t)
	pushCosignSignature(t, host, repo, digest, sv, simpleSigningPayload(digest))

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{
		Keys:        []string{pubPEM},
		TrustedRoot: m.rootPath,
		Tlog:        false,
	}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status)
	assert.Equal(t, artifact.TierSigned, res.Tier)
	assert.Contains(t, res.Message, "configured key")
}
