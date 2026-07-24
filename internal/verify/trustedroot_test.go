package verify

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// fulcioTestMaterial is a self-signed Fulcio-like CA + leaf used for hermetic
// trusted_root / cert-backed cosign tests.
type fulcioTestMaterial struct {
	leafCert *x509.Certificate
	leafKey  *ecdsa.PrivateKey
	leafPEM  []byte
	rootJSON []byte
	rootPath string
}

func generateFulcioTestMaterial(t *testing.T) *fulcioTestMaterial {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"specula-test"}, CommonName: "test-fulcio"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

	doc := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"certificateAuthorities": []map[string]any{
			{
				"uri": "https://fulcio.test.local",
				"certChain": map[string]any{
					"certificates": []map[string]string{
						{"rawBytes": base64.StdEncoding.EncodeToString(caDER)},
					},
				},
				"validFor": map[string]string{
					"start": now.Add(-time.Hour).Format(time.RFC3339),
				},
			},
		},
		"tlogs": []any{},
	}
	rootJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal trusted_root: %v", err)
	}
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "trusted_root.json")
	if err := os.WriteFile(rootPath, rootJSON, 0o600); err != nil {
		t.Fatalf("write trusted_root: %v", err)
	}

	return &fulcioTestMaterial{
		leafCert: leafCert,
		leafKey:  leafKey,
		leafPEM:  leafPEM,
		rootJSON: rootJSON,
		rootPath: rootPath,
	}
}

func (m *fulcioTestMaterial) signPayload(t *testing.T, payload []byte) CosignSignature {
	t.Helper()
	signer, err := signature.LoadECDSASignerVerifier(m.leafKey, crypto.SHA256)
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}
	sig, err := signer.SignMessage(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return CosignSignature{
		Payload:   payload,
		Base64Sig: base64.StdEncoding.EncodeToString(sig),
		CertPEM:   m.leafPEM,
	}
}

func TestParseTrustedRoot_OK(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	tr, err := ParseTrustedRoot(m.rootJSON)
	if err != nil {
		t.Fatalf("ParseTrustedRoot: %v", err)
	}
	if tr.Roots == nil || len(tr.Authorities) != 1 {
		t.Fatalf("unexpected trusted root: %+v", tr)
	}
	if err := tr.VerifyCertificateChain(m.leafCert, nil, time.Now().UTC()); err != nil {
		t.Fatalf("VerifyCertificateChain: %v", err)
	}
}

func TestParseTrustedRoot_RejectsEmptyCAs(t *testing.T) {
	_, err := ParseTrustedRoot([]byte(`{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1","certificateAuthorities":[]}`))
	if err == nil {
		t.Fatal("expected error for empty certificateAuthorities")
	}
}

func TestNewCosignVerifier_TrustedRootOnly(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: m.rootPath, Tlog: false}, &fakeSigFetcher{})
	if err != nil {
		t.Fatalf("NewCosignVerifier: %v", err)
	}
	if v.trustedRoot == nil {
		t.Fatal("expected trustedRoot loaded")
	}
	if len(v.verifiers) != 0 {
		t.Fatalf("expected no keyed verifiers, got %d", len(v.verifiers))
	}
}

func TestNewCosignVerifier_RejectsEmptyAnchors(t *testing.T) {
	_, err := NewCosignVerifier(CosignConfig{Tlog: false}, nil)
	if err == nil {
		t.Fatal("expected error when neither keys nor trusted_root set")
	}
}

func TestCosignVerifier_TrustedRoot_Pass(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:abc"}}}`)
	sig := m.signPayload(t, payload)
	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: m.rootPath, Tlog: false}, fetcher)
	if err != nil {
		t.Fatalf("NewCosignVerifier: %v", err)
	}
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "example/app", Version: "sha256:abc", Digest: "sha256:abc"}
	art := ociArtifactCT(ref.Digest, "application/vnd.oci.image.manifest.v1+json")
	res, err := v.Verify(context.Background(), ref, art)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != artifact.StatusPass || res.Tier != artifact.TierSigned {
		t.Fatalf("want Pass/Signed, got %+v", res)
	}
	if !strings.Contains(res.Message, "trusted_root") {
		t.Fatalf("message should mention trusted_root: %s", res.Message)
	}
}

func TestCosignVerifier_TrustedRoot_RejectsWrongCA(t *testing.T) {
	good := generateFulcioTestMaterial(t)
	evil := generateFulcioTestMaterial(t)
	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:abc"}}}`)
	sig := evil.signPayload(t, payload)
	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: good.rootPath, Tlog: false}, fetcher)
	if err != nil {
		t.Fatalf("NewCosignVerifier: %v", err)
	}
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "example/app", Version: "sha256:abc", Digest: "sha256:abc"}
	art := ociArtifactCT(ref.Digest, "application/vnd.oci.image.manifest.v1+json")
	res, err := v.Verify(context.Background(), ref, art)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != artifact.StatusFail {
		t.Fatalf("want Fail against wrong CA, got %+v", res)
	}
}

func TestCosignVerifier_TrustedRoot_RejectsMissingCert(t *testing.T) {
	m := generateFulcioTestMaterial(t)
	payload := []byte(`payload`)
	sig := m.signPayload(t, payload)
	sig.CertPEM = nil
	fetcher := &fakeSigFetcher{sigs: []CosignSignature{sig}}
	v, err := NewCosignVerifier(CosignConfig{TrustedRoot: m.rootPath, Tlog: false}, fetcher)
	if err != nil {
		t.Fatalf("NewCosignVerifier: %v", err)
	}
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "example/app", Version: "sha256:abc", Digest: "sha256:abc"}
	art := ociArtifact(ref.Digest)
	res, err := v.Verify(context.Background(), ref, art)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != artifact.StatusFail {
		t.Fatalf("want Fail without cert annotation, got %+v", res)
	}
}
