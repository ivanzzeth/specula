package verify

// Tests for HelmProvVerifier (helmprov.go).
//
// Traceable to:
//   - PRD §G2 "Helm (repo): signed — .prov GPG 签名 + keyring; 无 .prov 降级"
//   - DESIGN-REVIEW §1.1: "local keyring → detached/clear GPG signature over
//     the .prov body → .prov body → sha256 of the chart .tgz"
//   - DESIGN-REVIEW §1.1: "A mirror cannot forge the signature without the
//     publisher's private key"
//   - PRD §G2 tier ceiling table: Helm (repo) max tier = signed; absent .prov
//     MUST degrade to checksum tier (warn), never claim signed without verifying.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test infrastructure: GPG key generation and .prov signing
// ─────────────────────────────────────────────────────────────────────────────

// helmTestKey holds a generated GPG entity and the armored public-key file path.
type helmTestKey struct {
	entity  *openpgp.Entity
	keyFile string // temp file holding the armored public key
}

// newHelmTestKey generates a fresh RSA GPG entity, writes the armored public key
// to a temp file, and returns the bundle. The entity's PrivateKey is used by
// signHelmProv to produce test .prov files.
func newHelmTestKey(t *testing.T) *helmTestKey {
	t.Helper()
	entity, err := openpgp.NewEntity("TestPublisher", "Test", "test@example.com", nil)
	require.NoError(t, err, "generate GPG entity")

	var pubBuf bytes.Buffer
	w, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(w))
	require.NoError(t, w.Close())

	f, err := os.CreateTemp(t.TempDir(), "helm-keyring-*.gpg")
	require.NoError(t, err)
	_, err = f.Write(pubBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return &helmTestKey{entity: entity, keyFile: f.Name()}
}

