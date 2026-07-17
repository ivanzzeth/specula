package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

// fakeTofuStore is a thread-unsafe in-memory TofuStore suitable for tests.
type fakeTofuStore struct {
	pins   map[string]string
	getErr error
	setErr error
}

func newFakeTofuStore() *fakeTofuStore {
	return &fakeTofuStore{pins: make(map[string]string)}
}

func (s *fakeTofuStore) GetPin(_ context.Context, key string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	return s.pins[key], nil
}

func (s *fakeTofuStore) SetPin(_ context.Context, key, digest string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.pins[key] = digest
	return nil
}

// fakeVerifier is a controllable Verifier used for Chain unit tests.
type fakeVerifier struct {
	name   string
	tier   artifact.Tier
	result artifact.Result
	err    error
}

func (f *fakeVerifier) Name() string        { return f.name }
func (f *fakeVerifier) Tier() artifact.Tier { return f.tier }
func (f *fakeVerifier) Verify(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
	return f.result, f.err
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper builders
// ─────────────────────────────────────────────────────────────────────────────

func makeArt(digest string) *artifact.Artifact {
	return &artifact.Artifact{
		Path:   "/quarantine/test-blob",
		Digest: digest,
		Size:   1024,
	}
}

func makeRef(protocol, name, version, digest string, mutable bool) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: protocol,
		Name:     name,
		Version:  version,
		Digest:   digest,
		Mutable:  mutable,
	}
}

func passVerifier(name string, tier artifact.Tier) *fakeVerifier {
	return &fakeVerifier{
		name:   name,
		tier:   tier,
		result: artifact.Result{Status: artifact.StatusPass, Tier: tier, Message: name + ": pass"},
	}
}

func warnVerifier(name string, tier artifact.Tier) *fakeVerifier {
	return &fakeVerifier{
		name:   name,
		tier:   tier,
		result: artifact.Result{Status: artifact.StatusWarn, Tier: tier, Message: name + ": warn"},
	}
}

func failVerifier(name string, tier artifact.Tier) *fakeVerifier {
	return &fakeVerifier{
		name:   name,
		tier:   tier,
		result: artifact.Result{Status: artifact.StatusFail, Tier: tier, Message: name + ": fail"},
	}
}

