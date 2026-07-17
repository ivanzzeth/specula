// gpg_decompress_test.go — the index decompression path, which 4f30989 added and
// which until now was only ever exercised by a gzip double.
//
// mirrors.aliyun.com/ubuntu serves dists/noble/main/binary-amd64/Packages.xz and
// 404s the uncompressed Packages, and apt PREFERS .xz — so .xz, not .gz, is the
// codec every real `apt-get update` drives through this code. It had no test.
//
// The 512 MB bomb bound was likewise only a comment: nothing failed if it were
// deleted. These tests pin the bound's enforcement, the constant, and — most
// importantly — the ORDERING guarantee that no unverified bytes are ever fed to
// a decompressor.
package verify

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// xzBytes compresses b with the same library the verifier decodes with.
func xzBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := xz.NewWriter(&buf)
	require.NoError(t, err)
	_, err = zw.Write(b)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// gzBytes compresses b with gzip.
func gzBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(b)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// .xz — the codec real apt actually uses
// ---------------------------------------------------------------------------

// TestGPGVerifier_XzPackages_ChainVerifiesAndPinsPool is the .xz twin of the
// existing gz test. A real `apt-get update` against mirrors.aliyun.com fetches
// main/binary-amd64/Packages.xz (confirmed: InRelease pins that path, and apt
// requests it by-hash); every .deb's SHA256 reaches the pool cache only if this
// index decodes. Without it the .deb fails closed and apt — the PRD §G2 gold
// standard — cannot install anything.
func TestGPGVerifier_XzPackages_ChainVerifiesAndPinsPool(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	poolFilename := "pool/main/h/hello/hello_2.10-3build1_amd64.deb"
	poolDeb := []byte("fake .deb payload for xz chain test\n")
	poolDebPath, poolDebDigest := writeQuarantine(t, poolDeb)
	poolDebHex := poolDebDigest[7:]

	// The index as a real mirror serves it: xz-compressed. The SHA256 InRelease
	// pins covers the COMPRESSED bytes.
	indexXz := xzBytes(t, buildPackagesContent(poolFilename, poolDebHex))
	indexPath, indexDigest := writeQuarantine(t, indexXz)
	indexHex := indexDigest[7:]

	sums := []string{
		fmt.Sprintf("%s %d main/binary-amd64/Packages.xz", indexHex, len(indexXz)),
	}
	inReleasePath, inReleaseDigest := writeQuarantine(t, signInRelease(t, key, sums))
	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status, res.Message)

	// The by-hash request real apt issues for that index.
	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/by-hash/SHA256/" + indexHex,
		Digest:  indexDigest, Mutable: true,
	}, &artifact.Artifact{Path: indexPath, Digest: indexDigest})
	require.NoError(t, err)
	require.Equal(t, artifact.StatusPass, res.Status,
		"an xz Packages index must chain-verify — .xz is what apt prefers and what "+
			"aliyun serves: "+res.Message)
	require.Equal(t, artifact.TierSigned, res.Tier, res.Message)

	// And the chain must CONTINUE: the .deb reaches signed only if the xz index
	// was decoded and its pool hashes pinned.
	res, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "main/h/hello",
		Version: "hello_2.10-3build1_amd64.deb",
		Digest:  poolDebDigest, Mutable: false,
	}, &artifact.Artifact{Path: poolDebPath, Digest: poolDebDigest})
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res.Status,
		"the .deb must reach signed through an xz-decoded Packages index "+
			"(InRelease→Packages.xz→.deb): "+res.Message)
	assert.Equal(t, artifact.TierSigned, res.Tier, res.Message)
}

// ---------------------------------------------------------------------------
// Ordering: never decompress unverified bytes
// ---------------------------------------------------------------------------

// TestGPGVerifier_DigestMismatch_DoesNotDecompress pins the ordering that makes
// bounded decompression safe in the first place: the SHA256 pinned by the
// GPG-verified InRelease is checked BEFORE any decoder touches the bytes.
//
// A mirror that serves an index the signature does not cover must be rejected on
// the digest — the bomb never reaches the decompressor. If the two steps were
// ever reordered, an attacker would get free rein of the xz decoder with bytes
// nothing has vouched for, and the 512 MB cap would be the ONLY thing between a
// hostile mirror and the heap.
func TestGPGVerifier_DigestMismatch_DoesNotDecompress(t *testing.T) {
	key := newAptTestKey(t)
	v, err := NewGPGVerifier(key.keyFile)
	require.NoError(t, err)
	ctx := context.Background()

	// InRelease pins some index the mirror will NOT serve.
	honestIndex := xzBytes(t, buildPackagesContent("pool/main/h/hello/hello.deb",
		strings.Repeat("ab", 32)))
	_, honestDigest := writeQuarantine(t, honestIndex)
	sums := []string{
		fmt.Sprintf("%s %d main/binary-amd64/Packages.xz", honestDigest[7:], len(honestIndex)),
	}
	inReleasePath, inReleaseDigest := writeQuarantine(t, signInRelease(t, key, sums))
	_, err = v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu", Version: "noble/InRelease",
		Digest: inReleaseDigest, Mutable: true,
	}, &artifact.Artifact{Path: inReleasePath, Digest: inReleaseDigest})
	require.NoError(t, err)

	// The mirror instead serves a gzip bomb at the pinned path. Not what the
	// signature covers — and, critically, not something we should decode to find
	// out. 100 MB of zeros compresses to ~100 KB.
	bomb := gzBytes(t, make([]byte, 100<<20))
	bombPath, bombDigest := writeQuarantine(t, bomb)

	res, err := v.Verify(ctx, artifact.ArtifactRef{
		Protocol: "apt", Name: "ubuntu",
		Version: "noble/main/binary-amd64/Packages.xz",
		Digest:  bombDigest, Mutable: true,
	}, &artifact.Artifact{Path: bombPath, Digest: bombDigest})
	require.NoError(t, err)

	assert.Equal(t, artifact.StatusFail, res.Status, res.Message)
	assert.Contains(t, res.Message, "SHA256 mismatch",
		"the index must be rejected on the InRelease-pinned digest, BEFORE any "+
			"decompression is attempted — decompressing first would hand an unverified, "+
			"mirror-chosen stream to the decoder. Got: "+res.Message)
	assert.NotContains(t, res.Message, "decompress",
		"reaching the decompressor at all means unverified bytes were decoded: "+res.Message)
}