// signHelmProv creates a clear-signed .prov document for the chart file with the
// given SHA256 digest. The plaintext body follows Helm's provenance format:
//
//	files:
//	  <chartFilename>: sha256:<hex>
func signHelmProv(t *testing.T, key *helmTestKey, chartFilename, digestHex string) []byte {
	t.Helper()
	plaintext := fmt.Sprintf("description: Test chart\nfiles:\n  %s: sha256:%s\n", chartFilename, digestHex)

	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, key.entity.PrivateKey, nil)
	require.NoError(t, err, "clearsign.Encode")
	_, err = w.Write([]byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// makeHelmArtifact constructs an artifact.Artifact with the given digest and an
// optional Attachments[0] (the .prov bytes).
func makeHelmArtifact(digest string, provBytes []byte) *artifact.Artifact {
	art := &artifact.Artifact{
		Path:   "/quarantine/mychart-1.0.0.tgz",
		Digest: "sha256:" + digest,
		Size:   4096,
	}
	if len(provBytes) > 0 {
		art.Meta.Attachments = [][]byte{provBytes}
	}
	return art
}

// helmChartRef builds an immutable helm ArtifactRef for a .tgz chart.
func helmChartRef(version string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "helm",
		Name:     "stable/nginx",
		Version:  version,
		Mutable:  false,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NewHelmProvVerifier — constructor errors
// ─────────────────────────────────────────────────────────────────────────────

func TestNewHelmProvVerifier_EmptyKeyringPath(t *testing.T) {
	_, err := NewHelmProvVerifier("")
	require.Error(t, err, "empty keyring path must be rejected")
	assert.Contains(t, err.Error(), "keyring path is required")
}

func TestNewHelmProvVerifier_MissingFile(t *testing.T) {
	_, err := NewHelmProvVerifier("/nonexistent/path/keyring.gpg")
	require.Error(t, err, "missing file must be rejected")
}

func TestNewHelmProvVerifier_InvalidKeyring(t *testing.T) {
	dir := t.TempDir()
	badFile := dir + "/bad.gpg"
	require.NoError(t, os.WriteFile(badFile, []byte("not a gpg keyring"), 0o600))

	_, err := NewHelmProvVerifier(badFile)
	require.Error(t, err, "invalid keyring must be rejected")
}

func TestNewHelmProvVerifier_EmptyKeyring(t *testing.T) {
	dir := t.TempDir()
	emptyFile := dir + "/empty.gpg"
	// A zero-byte file has no GPG keys.
	require.NoError(t, os.WriteFile(emptyFile, []byte{}, 0o600))

	_, err := NewHelmProvVerifier(emptyFile)
	require.Error(t, err, "keyring with no keys must be rejected")
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface
// ─────────────────────────────────────────────────────────────────────────────

func TestHelmProvVerifier_Interface(t *testing.T) {
	key := newHelmTestKey(t)
	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)
	assert.Equal(t, "helmprov", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())
	var _ Verifier = v
}

// ─────────────────────────────────────────────────────────────────────────────
// Self-gating
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_SelfGate verifies that the verifier is a no-op for
// non-helm artifacts, mutable refs, and non-.tgz versions.
func TestHelmProvVerifier_SelfGate(t *testing.T) {
	key := newHelmTestKey(t)
	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	cases := []struct {
		name    string
		ref     artifact.ArtifactRef
		wantMsg string
	}{
		{
			name: "non-helm protocol",
			ref:  makeRef("oci", "nginx", "1.25.0", "sha256:abc", false),
		},
		{
			name: "mutable ref (index.yaml)",
			ref: artifact.ArtifactRef{
				Protocol: "helm",
				Name:     "stable/index.yaml",
				Version:  "index.yaml",
				Mutable:  true,
			},
		},
		{
			name: ".prov file itself (not .tgz)",
			ref: artifact.ArtifactRef{
				Protocol: "helm",
				Name:     "stable/nginx",
				Version:  "nginx-15.0.0.tgz.prov",
				Mutable:  false,
			},
		},
		{
			name: "non-.tgz extension",
			ref: artifact.ArtifactRef{
				Protocol: "helm",
				Name:     "stable/nginx",
				Version:  "nginx-15.0.0.tar.gz",
				Mutable:  false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := makeArt("sha256:abc")
			res, err := v.Verify(ctx, tc.ref, art)
			require.NoError(t, err, "self-gate must not error")
			assert.Equal(t, artifact.StatusPass, res.Status, "self-gate must pass through")
			assert.Equal(t, artifact.TierChecksum, res.Tier, "self-gate must not attest TierSigned")
			assert.Contains(t, res.Message, "skipped")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// No .prov attachment: honest tier downgrade
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_NoProv_DegradesHonestly verifies PRD §G2: when no .prov
// is available the verifier returns StatusWarn/TierChecksum (honest downgrade),
// never claims TierSigned (which would be a lie).
//
// "无 .prov 降级" — absent .prov is a tier downgrade, not a rejection.
func TestHelmProvVerifier_NoProv_DegradesHonestly(t *testing.T) {
	key := newHelmTestKey(t)
	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)

	ref := helmChartRef("nginx-15.0.0.tgz")
	art := makeHelmArtifact("deadbeef", nil /* no prov */)

	res, verErr := v.Verify(context.Background(), ref, art)

	require.NoError(t, verErr, "absent .prov is not an error — it is a tier downgrade")
	assert.Equal(t, artifact.StatusWarn, res.Status, "absent .prov must WARN, not fail")
	assert.Equal(t, artifact.TierChecksum, res.Tier,
		"absent .prov MUST degrade to TierChecksum — never claim TierSigned without a verified signature")
	assert.Contains(t, res.Message, "no .prov")
}

// TestHelmProvVerifier_EmptyProv_DegradesHonestly verifies that an empty
// Attachments[0] slice is treated the same as a missing attachment.
func TestHelmProvVerifier_EmptyProv_DegradesHonestly(t *testing.T) {
	key := newHelmTestKey(t)
	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)

	ref := helmChartRef("nginx-15.0.0.tgz")
	art := makeHelmArtifact("deadbeef", []byte{}) // empty bytes, not nil

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusWarn, res.Status)
	assert.Equal(t, artifact.TierChecksum, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Invalid .prov content
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_InvalidProvFormat_Fail verifies that a .prov attachment
// that is not a valid clear-signed PGP document returns StatusFail.
func TestHelmProvVerifier_InvalidProvFormat_Fail(t *testing.T) {
	key := newHelmTestKey(t)
	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)

	ref := helmChartRef("nginx-15.0.0.tgz")
	art := makeHelmArtifact("deadbeef", []byte("not a prov file at all"))

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status, "invalid .prov format must FAIL")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Wrong key: signature is valid but signed with a different (unknown) key
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_WrongKey_Fail verifies that a .prov signed with a key
// not in the keyring returns StatusFail. This guards against a malicious mirror
// replacing the signature with one from its own key.
func TestHelmProvVerifier_WrongKey_Fail(t *testing.T) {
	publisherKey := newHelmTestKey(t) // only this key is in the keyring
	attackerKey := newHelmTestKey(t)  // signed by this one, not trusted

	const chartFile = "nginx-15.0.0.tgz"
	const digestHex = "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"

	// Sign with the attacker's key (not in the publisher's keyring).
	provBytes := signHelmProv(t, attackerKey, chartFile, digestHex)

	v, err := NewHelmProvVerifier(publisherKey.keyFile)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	art := makeHelmArtifact(digestHex, provBytes)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"signature from untrusted key MUST FAIL — mirror cannot forge publisher's signature")
	assert.Equal(t, artifact.TierSigned, res.Tier)
}

// ─────────────────────────────────────────────────────────────────────────────
// Valid signature but digest mismatch
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_ValidSig_DigestMismatch_Fail verifies that a valid GPG
// signature whose .prov contains a DIFFERENT digest than the artifact fails.
// This catches the scenario where the .prov was signed for a different chart file.
func TestHelmProvVerifier_ValidSig_DigestMismatch_Fail(t *testing.T) {
	key := newHelmTestKey(t)

	const chartFile = "nginx-15.0.0.tgz"
	const signedDigestHex = "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"
	const actualDigestHex = "deadbeef0000000000000000deadbeef0000000000000000deadbeef00000000" // different!

	// Sign a .prov that commits to signedDigestHex, but the artifact has actualDigestHex.
	provBytes := signHelmProv(t, key, chartFile, signedDigestHex)

	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	art := makeHelmArtifact(actualDigestHex, provBytes)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"digest mismatch between .prov and artifact MUST FAIL")
	assert.Equal(t, artifact.TierSigned, res.Tier)
	assert.Contains(t, res.Message, "mismatch")
}

// ─────────────────────────────────────────────────────────────────────────────
// Full valid verification: GPG sig + digest match → TierSigned
// ─────────────────────────────────────────────────────────────────────────────

// TestHelmProvVerifier_ValidSigAndDigest_Pass verifies the golden path:
// a .prov that is clear-signed with the publisher's key AND whose digest matches
// the artifact must return StatusPass/TierSigned.
//
// This is the trust anchor: local keyring → .prov GPG sig → chart SHA256.
func TestHelmProvVerifier_ValidSigAndDigest_Pass(t *testing.T) {
	key := newHelmTestKey(t)

	const chartFile = "nginx-15.0.0.tgz"
	const digestHex = "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"

	provBytes := signHelmProv(t, key, chartFile, digestHex)

	v, err := NewHelmProvVerifier(key.keyFile)
	require.NoError(t, err)

	ref := helmChartRef(chartFile)
	art := makeHelmArtifact(digestHex, provBytes)

	res, verErr := v.Verify(context.Background(), ref, art)
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"valid signature + matching digest MUST PASS at TierSigned")
	assert.Equal(t, artifact.TierSigned, res.Tier,
		"passing helmprov check must reach TierSigned (real publisher authenticity)")
	assert.Contains(t, res.Message, "verified")
}

// ─────────────────────────────────────────────────────────────────────────────
// extractChartDigest (internal helper)
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractChartDigest(t *testing.T) {
	const validHex = "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"

	t.Run("standard prov body", func(t *testing.T) {
		body := []byte(fmt.Sprintf("description: Nginx chart\nfiles:\n  nginx-15.0.0.tgz: sha256:%s\n", validHex))
		digest, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.NoError(t, err)
		assert.Equal(t, "sha256:"+validHex, digest)
	})

	t.Run("multiple files in block", func(t *testing.T) {
		body := []byte(fmt.Sprintf(
			"files:\n  other-chart-1.0.0.tgz: sha256:%s\n  nginx-15.0.0.tgz: sha256:%s\n",
			strings.Repeat("a", 64), validHex,
		))
		digest, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.NoError(t, err)
		assert.Equal(t, "sha256:"+validHex, digest)
	})

	t.Run("file not found in block", func(t *testing.T) {
		body := []byte("files:\n  other.tgz: sha256:" + validHex + "\n")
		_, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.Error(t, err, "missing chart entry must return error")
		assert.Contains(t, err.Error(), "nginx-15.0.0.tgz")
	})

	t.Run("no files block", func(t *testing.T) {
		body := []byte("description: chart without files section\n")
		_, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.Error(t, err)
	})

	t.Run("non-sha256 digest format", func(t *testing.T) {
		body := []byte("files:\n  nginx-15.0.0.tgz: md5:abcdef\n")
		_, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.Error(t, err, "non-sha256 digest must be rejected")
		assert.Contains(t, err.Error(), "sha256")
	})

	t.Run("windows line endings (CRLF)", func(t *testing.T) {
		body := []byte("files:\r\n  nginx-15.0.0.tgz: sha256:" + validHex + "\r\n")
		digest, err := extractChartDigest(body, "nginx-15.0.0.tgz")
		require.NoError(t, err, "CRLF line endings must be handled")
		assert.Equal(t, "sha256:"+validHex, digest)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// digestsMatch (internal helper)
// ─────────────────────────────────────────────────────────────────────────────

func TestDigestsMatch(t *testing.T) {
	const hex = "cafebabe0000000000000000cafebabe0000000000000000cafebabe00000000"
	tests := []struct {
		d1, d2 string
		want   bool
	}{
		{"sha256:" + hex, "sha256:" + hex, true},
		{hex, "sha256:" + hex, true}, // bare hex matches prefixed
		{"sha256:" + hex, hex, true}, // prefix on left side only
		{hex, hex, true},             // both bare
		{"sha256:" + hex, "sha256:other", false},
		{"", "sha256:" + hex, false}, // empty is never equal
		{"sha256:" + hex, "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q/%q", tc.d1[:min(len(tc.d1), 10)], tc.d2[:min(len(tc.d2), 10)]), func(t *testing.T) {
			got := digestsMatch(tc.d1, tc.d2)
			assert.Equal(t, tc.want, got)
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
