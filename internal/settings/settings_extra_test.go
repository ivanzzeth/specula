package settings

// settings_extra_test.go lives in package settings (internal) so it can access
// unexported symbols: Resolver.fire, Resolver.reg, and the package-level
// RemovedEnvVars map.
//
// Gaps covered:
//   - SplitCSV: 0% → full coverage
//   - Resolver.Registry(): 0% → covered
//   - ConfigEnabled with nil store: nil-store branch uncovered
//   - Source with unknown key: ErrUnknownKey return in Source uncovered
//   - Set with nil store: nil-store early-return uncovered
//   - Clear with unknown key: ErrUnknownKey return in Clear uncovered
//   - Clear with nil store: nil-store early-return uncovered
//   - fire when Effective returns error: error-propagation path uncovered
//   - WarnRemovedEnv hits branch: sort+warn code path unreachable when
//     RemovedEnvVars is empty; temporarily populate it

import (
	"context"
	"errors"
	"testing"
)

// ── SplitCSV ─────────────────────────────────────────────────────────────────

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		name string
		v    string
		def  string
		want []string
	}{
		{"value_used", "a,b,c", "fallback", []string{"a", "b", "c"}},
		{"value_trimmed", "  a , b , c  ", "", []string{"a", "b", "c"}},
		{"empty_value_uses_default", "", "x,y", []string{"x", "y"}},
		{"both_empty_returns_nil", "", "", nil},
		{"empty_segment_dropped", "a,,b", "", []string{"a", "b"}},
		{"whitespace_only_segments_dropped", "  ,  ", "", nil},
		{"non_empty_value_overrides_non_empty_default", "z", "fallback", []string{"z"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := SplitCSV(c.v, c.def)
			if len(got) != len(c.want) {
				t.Fatalf("SplitCSV(%q, %q) = %v; want %v", c.v, c.def, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("SplitCSV(%q, %q)[%d] = %q; want %q",
						c.v, c.def, i, got[i], c.want[i])
				}
			}
		})
	}
}

// ── Registry accessor ─────────────────────────────────────────────────────────

func TestResolverRegistry(t *testing.T) {
	r := testResolver(t, enabledStore(t))
	if r.Registry() != r.reg {
		t.Fatal("Registry() must return the resolver's underlying registry pointer")
	}
}

// ── ConfigEnabled with nil store ──────────────────────────────────────────────

// TestConfigEnabled_NilStore covers the r.store == nil early-return in
// ConfigEnabled.  A resolver built without a store must report disabled.
func TestConfigEnabled_NilStore(t *testing.T) {
	r := NewResolver(DefaultRegistry(), nil, map[string]string{})
	if r.ConfigEnabled() {
		t.Fatal("ConfigEnabled with nil store: want false, got true")
	}
}

// ── Source with unknown key ───────────────────────────────────────────────────

// TestSource_UnknownKey covers the ErrUnknownKey return in Source.
// The existing TestUnknownKey only probes Effective and Set.
func TestSource_UnknownKey(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	_, err := r.Source(ctx, "no.such.key")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Source unknown key: got %v; want ErrUnknownKey", err)
	}
}

// ── Set with nil store ────────────────────────────────────────────────────────

// TestSet_NilStore covers the r.store == nil early-return in Set.
// The existing TestSetClearDisabledStoreReturnsErr uses a *disabled* MemStore,
// not nil — a different code path.
func TestSet_NilStore(t *testing.T) {
	ctx := context.Background()
	r := NewResolver(DefaultRegistry(), nil, map[string]string{})
	err := r.Set(ctx, KeyOrgMaxPerUser, "5")
	if !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Set nil store: got %v; want ErrConfigDisabled", err)
	}
}

// ── Clear: unknown key ────────────────────────────────────────────────────────

// TestClear_UnknownKey covers the ErrUnknownKey return in Clear.
func TestClear_UnknownKey(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	err := r.Clear(ctx, "no.such.key")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Clear unknown key: got %v; want ErrUnknownKey", err)
	}
}

// ── Clear with nil store ──────────────────────────────────────────────────────

// TestClear_NilStore covers the r.store == nil early-return in Clear.
func TestClear_NilStore(t *testing.T) {
	ctx := context.Background()
	r := NewResolver(DefaultRegistry(), nil, map[string]string{})
	err := r.Clear(ctx, KeyOrgMaxPerUser)
	if !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Clear nil store: got %v; want ErrConfigDisabled", err)
	}
}

// ── fire: Effective returns error ─────────────────────────────────────────────

// TestFire_EffectiveError covers the `eff, err := r.Effective(ctx, key); if err != nil`
// path in fire.  This can only trigger when a hook is registered for a key that
// is not in the registry.  OnReload stores hooks without registry validation, so
// we can create this mismatch directly and then invoke fire() from the same
// internal test package.
func TestFire_EffectiveError(t *testing.T) {
	ctx := context.Background()
	r := NewResolver(DefaultRegistry(), enabledStore(t), map[string]string{})

	// Register a hook for a key that is NOT in the registry.
	r.OnReload("phantom.key", func(_, _ string) error { return nil })

	// fire() looks up hooks["phantom.key"] → non-nil → calls Effective, which
	// returns ErrUnknownKey because the key is not registered.
	err := r.fire(ctx, "phantom.key")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("fire with unregistered key: got %v; want ErrUnknownKey", err)
	}
}

// ── WarnRemovedEnv hits branch ────────────────────────────────────────────────

// TestWarnRemovedEnv_WithHits covers the sort + string-builder + log.Warn code
// path in WarnRemovedEnv.  RemovedEnvVars is currently empty (Specula has retired
// no env vars yet), so the hits branch is ordinarily dead.  We temporarily add an
// entry, set the corresponding env var, then restore the map in t.Cleanup.
func TestWarnRemovedEnv_WithHits(t *testing.T) {
	const retiredEnv = "SPECULA_RETIRED_TEST_VAR_COVERAGE"

	// The value must map to a key that actually exists in the registry, or
	// TestRemovedEnvVars_PointAtRealSettings would catch the inconsistency.
	RemovedEnvVars[retiredEnv] = KeyOrgMaxPerUser
	t.Cleanup(func() { delete(RemovedEnvVars, retiredEnv) })

	t.Setenv(retiredEnv, "old-ignored-value")

	// Must not panic; the hit path runs sort + string build + slog.Warn.
	WarnRemovedEnv(nil)
}
