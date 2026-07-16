package verify

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Handler-integration seam note
// ─────────────────────────────────────────────────────────────────────────────
//
// The SignatureFetcher interface is the seam between this pure verification
// core and the OCI registry transport layer.  In production the implementation
// (internal/handler/oci or a shared transport helper) resolves the
// sha256-<hex>.sig companion tag via go-containerregistry, reads each layer
// blob as the Payload, and lifts the
// "dev.cosignproject.cosign/signature" layer annotation as Base64Sig.
//
// These unit tests exercise the verification core in full — key loading, the
// full ECDSA sign→verify round-trip, wrong-key rejection, tampered-payload
// detection, the nil-fetcher fail-closed guard, and the self-gating logic —
// using an in-process fake fetcher.  They deliberately avoid any OCI registry
// network call so they run hermetically.

// ─────────────────────────────────────────────────────────────────────────────
// Test infrastructure
// ─────────────────────────────────────────────────────────────────────────────

// cosignTestKey holds one ECDSA P-256 keypair written to a temp PEM file.
// The signer is used to produce CosignSignature fixtures; the PEM file path
// is passed to NewCosignVerifier so the real key-loading path is exercised.
type cosignTestKey struct {
	sv      *signature.ECDSASignerVerifier
	pemFile string // temp file holding the PKIX public key PEM
}

// newCosignTestKey generates a fresh P-256 ECDSA keypair, serialises the
// public key as a PKIX PEM file in t.TempDir(), and returns the bundle.
func newCosignTestKey(t *testing.T) *cosignTestKey {
	t.Helper()
	sv, _, err := signature.NewDefaultECDSASignerVerifier()
	require.NoError(t, err, "generate ECDSA P-256 keypair")

	pub, err := sv.PublicKey()
	require.NoError(t, err)

	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)

	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	f, err := os.CreateTemp(t.TempDir(), "cosign-pub-*.pem")
	require.NoError(t, err)
	_, err = f.Write(pubPEM)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return &cosignTestKey{sv: sv, pemFile: f.Name()}
}

// makeSignature signs payload with this key and returns a CosignSignature
// whose Base64Sig uses StdEncoding — matching the decoder in cosign.go.
func (k *cosignTestKey) makeSignature(t *testing.T, payload []byte) CosignSignature {
	t.Helper()
	sigBytes, err := k.sv.SignMessage(bytes.NewReader(payload))
	require.NoError(t, err)
	return CosignSignature{
		Payload:   append([]byte(nil), payload...), // defensive copy
		Base64Sig: base64.StdEncoding.EncodeToString(sigBytes),
	}
}

// fakeSigFetcher is an injectable SignatureFetcher for unit tests.
type fakeSigFetcher struct {
	sigs []CosignSignature
	err  error
}

func (f *fakeSigFetcher) FetchSignatures(_ context.Context, _ artifact.ArtifactRef) ([]CosignSignature, error) {
	return f.sigs, f.err
}

// immutableOCIRef is a fully-resolved, immutable OCI ref that passes the
// CosignVerifier self-gate (protocol=="oci", !mutable).
func immutableOCIRef() artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "registry.example.com/myapp",
		Version:  "sha256:cafebabe00000000",
		Digest:   "sha256:cafebabe00000000",
		Mutable:  false,
	}
}

// ociArtifact returns an *artifact.Artifact with the given digest (simulates a
// quarantined, digest-resolved OCI image handed to the verify chain).
func ociArtifact(digest string) *artifact.Artifact {
	return &artifact.Artifact{
		Path:   "/quarantine/oci-test-blob",
		Digest: digest,
		Size:   4096,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Constructor validation
// ─────────────────────────────────────────────────────────────────────────────

func TestNewCosignVerifier_RejectsTlog(t *testing.T) {
	key := newCosignTestKey(t)
	_, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}, Tlog: true}, nil)
	require.Error(t, err, "tlog:true must be rejected")
	assert.Contains(t, err.Error(), "tlog")
}

