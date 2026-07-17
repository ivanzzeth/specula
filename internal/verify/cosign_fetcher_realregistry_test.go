package verify

// Integration tests for the REAL cosign signature-discovery transport
// (cosign_fetcher.go: OCISignatureFetcher / signaturesFromImage / cosignSigTag /
// registryHost) driven against an in-process OCI registry.
//
// WHY THIS FILE EXISTS
// --------------------
// Every other cosign test (cosign_test.go) injects a *fakeSigFetcher* and hands
// the verifier a hand-built CosignSignature. That exercises the verification
// core but NEVER runs cosign_fetcher.go — the code that actually resolves the
// `sha256-<hex>.sig` companion tag over the wire and lifts the signature out of
// the manifest-layer annotation. Mutation testing (HEAD 96ad163) reported
// cosign_fetcher.go entirely NOT COVERED for exactly this reason: the double
// answered whatever the code asked, so a broken fetcher would still pass.
//
// These tests close that hole. They stand up go-containerregistry's in-process
// registry (real HTTP, real manifest/layer storage), push a signature image in
// cosign's ACTUAL "simple signing" layout (payload layer + the
// `dev.cosignproject.cosign/signature` annotation, media type
// application/vnd.dev.cosign.simplesigning.v1+json — the constants cosign itself
// uses, see sigstore/cosign/v2/pkg/oci/static), and then run the PRODUCTION
// OCISignatureFetcher + CosignVerifier over it end to end.
//
// The signature bytes are produced with a real ECDSA P-256 key round-trip, so a
// PASS means the discovered-over-the-wire signature actually verifies. The one
// thing not exercised here is the cosign *CLI*'s output format — that is proved
// separately in cosign_cli_test.go against the real binary. Here the fetcher is
// the subject under test; there the format is.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// simpleSigningMediaType is the media type cosign uses for the signed payload
// layer (sigstore/cosign/v2/pkg/types.SimpleSigningMediaType). Mirrored as a
// literal so this test does not import the multi-MB cosign/v2 tree the verifier
// deliberately avoids.
const simpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"

// startInProcessRegistry stands up an in-memory OCI registry and returns its
// bare host (127.0.0.1:PORT). It is torn down at test end.
func startInProcessRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

// pushRandomImage pushes a small random image to host/repo and returns its
// digest ("sha256:<hex>"). This is the "subject" the signature commits to.
func pushRandomImage(t *testing.T, host, repo string) string {
	t.Helper()
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	ref, err := name.NewRepository(host + "/" + repo)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref.Tag("v1"), img))
	dig, err := img.Digest()
	require.NoError(t, err)
	return dig.String()
}

// simpleSigningPayload is the JSON document cosign signs: it commits to the
// image's manifest digest. Content mirrors cosign's simple-signing envelope.
func simpleSigningPayload(imageDigest string) []byte {
	return []byte(fmt.Sprintf(
		`{"critical":{"identity":{"docker-reference":""},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`,
		imageDigest,
	))
}

