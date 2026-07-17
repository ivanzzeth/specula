package verify

// Tests for cosign_fetcher.go helpers and OCISignatureFetcher construction.
//
// Note: FetchSignatures itself requires a live OCI registry or a complex
// go-containerregistry mock; those paths are exercised via the injectable
// SignatureFetcher interface in cosign_test.go. The tests here cover the
// pure helper functions (cosignSigTag, registryHost) and the constructor
// deduplication logic.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// cosignSigTag
// ─────────────────────────────────────────────────────────────────────────────

// TestCosignSigTag verifies the digest-to-sig-tag conversion.
// Format: "sha256:<hex>" → "sha256-<hex>.sig"
func TestCosignSigTag(t *testing.T) {
	tests := []struct {
		digest  string
		wantTag string
		wantErr bool
	}{
		{
			digest:  "sha256:cafebabe00000000cafebabe00000000cafebabe00000000cafebabe00000000",
			wantTag: "sha256-cafebabe00000000cafebabe00000000cafebabe00000000cafebabe00000000.sig",
		},
		{
			digest:  "sha256:abc123",
			wantTag: "sha256-abc123.sig",
		},
		{
			// Malformed: no colon.
			digest:  "sha256abc",
			wantErr: true,
		},
		{
			// Malformed: colon at position 0.
			digest:  ":abc",
			wantErr: true,
		},
		{
			// Malformed: colon at the very end (empty hex part).
			digest:  "sha256:",
			wantErr: true,
		},
		{
			// Empty digest.
			digest:  "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.digest, func(t *testing.T) {
			got, err := cosignSigTag(tc.digest)
			if tc.wantErr {
				require.Error(t, err, "expected error for digest %q", tc.digest)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTag, got)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// registryHost
// ─────────────────────────────────────────────────────────────────────────────

// TestRegistryHost verifies that configured upstream entries (URLs or bare hosts)
// are normalised to a bare registry host.
func TestRegistryHost(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{
			raw:  "registry-1.docker.io",
			want: "registry-1.docker.io",
		},
		{
			raw:  "https://docker.m.daocloud.io",
			want: "docker.m.daocloud.io",
		},
		{
			raw:  "http://internal-registry.example.com",
			want: "internal-registry.example.com",
		},
		{
			raw:  "https://gcr.io/v2/library/nginx",
			want: "gcr.io",
		},
		{
			raw:  "mirror.example.com/v2",
			want: "mirror.example.com",
		},
		{
			raw:  "",
			want: "",
		},
		{
			raw:  "   ",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got := registryHost(tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NewOCISignatureFetcher
// ─────────────────────────────────────────────────────────────────────────────

// TestNewOCISignatureFetcher_Deduplication verifies that duplicate registry hosts
// are deduplicated so the fetcher does not poll the same host twice.
func TestNewOCISignatureFetcher_Deduplication(t *testing.T) {
	registries := []string{
		"registry-1.docker.io",
		"https://registry-1.docker.io", // same host, different form
		"docker.m.daocloud.io",
	}
	f := NewOCISignatureFetcher(registries)
	require.NotNil(t, f)

	// The deduped list should have exactly 2 hosts (docker.io deduplicated).
	assert.Equal(t, 2, len(f.registries), "duplicate hosts must be removed")
	assert.Contains(t, f.registries, "registry-1.docker.io")
	assert.Contains(t, f.registries, "docker.m.daocloud.io")
}

// TestNewOCISignatureFetcher_EmptyRegistries verifies that an empty list
// produces a fetcher (not an error); FetchSignatures will return an error.
func TestNewOCISignatureFetcher_EmptyRegistries(t *testing.T) {
	f := NewOCISignatureFetcher(nil)
	require.NotNil(t, f, "nil registries must not panic")
	assert.Empty(t, f.registries)
}

// TestNewOCISignatureFetcher_StripsEmptyEntries verifies that empty/whitespace
// entries in the registry list are ignored gracefully.
func TestNewOCISignatureFetcher_StripsEmptyEntries(t *testing.T) {
	registries := []string{"", "  ", "registry.example.com", "", ""}
	f := NewOCISignatureFetcher(registries)
	require.NotNil(t, f)
	assert.Equal(t, 1, len(f.registries), "empty entries must be removed")
	assert.Equal(t, "registry.example.com", f.registries[0])
}

// TestNewOCISignatureFetcher_Interface verifies compile-time interface satisfaction.
func TestNewOCISignatureFetcher_Interface(t *testing.T) {
	f := NewOCISignatureFetcher([]string{"registry.example.com"})
	var _ SignatureFetcher = f // compile-time check
}