func TestNewCosignVerifier_RejectsEmptyKeys(t *testing.T) {
	_, err := NewCosignVerifier(CosignConfig{Keys: nil, Tlog: false}, nil)
	require.Error(t, err, "empty key list must be rejected")
	assert.Contains(t, err.Error(), "key")
}

func TestNewCosignVerifier_RejectsMissingKeyFile(t *testing.T) {
	_, err := NewCosignVerifier(CosignConfig{
		Keys: []string{"/nonexistent/path/cosign.pub"},
		Tlog: false,
	}, nil)
	require.Error(t, err, "unreadable key file must be rejected")
}

func TestNewCosignVerifier_RejectsInvalidPEM(t *testing.T) {
	dir := t.TempDir()
	badFile := dir + "/bad.pem"
	require.NoError(t, os.WriteFile(badFile, []byte("not a PEM at all"), 0o600))
	_, err := NewCosignVerifier(CosignConfig{Keys: []string{badFile}, Tlog: false}, nil)
	require.Error(t, err, "invalid PEM must be rejected")
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface contracts
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_Interface(t *testing.T) {
	key := newCosignTestKey(t)
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, nil)
	require.NoError(t, err)

	assert.Equal(t, "cosign", v.Name(), "Name must be 'cosign'")
	assert.Equal(t, artifact.TierSigned, v.Tier(), "Tier must be TierSigned")

	var _ Verifier = v // compile-time interface check replicated at run-time
}

// ─────────────────────────────────────────────────────────────────────────────
// Self-gating: skipped paths must return StatusPass without crashing
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_SelfGate(t *testing.T) {
	key := newCosignTestKey(t)
	// nil fetcher is fine here: self-gate fires before the fetcher is called.
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, nil)
	require.NoError(t, err)
	ctx := context.Background()

	tests := []struct {
		name string
		ref  artifact.ArtifactRef
		art  *artifact.Artifact
	}{
		{
			name: "non-oci protocol is skipped",
			ref:  makeRef("pypi", "requests", "2.31.0", "sha256:abc", false),
			art:  ociArtifact("sha256:abc"),
		},
		{
			name: "mutable oci ref is skipped",
			ref:  makeRef("oci", "nginx", "latest", "", true),
			art:  ociArtifact("sha256:abc"),
		},
		{
			name: "empty art.Digest is skipped",
			ref:  makeRef("oci", "nginx", "1.25.0", "sha256:abc", false),
			art:  ociArtifact(""),
		},
		{
			name: "npm protocol is skipped",
			ref:  makeRef("npm", "lodash", "4.17.21", "sha512:abc", false),
			art:  ociArtifact("sha512:abc"),
		},
		{
			name: "gomod protocol is skipped",
			ref:  makeRef("gomod", "example.com/pkg", "v1.0.0", "h1:abc", false),
			art:  ociArtifact("h1:abc"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := v.Verify(ctx, tc.ref, tc.art)
			require.NoError(t, err, "self-gate path must never error")
			assert.Equal(t, artifact.StatusPass, res.Status, "self-gate must return StatusPass")
			// The tier reported on a skip is TierChecksum (downgrade from TierSigned),
			// signalling that no signature was checked rather than that one passed.
			assert.Equal(t, artifact.TierChecksum, res.Tier, "self-gate must report TierChecksum (no sig checked)")
			assert.Contains(t, res.Message, "skipped")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fail-closed guards (nil fetcher, fetcher error)
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_NilFetcher_FailClosed(t *testing.T) {
	key := newCosignTestKey(t)
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, nil /* nil fetcher */)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	// Nil fetcher must fail closed: a non-nil error AND StatusFail.
	require.Error(t, verErr, "nil fetcher must produce a non-nil error (fail-closed)")
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Equal(t, artifact.TierSigned, res.Tier, "fail tier must be TierSigned")
	assert.Contains(t, res.Message, "fail-closed")
}

func TestCosignVerifier_FetcherError_Fail(t *testing.T) {
	key := newCosignTestKey(t)
	fetcher := &fakeSigFetcher{err: errors.New("registry unreachable")}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr, "fetcher error must not propagate as a Go error (Verify handles it)")
	assert.Equal(t, artifact.StatusFail, res.Status)
	assert.Contains(t, res.Message, "fetch")
}

// ─────────────────────────────────────────────────────────────────────────────
// No attached signature → StatusFail (not a crash or StatusPass)
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_NoSignatures_Fail(t *testing.T) {
	key := newCosignTestKey(t)
	// Fetcher returns empty slice — image is unsigned.
	fetcher := &fakeSigFetcher{sigs: nil}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status, "unsigned image must FAIL under keyed policy")
	assert.Equal(t, artifact.TierSigned, res.Tier)
	assert.Contains(t, res.Message, "no cosign signature")
}

