package sbom

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

func TestPackageURL_NPM(t *testing.T) {
	assert.Equal(t, "pkg:npm/left-pad@1.3.0", PackageURL(artifact.ArtifactRef{
		Protocol: "npm", Name: "left-pad", Version: "1.3.0",
	}))
	assert.Equal(t, "pkg:npm/%40types/node@20.0.0", PackageURL(artifact.ArtifactRef{
		Protocol: "npm", Name: "@types/node", Version: "20.0.0",
	}))
}

func TestPackageURL_CargoOCI(t *testing.T) {
	assert.Equal(t, "pkg:cargo/serde@1.0.0", PackageURL(artifact.ArtifactRef{
		Protocol: "cargo", Name: "serde", Version: "1.0.0",
	}))
	assert.Equal(t, "pkg:oci/library/nginx@sha256:abc", PackageURL(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Digest: "sha256:abc",
	}))
}

func TestFromEntries_SkipsMutableAndSetsHashes(t *testing.T) {
	fixed := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	entries := []meta.Entry{
		{
			CacheEntry: artifact.CacheEntry{
				Ref:    artifact.ArtifactRef{Protocol: "npm", Name: "ms", Version: "2.1.3", Mutable: true},
				Digest: "sha256:dead",
				Tier:   artifact.TierChecksum,
			},
		},
		{
			CacheEntry: artifact.CacheEntry{
				Ref:      artifact.ArtifactRef{Protocol: "npm", Name: "ms", Version: "2.1.3"},
				Digest:   "sha256:abcdef0123456789",
				Tier:     artifact.TierConsensus,
				Upstream: "npmmirror",
			},
		},
	}
	doc := FromEntries(entries, Options{
		SpeculaVersion: "v0.12.0-test",
		SerialNumber:   "urn:uuid:test",
		Now:            func() time.Time { return fixed },
	})
	require.Equal(t, "CycloneDX", doc.BOMFormat)
	require.Equal(t, SpecVersion, doc.SpecVersion)
	require.Equal(t, "urn:uuid:test", doc.SerialNumber)
	require.Equal(t, fixed.Format(time.RFC3339), doc.Metadata.Timestamp)
	require.Len(t, doc.Components, 1)
	c := doc.Components[0]
	assert.Equal(t, "ms", c.Name)
	assert.Equal(t, "pkg:npm/ms@2.1.3", c.PURL)
	require.Len(t, c.Hashes, 1)
	assert.Equal(t, "SHA-256", c.Hashes[0].Alg)
	assert.Equal(t, "abcdef0123456789", c.Hashes[0].Content)
	assert.False(t, doc.Truncated)

	var foundKind bool
	for _, p := range doc.Metadata.Properties {
		if p.Name == "specula:bom-kind" {
			assert.Equal(t, "cache-inventory", p.Value)
			foundKind = true
		}
	}
	assert.True(t, foundKind)
}

func TestFromEntries_Truncated(t *testing.T) {
	entries := make([]meta.Entry, MaxComponents+3)
	for i := range entries {
		entries[i] = meta.Entry{
			CacheEntry: artifact.CacheEntry{
				Ref: artifact.ArtifactRef{
					Protocol: "npm",
					Name:     "pkg",
					Version:  "1.0.0",
				},
				Digest: "sha256:aa",
			},
		}
	}
	doc := FromEntries(entries, Options{})
	assert.Len(t, doc.Components, MaxComponents)
	assert.True(t, doc.Truncated)
}