// ---------------------------------------------------------------------------
// The 512 MB bomb bound
// ---------------------------------------------------------------------------

// TestDecompressIndex_LimitIs512MB pins the production cap. maxIndexPlaintextBytes
// is what decompressIndex passes to decompressIndexLimit; the enforcement tests
// below use a small injected cap so they need no 512 MB allocation, and this test
// is what keeps the two connected.
func TestDecompressIndex_LimitIs512MB(t *testing.T) {
	assert.Equal(t, int64(512*1024*1024), maxIndexPlaintextBytes,
		"noble/main/binary-amd64/Packages is ~50 MB of plaintext; the cap must stay "+
			"generous enough for the largest real suite and small enough to bound a bomb")
}

// TestDecompressIndexLimit_BombRejected proves the bound is enforced, not merely
// described in a comment: a stream that decodes past the cap is refused rather
// than buffered into the heap.
func TestDecompressIndexLimit_BombRejected(t *testing.T) {
	const limit = int64(1 << 20) // 1 MB stand-in for the 512 MB production cap

	for _, tc := range []struct {
		name, ext string
		compress  func(*testing.T, []byte) []byte
	}{
		{"gzip", ".gz", gzBytes},
		{"xz", ".xz", xzBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 4x the cap of highly-compressible zeros — a decompression bomb in
			// miniature.
			bomb := tc.compress(t, make([]byte, limit*4))
			assert.Less(t, int64(len(bomb)), limit,
				"sanity: the bomb must be small compressed, that is the whole attack")

			plain, err := decompressIndexLimit("main/binary-amd64/Packages"+tc.ext, bomb, limit)

			require.Error(t, err,
				"a stream decoding to %d bytes under a %d-byte cap must be refused: the "+
					"cap is what stops a hostile mirror's index from becoming an OOM",
				limit*4, limit)
			assert.Contains(t, err.Error(), "decompression bomb")
			assert.Nil(t, plain, "no plaintext may be returned alongside a bomb rejection")
		})
	}
}

// TestDecompressIndexLimit_ExactlyAtLimit_Accepted is the boundary. An index whose
// plaintext is exactly the cap has NOT exceeded it, and the error text says
// "exceeds". Rejecting it contradicts the message and would, at the real 512 MB
// value, refuse a legitimate suite for being precisely at the line.
func TestDecompressIndexLimit_ExactlyAtLimit_Accepted(t *testing.T) {
	const limit = int64(64 << 10)
	payload := bytes.Repeat([]byte("x"), int(limit))

	plain, err := decompressIndexLimit("main/binary-amd64/Packages.xz", xzBytes(t, payload), limit)

	require.NoError(t, err,
		"a plaintext of exactly %d bytes does not EXCEED a %d-byte cap; refusing it "+
			"makes the error message a lie and drops a legitimate index", limit, limit)
	assert.Equal(t, payload, plain)
}

// TestDecompressIndexLimit_JustOverLimit_Rejected is the other side of the
// boundary: one byte past the cap is over it.
func TestDecompressIndexLimit_JustOverLimit_Rejected(t *testing.T) {
	const limit = int64(64 << 10)
	payload := bytes.Repeat([]byte("x"), int(limit)+1)

	_, err := decompressIndexLimit("main/binary-amd64/Packages.xz", xzBytes(t, payload), limit)

	require.Error(t, err, "%d bytes exceeds a %d-byte cap", limit+1, limit)
	assert.Contains(t, err.Error(), "decompression bomb")
}

// TestDecompressIndexLimit_UncompressedPassthrough: an index with no compression
// extension is returned as-is, so an uncompressed Packages keeps working.
func TestDecompressIndexLimit_UncompressedPassthrough(t *testing.T) {
	raw := []byte("Package: hello\n")
	plain, err := decompressIndexLimit("main/binary-amd64/Packages", raw, maxIndexPlaintextBytes)
	require.NoError(t, err)
	assert.Equal(t, raw, plain)
}

// TestDecompressIndexLimit_CorruptStream_Errors: junk at a compressed path is a
// decode error, never silently-passed-through bytes that the RFC2822 parser would
// then read as an empty (i.e. pins-nothing) index.
func TestDecompressIndexLimit_CorruptStream_Errors(t *testing.T) {
	for _, ext := range []string{".gz", ".xz"} {
		t.Run(ext, func(t *testing.T) {
			_, err := decompressIndexLimit("main/binary-amd64/Packages"+ext,
				[]byte("not a compressed stream at all"), maxIndexPlaintextBytes)
			require.Error(t, err)
		})
	}
}