// ─────────────────────────────────────────────────────────────────────────────
// Core verification: valid signature → StatusPass (TierSigned)
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_ValidSignature_Pass(t *testing.T) {
	key := newCosignTestKey(t)

	// Build a cosign simple-signing payload (JSON stub; content is arbitrary for
	// the signature-core test — what matters is the round-trip sign→verify).
	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"},"type":"cosign container image signature"},"optional":null}`)
	sig := key.makeSignature(t, payload)

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status, "valid signature must PASS")
	assert.Equal(t, artifact.TierSigned, res.Tier, "valid signature must reach TierSigned")
	assert.Contains(t, res.Message, "verified")
	assert.Contains(t, res.Message, "tlog disabled")
}

// ─────────────────────────────────────────────────────────────────────────────
// Wrong key: signature produced by key B, verifier loaded with key A → FAIL
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_WrongKey_Fail(t *testing.T) {
	keyA := newCosignTestKey(t) // verifier key
	keyB := newCosignTestKey(t) // signing key (different keypair)

	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"}}}`)
	// Sign with key B but load only key A in the verifier.
	sig := keyB.makeSignature(t, payload)

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{keyA.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status, "wrong key must FAIL")
	assert.Equal(t, artifact.TierSigned, res.Tier)
	assert.Contains(t, res.Message, "configured key")
}

