package verify

// Tests for the cosign manifest-gating self-gate.
//
// THE BUG THIS GUARDS AGAINST (found by driving the real pull-through path)
// ------------------------------------------------------------------------
// cosign signs an OCI image MANIFEST by digest — never its individual config
// and layer blobs. But in Specula's pull-through path every blob is fetched and
// stored as its own oci, immutable, digest-resolved artifact, indistinguishable
// (by protocol/mutable/digest alone) from the manifest. Without a media-type
// gate the cosign verifier demands a `.sig` companion tag for EACH layer digest;
// none exists, so it fail-closes the chain on the first layer and the entire
// `docker pull` of a correctly-signed image fails. The `signed` tier was, in
// effect, unusable for any real multi-blob image.
//
// The gate restricts cosign to manifest / index artifacts using the
// upstream-reported media type (registries serve blobs as octet-stream). These
// tests pin that behaviour with the fake fetcher, so they are hermetic and run
// under -short.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ociArtifactCT is ociArtifact plus an upstream ContentType (as the pull-through
// path records from the upstream response header).
func ociArtifactCT(digest, contentType string) *artifact.Artifact {
	a := ociArtifact(digest)
	a.Meta.ContentType = contentType
	return a
}

// TestCosignVerifier_SkipsLayerBlob proves cosign SKIPS a layer/config blob
// (served as application/octet-stream) instead of fail-closing on a missing
// per-blob signature. Before the media-type gate this returned StatusFail and
// broke the pull of every signed image on its first layer.
func TestCosignVerifier_SkipsLayerBlob(t *testing.T) {
	key := newCosignTestKey(t)
	// A fetcher that would report "unsigned" for any digest — exactly what a
	// layer blob's non-existent .sig tag yields. If the gate is absent this makes
	// the verifier FAIL; with the gate it must never be consulted.
	fetcher := &fakeSigFetcher{sigs: nil}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	blobTypes := []string{
		"application/octet-stream",
		"application/vnd.oci.image.config.v1+json",
		"application/vnd.docker.container.image.v1+json",
		"application/vnd.oci.image.layer.v1.tar+gzip",
	}
	for _, ct := range blobTypes {
		t.Run(ct, func(t *testing.T) {
			ref := immutableOCIRef()
			res, verErr := v.Verify(context.Background(), ref, ociArtifactCT(ref.Digest, ct))
			require.NoError(t, verErr, "a non-manifest blob must not error")
			assert.Equal(t, artifact.StatusSkip, res.Status,
				"cosign must SKIP a %s blob, not fail-close on a per-blob signature", ct)
			assert.Equal(t, artifact.TierChecksum, res.Tier,
				"a skipped blob must not attest TierSigned")
		})
	}
}

// TestCosignVerifier_RunsOnManifestMediaTypes proves cosign still RUNS on
// manifest and index media types — the artifacts cosign actually signs.
func TestCosignVerifier_RunsOnManifestMediaTypes(t *testing.T) {
	key := newCosignTestKey(t)
	fetcher := &fakeSigFetcher{sigs: nil} // unsigned => the verifier runs and FAILs
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	manifestTypes := []string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json; charset=utf-8", // with params
	}
	for _, ct := range manifestTypes {
		t.Run(ct, func(t *testing.T) {
			ref := immutableOCIRef()
			res, verErr := v.Verify(context.Background(), ref, ociArtifactCT(ref.Digest, ct))
			require.NoError(t, verErr)
			// It RAN (reached the fetcher) => reports the honest "unsigned" FAIL,
			// not a skip.
			assert.Equal(t, artifact.StatusFail, res.Status,
				"cosign must RUN on a %s manifest (here unsigned => FAIL)", ct)
		})
	}
}

// TestCosignVerifier_EmptyContentType_Runs pins the backward-compatible default:
// an empty content type (unit tests, or an upstream that omits the header) is
// treated as "run", since real registries always label blob responses so an
// empty type never denotes a layer in production.
func TestCosignVerifier_EmptyContentType_Runs(t *testing.T) {
	key := newCosignTestKey(t)
	fetcher := &fakeSigFetcher{sigs: nil}
	v, err := NewCosignVerifier(CosignConfig{Keys: []string{key.pemFile}}, fetcher)
	require.NoError(t, err)

	ref := immutableOCIRef()
	res, verErr := v.Verify(context.Background(), ref, ociArtifactCT(ref.Digest, ""))
	require.NoError(t, verErr)
	assert.Equal(t, artifact.StatusFail, res.Status,
		"empty content type must still RUN the verifier (unsigned => FAIL), not skip")
}