func errVerifier(name string, tier artifact.Tier) *fakeVerifier {
	return &fakeVerifier{
		name:   name,
		tier:   tier,
		err:    errors.New("internal verifier error"),
		result: artifact.Result{Status: artifact.StatusFail, Tier: tier},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ChecksumVerifier
// ─────────────────────────────────────────────────────────────────────────────

func TestChecksumVerifier_Interface(t *testing.T) {
	v := NewChecksumVerifier()
	assert.Equal(t, "checksum", v.Name())
	assert.Equal(t, artifact.TierChecksum, v.Tier())
}

func TestChecksumVerifier_Verify(t *testing.T) {
	const goodDigest = "sha256:aabbccddee112233"
	const otherDigest = "sha256:ffeeddccbb998877"

	tests := []struct {
		name        string
		ref         artifact.ArtifactRef
		art         *artifact.Artifact
		wantStatus  artifact.Status
		wantTier    artifact.Tier
		msgContains string
	}{
		{
			name:        "ref digest matches art digest: pass",
			ref:         makeRef("oci", "nginx", "1.25.0", goodDigest, false),
			art:         makeArt(goodDigest),
			wantStatus:  artifact.StatusPass,
			wantTier:    artifact.TierChecksum,
			msgContains: goodDigest,
		},
		{
			name:        "ref digest mismatches art digest: fail",
			ref:         makeRef("oci", "nginx", "1.25.0", goodDigest, false),
			art:         makeArt(otherDigest),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierChecksum,
			msgContains: "mismatch",
		},
		{
			name:        "ref digest empty (mutable tag path): skip with note",
			ref:         makeRef("oci", "nginx", "latest", "", true),
			art:         makeArt(goodDigest),
			wantStatus:  artifact.StatusSkip,
			wantTier:    artifact.TierChecksum,
			msgContains: "no reference",
		},
		{
			name:        "art digest empty: fail",
			ref:         makeRef("oci", "nginx", "1.25.0", goodDigest, false),
			art:         makeArt(""),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierChecksum,
			msgContains: "empty",
		},
		{
			name:        "both digests empty: fail on empty art digest",
			ref:         makeRef("tarball", "example.tar.gz", "", "", false),
			art:         makeArt(""),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierChecksum,
			msgContains: "empty",
		},
		{
			name:        "ref empty and art has non-sha256 digest: skip (nothing to compare)",
			ref:         makeRef("gomod", "example.com/pkg", "v1.0.0", "", false),
			art:         makeArt("h1:abcdefgh"),
			wantStatus:  artifact.StatusSkip,
			wantTier:    artifact.TierChecksum,
			msgContains: "no reference",
		},
		{
			name:        "ref and art both set, same non-sha256 digest: pass",
			ref:         makeRef("gomod", "example.com/pkg", "v1.0.0", "h1:abcdefgh", false),
			art:         makeArt("h1:abcdefgh"),
			wantStatus:  artifact.StatusPass,
			wantTier:    artifact.TierChecksum,
			msgContains: "verified",
		},
		{
			name:        "ref and art both set, different digest: fail",
			ref:         makeRef("npm", "lodash", "4.17.21", "sha512:aaa", false),
			art:         makeArt("sha512:bbb"),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierChecksum,
			msgContains: "mismatch",
		},
	}

	v := NewChecksumVerifier()
	ctx := context.Background()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := v.Verify(ctx, tc.ref, tc.art)
			require.NoError(t, err, "ChecksumVerifier.Verify must never return a non-nil error")
			assert.Equal(t, tc.wantStatus, res.Status, "status")
			assert.Equal(t, tc.wantTier, res.Tier, "tier")
			assert.Contains(t, res.Message, tc.msgContains, "message should contain expected substring")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TofuVerifier
// ─────────────────────────────────────────────────────────────────────────────

func TestTofuVerifier_Interface(t *testing.T) {
	v := NewTofuVerifier(newFakeTofuStore())
	assert.Equal(t, "tofu", v.Name())
	assert.Equal(t, artifact.TierTofu, v.Tier())
}

func TestTofuVerifier_Verify(t *testing.T) {
	const digest1 = "sha256:aabbccddee112233"
	const digest2 = "sha256:ffeeddccbb998877"

	tests := []struct {
		name        string
		setupStore  func(s *fakeTofuStore)
		ref         artifact.ArtifactRef
		art         *artifact.Artifact
		wantStatus  artifact.Status
		wantTier    artifact.Tier
		wantErr     bool
		msgContains string
	}{
		{
			name:        "mutable ref is skipped",
			ref:         makeRef("oci", "nginx", "latest", "", true),
			art:         makeArt(digest1),
			wantStatus:  artifact.StatusSkip,
			wantTier:    artifact.TierTofu,
			msgContains: "skipped",
		},
		{
			name:        "first sight of immutable version: pin and warn",
			ref:         makeRef("oci", "nginx", "1.25.0", digest1, false),
			art:         makeArt(digest1),
			wantStatus:  artifact.StatusWarn,
			wantTier:    artifact.TierTofu,
			msgContains: "first-lock",
		},
		{
			name: "second sight with same digest: pass",
			setupStore: func(s *fakeTofuStore) {
				s.pins["oci:nginx@1.25.0"] = digest1
			},
			ref:         makeRef("oci", "nginx", "1.25.0", digest1, false),
			art:         makeArt(digest1),
			wantStatus:  artifact.StatusPass,
			wantTier:    artifact.TierTofu,
			msgContains: "confirmed",
		},
		{
			name: "digest changed for pinned version: fail with tamper alert",
			setupStore: func(s *fakeTofuStore) {
				s.pins["oci:nginx@1.25.0"] = digest1
			},
			ref:         makeRef("oci", "nginx", "1.25.0", digest2, false),
			art:         makeArt(digest2),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierTofu,
			msgContains: "DIGEST CHANGED",
		},
		{
			name: "tamper alert message contains both old and new digests",
			setupStore: func(s *fakeTofuStore) {
				s.pins["pypi:requests@2.31.0"] = digest1
			},
			ref:         makeRef("pypi", "requests", "2.31.0", digest2, false),
			art:         makeArt(digest2),
			wantStatus:  artifact.StatusFail,
			wantTier:    artifact.TierTofu,
			msgContains: digest1,
		},
		{
			name: "store GetPin error: fail",
			setupStore: func(s *fakeTofuStore) {
				s.getErr = errors.New("db unavailable")
			},
			ref:        makeRef("npm", "lodash", "4.17.21", digest1, false),
			art:        makeArt(digest1),
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierTofu,
			wantErr:    true,
		},
		{
			name: "store SetPin error on first pin: fail",
			setupStore: func(s *fakeTofuStore) {
				s.setErr = errors.New("db write failed")
			},
			ref:        makeRef("helm", "nginx", "15.0.0", digest1, false),
			art:        makeArt(digest1),
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierTofu,
			wantErr:    true,
		},
		{
			name:        "different protocols/versions get independent pins",
			ref:         makeRef("pypi", "requests", "2.31.0", digest2, false),
			art:         makeArt(digest2),
			wantStatus:  artifact.StatusWarn,
			wantTier:    artifact.TierTofu,
			msgContains: "first-lock",
		},
	}

	ctx := context.Background()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeTofuStore()
			if tc.setupStore != nil {
				tc.setupStore(store)
			}
			v := NewTofuVerifier(store)

			res, err := v.Verify(ctx, tc.ref, tc.art)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantStatus, res.Status, "status")
			assert.Equal(t, tc.wantTier, res.Tier, "tier")
			if tc.msgContains != "" {
				assert.Contains(t, res.Message, tc.msgContains, "message")
			}
		})
	}
}