// ─────────────────────────────────────────────────────────────────────────────
// Tampered payload: sig covers payload A but CosignSignature presents payload B
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_TamperedPayload_Fail(t *testing.T) {
	key := newCosignTestKey(t)

	payloadA := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"}}}`)
	payloadB := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:000000000000cafe"}}}`) // different digest

	sigA := key.makeSignature(t, payloadA) // signature commits to payloadA

	// Present the signature for payloadA together with payloadB (tampered).
	tampered := CosignSignature{
		Payload:   payloadB,
		Base64Sig: sigA.Base64Sig, // sig was over payloadA, not payloadB
	}

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{tampered}}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"signature over payloadA must not verify against payloadB (tampered payload must FAIL)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Key rotation: sign with key B, configure [keyA, keyB] → PASS via key B
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_KeyRotation_Pass(t *testing.T) {
	keyA := newCosignTestKey(t)
	keyB := newCosignTestKey(t)

	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"}}}`)
	sig := keyB.makeSignature(t, payload)

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	// Configure both keys: verifier should succeed via keyB even though keyA fails.
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{keyA.pemFile, keyB.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status, "signature valid under one of the configured keys must PASS (key rotation)")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Multiple signatures: first signature is by the wrong key, second is valid
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_MultipleSignatures_SecondValid_Pass(t *testing.T) {
	keyA := newCosignTestKey(t) // verifier key
	keyB := newCosignTestKey(t) // unrelated key (wrong signer)

	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"}}}`)

	wrongSig := keyB.makeSignature(t, payload) // signed by keyB — wrong
	goodSig := keyA.makeSignature(t, payload)  // signed by keyA — correct

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{wrongSig, goodSig}}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{keyA.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	art := ociArtifact(ref.Digest)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"one valid signature among multiple must PASS (first match wins)")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Invalid base64 in Base64Sig: silently skipped, remaining sigs checked
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_InvalidBase64_Skipped(t *testing.T) {
	key := newCosignTestKey(t)

	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:cafebabe00000000"}}}`)
	goodSig := key.makeSignature(t, payload)

	bad := CosignSignature{Payload: payload, Base64Sig: "!!!not-base64!!!"}

	t.Run("invalid base64 alone produces FAIL", func(t *testing.T) {
		fetcher := &fakeSigFetcher{sigs: []CosignSignature{bad}}
		v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
		require.NoError(t, err)
		res, verErr := v.Verify(context.Background(), immutableOCIRef(), ociArtifact("sha256:cafebabe00000000"))
		require.NoError(t, verErr)
		assert.Equal(t, artifact.StatusFail, res.Status,
			"all-invalid-base64 must FAIL (nothing verified)")
	})

	t.Run("invalid base64 first then valid sig produces PASS", func(t *testing.T) {
		fetcher := &fakeSigFetcher{sigs: []CosignSignature{bad, goodSig}}
		v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
		require.NoError(t, err)
		res, verErr := v.Verify(context.Background(), immutableOCIRef(), ociArtifact("sha256:cafebabe00000000"))
		require.NoError(t, verErr)
		assert.Equal(t, artifact.StatusPass, res.Status,
			"invalid base64 is skipped; subsequent valid sig must still PASS")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Verify embeds ref identity in failure messages
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_FailMessage_ContainsRefIdentity(t *testing.T) {
	key := newCosignTestKey(t)
	fetcher := &fakeSigFetcher{sigs: nil} // unsigned → FAIL
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "registry.acme.corp/billing-service",
		Version:  "sha256:deadbeef12345678",
		Mutable:  false,
	}
	art := ociArtifact("sha256:deadbeef12345678")

	res, _ := v.Verify(context.Background(), ref, art)
	assert.Equal(t, artifact.StatusFail, res.Status)
	// The message must name the artifact so an operator can act on it.
	assert.Contains(t, res.Message, ref.Name, "fail message must include image name")
}

// ─────────────────────────────────────────────────────────────────────────────
// Chain integration: CosignVerifier after ChecksumVerifier and TofuVerifier
// ─────────────────────────────────────────────────────────────────────────────

func TestCosignVerifier_InChain_Pass(t *testing.T) {
	key := newCosignTestKey(t)

	const digest = "sha256:cafebabe00000000"
	payload := []byte(fmt.Sprintf(`{"critical":{"image":{"docker-manifest-digest":%q}}}`, digest))
	sig := key.makeSignature(t, payload)

	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	cosignV, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	tofuStore := newFakeTofuStore()
	chain := NewChain(
		NewChecksumVerifier(),
		NewTofuVerifier(tofuStore),
		cosignV,
	)

	ref := immutableOCIRef()
	ref.Digest = digest
	art := ociArtifact(digest)

	res, verErr := chain.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	// ChecksumVerifier: pass (digests match); TofuVerifier: warn (first-lock);
	// CosignVerifier: pass (valid signature). Highest tier = TierSigned.
	assert.NotEqual(t, artifact.StatusFail, res.Status, "chain must not fail on valid cosign sig")
	assert.Equal(t, artifact.TierSigned, res.Tier, "chain must reach TierSigned when cosign passes")
}

func TestCosignVerifier_InChain_NonOCI_Passthrough(t *testing.T) {
	key := newCosignTestKey(t)
	// Nil fetcher: cosign self-gates on non-OCI and must not call the fetcher.
	cosignV, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, nil)
	require.NoError(t, err)

	tofuStore := newFakeTofuStore()
	chain := NewChain(NewChecksumVerifier(), NewTofuVerifier(tofuStore), cosignV)

	const digest = "sha512:aabbccdd"
	ref := artifact.ArtifactRef{
		Protocol: "npm",
		Name:     "express",
		Version:  "4.18.2",
		Digest:   digest,
		Mutable:  false,
	}
	art := makeArt(digest)

	res, verErr := chain.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	// Cosign self-gates on npm and must not be the failing verifier.
	assert.NotEqual(t, artifact.StatusFail, res.Status, "cosign must not block non-OCI artifacts")
	assert.LessOrEqual(t, int(res.Tier), int(artifact.TierTofu),
		"cosign must not falsely attest TierSigned for npm artifacts")
}