// pushCosignSignature builds a cosign-layout signature image over payload signed
// by sv and pushes it to the companion `sha256-<hex>.sig` tag for imageDigest.
// This is precisely the layout OCISignatureFetcher must discover.
func pushCosignSignature(t *testing.T, host, repo, imageDigest string, sv signature.Signer, payload []byte) {
	t.Helper()

	sigBytes, err := sv.SignMessage(bytes.NewReader(payload))
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(sigBytes)

	layer := static.NewLayer(payload, types.MediaType(simpleSigningMediaType))
	sigImg, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer: layer,
		Annotations: map[string]string{
			cosignSignatureAnnotation: b64,
		},
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

// signerAndPubPEM generates an ECDSA P-256 keypair and writes its PKIX public
// key to a temp PEM file (the form NewCosignVerifier loads). Returns the signer
// and the PEM path.
func signerAndPubPEM(t *testing.T) (signature.Signer, string) {
	t.Helper()
	sv, _, err := signature.NewDefaultECDSASignerVerifier()
	require.NoError(t, err)
	pub, err := sv.PublicKey()
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	f, err := os.CreateTemp(t.TempDir(), "cosign-pub-*.pem")
	require.NoError(t, err)
	_, err = f.Write(pemBytes)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return sv, f.Name()
}

// ociDigestRef builds the immutable OCI ref for host/repo@digest.
func ociDigestRef(host, repo, digest string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: protocolOCI,
		Name:     repo,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchSignatures — the real transport
// ─────────────────────────────────────────────────────────────────────────────

// TestOCISignatureFetcher_DiscoversRealSigTag proves the production fetcher
// resolves the companion tag and extracts the payload + annotation from a real
// registry. This is the code path fakeSigFetcher never touches.
func TestOCISignatureFetcher_DiscoversRealSigTag(t *testing.T) {
	host := startInProcessRegistry(t)
	const repo = "team/app"
	digest := pushRandomImage(t, host, repo)

	sv, _ := signerAndPubPEM(t)
	payload := simpleSigningPayload(digest)
	pushCosignSignature(t, host, repo, digest, sv, payload)

	f := NewOCISignatureFetcher([]string{host})
	sigs, err := f.FetchSignatures(context.Background(), ociDigestRef(host, repo, digest))
	require.NoError(t, err)
	require.Len(t, sigs, 1, "fetcher must discover exactly the one pushed signature")
	assert.Equal(t, payload, sigs[0].Payload, "discovered payload must be the signed bytes")
	assert.NotEmpty(t, sigs[0].Base64Sig, "discovered signature annotation must be non-empty")
}

// TestOCISignatureFetcher_UnsignedImage_Empty proves an image with NO companion
// .sig tag yields (empty, nil) — the honest "unsigned" signal, not an error and
// not a fabricated signature.
func TestOCISignatureFetcher_UnsignedImage_Empty(t *testing.T) {
	host := startInProcessRegistry(t)
	const repo = "team/unsigned"
	digest := pushRandomImage(t, host, repo) // no signature pushed

	f := NewOCISignatureFetcher([]string{host})
	sigs, err := f.FetchSignatures(context.Background(), ociDigestRef(host, repo, digest))
	require.NoError(t, err, "a missing .sig tag is 'unsigned', not a transport error")
	assert.Empty(t, sigs, "no signature tag => no signatures discovered")
}

// ─────────────────────────────────────────────────────────────────────────────
// Full loop: real fetcher + real verifier
// ─────────────────────────────────────────────────────────────────────────────

// TestCosignVerifier_RealFetcher_ValidSignature_Signed drives the entire
// production path — OCISignatureFetcher over a real registry into CosignVerifier
// — and asserts a real ECDSA signature reaches TierSigned. A broken fetcher (or
// a broken payload/annotation round-trip) fails here; the fake-fetcher tests
// cannot.
func TestCosignVerifier_RealFetcher_ValidSignature_Signed(t *testing.T) {
	host := startInProcessRegistry(t)
	const repo = "team/signed-app"
	digest := pushRandomImage(t, host, repo)

	sv, pubPEM := signerAndPubPEM(t)
	pushCosignSignature(t, host, repo, digest, sv, simpleSigningPayload(digest))

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{pubPEM}}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status, "valid over-the-wire signature must PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier, "must reach TierSigned via the real fetcher")
}

// TestCosignVerifier_RealFetcher_UnsignedImage_Fail proves the negative through
// the real transport: an unsigned image must NOT reach signed — it FAILs under
// keyed policy (never silently downgraded to a pass).
func TestCosignVerifier_RealFetcher_UnsignedImage_Fail(t *testing.T) {
	host := startInProcessRegistry(t)
	const repo = "team/unsigned-app"
	digest := pushRandomImage(t, host, repo)

	_, pubPEM := signerAndPubPEM(t)
	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{pubPEM}}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status, "unsigned image must FAIL keyed policy")
	assert.NotEqual(t, artifact.TierSigned, tierIfPass(res), "unsigned must never be recorded as signed")
}

// TestCosignVerifier_RealFetcher_WrongKey_Fail proves a signature made by a key
// NOT in the verifier's keyring is refused, even though it is discovered over
// the wire. This is the "malicious mirror re-signs with its own key" case.
func TestCosignVerifier_RealFetcher_WrongKey_Fail(t *testing.T) {
	host := startInProcessRegistry(t)
	const repo = "team/wrongkey-app"
	digest := pushRandomImage(t, host, repo)

	attacker, _ := signerAndPubPEM(t) // signs the image
	_, publisherPub := signerAndPubPEM(t)
	pushCosignSignature(t, host, repo, digest, attacker, simpleSigningPayload(digest))

	fetcher := NewOCISignatureFetcher([]string{host})
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{publisherPub}}, fetcher)
	require.NoError(t, err)

	res, verErr := v.Verify(context.Background(), ociDigestRef(host, repo, digest), ociArtifact(digest))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"signature from an untrusted key must FAIL — a mirror cannot forge the publisher's signature")
}

// tierIfPass reports the tier only when the result passed; otherwise a sentinel
// non-signed tier. Guards the "unsigned must never be signed" assertion from a
// false negative where StatusFail happens to carry TierSigned in its message.
func tierIfPass(res artifact.Result) artifact.Tier {
	if res.Status == artifact.StatusPass {
		return res.Tier
	}
	return artifact.TierChecksum
}