// TestTofuVerifier_PinPersists verifies that a first-lock warning is followed
// by a pass on a second call with the same store state.
func TestTofuVerifier_PinPersists(t *testing.T) {
	const digest = "sha256:deadbeef"
	ref := makeRef("oci", "alpine", "3.18", digest, false)
	art := makeArt(digest)
	store := newFakeTofuStore()
	v := NewTofuVerifier(store)
	ctx := context.Background()

	// First call: first-lock.
	res1, err := v.Verify(ctx, ref, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusWarn, res1.Status)
	assert.Contains(t, res1.Message, "first-lock")

	// Second call with same store: pass.
	res2, err := v.Verify(ctx, ref, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res2.Status)
	assert.Contains(t, res2.Message, "confirmed")
}

// ─────────────────────────────────────────────────────────────────────────────
// Chain
// ─────────────────────────────────────────────────────────────────────────────

func TestChain_Verifiers(t *testing.T) {
	v1 := passVerifier("cs", artifact.TierChecksum)
	v2 := passVerifier("tofu", artifact.TierTofu)
	chain := NewChain(v1, v2)
	assert.Equal(t, []Verifier{v1, v2}, chain.Verifiers())
}

func TestChain_Verify(t *testing.T) {
	ctx := context.Background()
	ref := makeRef("oci", "nginx", "1.25.0", "sha256:abc", false)
	art := makeArt("sha256:abc")

	tests := []struct {
		name       string
		verifiers  []Verifier
		wantStatus artifact.Status
		wantTier   artifact.Tier
		wantErr    bool
	}{
		{
			name:       "no verifiers: pass at lowest tier",
			verifiers:  nil,
			wantStatus: artifact.StatusPass,
			wantTier:   artifact.TierChecksum,
		},
		{
			name:       "single verifier pass",
			verifiers:  []Verifier{passVerifier("cs", artifact.TierChecksum)},
			wantStatus: artifact.StatusPass,
			wantTier:   artifact.TierChecksum,
		},
		{
			name:       "two pass verifiers: highest tier wins",
			verifiers:  []Verifier{passVerifier("cs", artifact.TierChecksum), passVerifier("tofu", artifact.TierTofu)},
			wantStatus: artifact.StatusPass,
			wantTier:   artifact.TierTofu,
		},
		{
			name: "three tier pass: signed is highest",
			verifiers: []Verifier{
				passVerifier("cs", artifact.TierChecksum),
				passVerifier("tofu", artifact.TierTofu),
				passVerifier("signed", artifact.TierSigned),
			},
			wantStatus: artifact.StatusPass,
			wantTier:   artifact.TierSigned,
		},
		{
			name:       "first verifier fails: short-circuit",
			verifiers:  []Verifier{failVerifier("cs", artifact.TierChecksum), passVerifier("tofu", artifact.TierTofu)},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierChecksum,
		},
		{
			name:       "second verifier fails: short-circuit after first",
			verifiers:  []Verifier{passVerifier("cs", artifact.TierChecksum), failVerifier("tofu", artifact.TierTofu)},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierTofu,
		},
		{
			name: "third verifier fails: only its tier reported",
			verifiers: []Verifier{
				passVerifier("cs", artifact.TierChecksum),
				passVerifier("tofu", artifact.TierTofu),
				failVerifier("signed", artifact.TierSigned),
			},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierSigned,
		},
		{
			name:       "warn propagates: final status is warn",
			verifiers:  []Verifier{warnVerifier("tofu", artifact.TierTofu), passVerifier("signed", artifact.TierSigned)},
			wantStatus: artifact.StatusWarn,
			wantTier:   artifact.TierSigned,
		},
		{
			name:       "warn then fail: final status is fail (short-circuit)",
			verifiers:  []Verifier{warnVerifier("tofu", artifact.TierTofu), failVerifier("signed", artifact.TierSigned)},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierSigned,
		},
		{
			name:       "verifier returns error: fail with wrapped error",
			verifiers:  []Verifier{passVerifier("cs", artifact.TierChecksum), errVerifier("tofu", artifact.TierTofu)},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierTofu,
			wantErr:    true,
		},
		{
			name:       "first verifier error: immediate fail",
			verifiers:  []Verifier{errVerifier("cs", artifact.TierChecksum)},
			wantStatus: artifact.StatusFail,
			wantTier:   artifact.TierChecksum,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := NewChain(tc.verifiers...)
			res, err := chain.Verify(ctx, ref, art)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantStatus, res.Status, "status")
			assert.Equal(t, tc.wantTier, res.Tier, "tier")
			assert.NotEmpty(t, res.Message, "result message must not be empty")
		})
	}
}

// TestChain_Verify_WithRealVerifiers exercises the Chain with the actual
// ChecksumVerifier and TofuVerifier wired together.
func TestChain_Verify_WithRealVerifiers(t *testing.T) {
	const digest = "sha256:cafebabedeadbeef"

	store := newFakeTofuStore()
	chain := NewChain(NewChecksumVerifier(), NewTofuVerifier(store))
	ctx := context.Background()

	ref := makeRef("oci", "debian", "12", digest, false)
	art := makeArt(digest)

	// First pass: checksum passes, tofu first-locks → overall Warn at TierTofu.
	res1, err := chain.Verify(ctx, ref, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusWarn, res1.Status)
	assert.Equal(t, artifact.TierTofu, res1.Tier)

	// Second pass: checksum passes, tofu confirms → overall Pass at TierTofu.
	res2, err := chain.Verify(ctx, ref, art)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusPass, res2.Status)
	assert.Equal(t, artifact.TierTofu, res2.Tier)

	// Third pass with wrong digest: checksum fails immediately.
	artBad := makeArt("sha256:000000000000")
	res3, err := chain.Verify(ctx, ref, artBad)
	require.NoError(t, err)
	assert.Equal(t, artifact.StatusFail, res3.Status)
	assert.Equal(t, artifact.TierChecksum, res3.Tier)
	assert.Contains(t, res3.Message, "mismatch")
}

// TestTofuKey ensures the TOFU key format is stable and unique across protocols.
func TestTofuKey(t *testing.T) {
	tests := []struct {
		ref  artifact.ArtifactRef
		want string
	}{
		{makeRef("oci", "nginx", "1.25.0", "", false), "oci:nginx@1.25.0"},
		{makeRef("pypi", "requests", "2.31.0", "", false), "pypi:requests@2.31.0"},
		{makeRef("gomod", "github.com/pkg/errors", "v0.9.1", "", false), "gomod:github.com/pkg/errors@v0.9.1"},
		{makeRef("npm", "@types/node", "20.0.0", "", false), "npm:@types/node@20.0.0"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, tofuKey(tc.ref))
	}
}
