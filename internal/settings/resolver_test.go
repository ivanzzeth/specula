package settings

// Ported (translated + re-pointed at Specula's registry keys) from ai-sandbox
// internal/controlplane/settings/resolver_test.go. Their assertions judge our
// implementation: a failure here is our bug, not theirs.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ivanzzeth/specula/internal/configstore"
)

// enabledStore returns an enabled in-memory config store (random master key).
func enabledStore(t *testing.T) configstore.Store {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c, err := configstore.NewCrypter(base64.StdEncoding.EncodeToString(key))
	if err != nil || !c.Enabled() {
		t.Fatalf("crypter: %v enabled=%v", err, c.Enabled())
	}
	return configstore.NewMemStore(c)
}

// disabledStore returns a disabled config store (no master key).
func disabledStore(t *testing.T) configstore.Store {
	t.Helper()
	c, _ := configstore.NewCrypter("")
	return configstore.NewMemStore(c)
}

// testResolver builds a Resolver over the real DefaultRegistry with a bootstrap
// snapshot. The reference used preview.* keys; Specula's equivalents are
// org.max_per_user (a plain hot-reload value) and auth.jwt_secret (a secret with
// no bootstrap value), which exercise the same three code paths.
func testResolver(t *testing.T, store configstore.Store) *Resolver {
	t.Helper()
	return NewResolver(DefaultRegistry(), store, map[string]string{
		KeyOrgMaxPerUser:    "3",
		KeyAuthJWTSecret:    "", // bootstrap default empty
		KeyRegistryTokenKey: "",
	})
}

func TestEffectivePrecedenceConfigOverEnv(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))

	// No override: the bootstrap default applies, source=env.
	got, err := r.Effective(ctx, KeyOrgMaxPerUser)
	if err != nil || got != "3" {
		t.Fatalf("env default = %q, %v; want 3", got, err)
	}
	if src, _ := r.Source(ctx, KeyOrgMaxPerUser); src != SourceEnv {
		t.Fatalf("source = %q; want env", src)
	}

	// Write an override: store > bootstrap.
	if err := r.Set(ctx, KeyOrgMaxPerUser, "7"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ = r.Effective(ctx, KeyOrgMaxPerUser)
	if got != "7" {
		t.Fatalf("override = %q; want 7", got)
	}
	if src, _ := r.Source(ctx, KeyOrgMaxPerUser); src != SourceRuntime {
		t.Fatalf("source = %q; want runtime", src)
	}
}

func TestClearFallsBackToEnv(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	if err := r.Set(ctx, KeyOrgMaxPerUser, "7"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := r.Clear(ctx, KeyOrgMaxPerUser); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, _ := r.Effective(ctx, KeyOrgMaxPerUser)
	if got != "3" {
		t.Fatalf("after clear = %q; want 3", got)
	}
	if src, _ := r.Source(ctx, KeyOrgMaxPerUser); src != SourceEnv {
		t.Fatalf("source = %q; want env", src)
	}
}

func TestSourceUnsetWhenNoEnvNoOverride(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	if src, _ := r.Source(ctx, KeyAuthJWTSecret); src != SourceUnset {
		t.Fatalf("source = %q; want unset", src)
	}
}

func TestSetClearDisabledStoreReturnsErr(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, disabledStore(t))
	if r.ConfigEnabled() {
		t.Fatal("ConfigEnabled = true; want false on disabled store")
	}
	if err := r.Set(ctx, KeyOrgMaxPerUser, "5"); !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Set on disabled = %v; want ErrConfigDisabled", err)
	}
	if err := r.Clear(ctx, KeyOrgMaxPerUser); !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Clear on disabled = %v; want ErrConfigDisabled", err)
	}
	// A disabled store still permits Effective/Source (bootstrap only).
	if got, _ := r.Effective(ctx, KeyOrgMaxPerUser); got != "3" {
		t.Fatalf("Effective on disabled = %q; want 3", got)
	}
}

func TestUnknownKey(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	if _, err := r.Effective(ctx, "no.such.key"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Effective unknown = %v; want ErrUnknownKey", err)
	}
	if err := r.Set(ctx, "no.such.key", "x"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Set unknown = %v; want ErrUnknownKey", err)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		kind  Kind
		value string
		ok    bool
	}{
		{KindDuration, "168h", true},
		{KindDuration, "30m", true},
		{KindDuration, "notaduration", false},
		{KindBool, "true", true},
		{KindBool, "1", true},
		{KindBool, "maybe", false},
		{KindString, "anything", true},
		{KindSecret, "s3cr3t", true},
		{KindList, "a,b,c", true},
		{KindInt, "42", true},
		{KindInt, "4.2", false},
		{KindFloat, "0.25", true},
		{KindFloat, "abc", false},
		{KindDuration, "", true}, // empty always allowed (clear semantics)
	}
	for _, c := range cases {
		err := Validate(c.kind, c.value)
		if c.ok && err != nil {
			t.Errorf("Validate(%s,%q) = %v; want nil", c.kind, c.value, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("Validate(%s,%q) = nil; want error", c.kind, c.value)
			} else if !errors.Is(err, ErrValidation) {
				t.Errorf("Validate(%s,%q) = %v; want wraps ErrValidation", c.kind, c.value, err)
			}
		}
	}
}

func TestValidateSettingEnum(t *testing.T) {
	s := Setting{Kind: KindEnum, Enum: []string{"off", "on"}}
	for _, v := range []string{"off", "on", ""} { // empty = clear, always allowed
		if err := ValidateSetting(s, v); err != nil {
			t.Errorf("ValidateSetting(enum,%q) = %v; want nil", v, err)
		}
	}
	for _, v := range []string{"aws", "ON", "maybe"} {
		if err := ValidateSetting(s, v); !errors.Is(err, ErrValidation) {
			t.Errorf("ValidateSetting(enum,%q) = %v; want ErrValidation", v, err)
		}
	}
}

// TestOrgMaxPerUserRegistered mirrors the reference's per-setting registration
// guards (TestSchedulerOverflowRegistered / TestIdleSettingsRegistered).
func TestOrgMaxPerUserRegistered(t *testing.T) {
	s, ok := DefaultRegistry().Lookup(KeyOrgMaxPerUser)
	if !ok {
		t.Fatal("org.max_per_user not registered")
	}
	if s.Kind != KindInt || !s.HotReload {
		t.Fatalf("org.max_per_user = %+v; want int + hotreload", s)
	}
	if err := ValidateSetting(s, "5"); err != nil {
		t.Fatalf("validate 5 = %v; want nil", err)
	}
	if err := ValidateSetting(s, "many"); !errors.Is(err, ErrValidation) {
		t.Fatalf("validate many = %v; want ErrValidation", err)
	}
}

// TestSecretSettingsRegistered guards the two secrets this port exists for: both
// must be redacted, and neither may be hot-reloadable (each backs a verifier
// built once at startup).
func TestSecretSettingsRegistered(t *testing.T) {
	for _, key := range []string{KeyAuthJWTSecret, KeyRegistryTokenKey} {
		s, ok := DefaultRegistry().Lookup(key)
		if !ok {
			t.Fatalf("%s not registered", key)
		}
		if s.Kind != KindSecret {
			t.Errorf("%s kind = %q; want secret", key, s.Kind)
		}
		if !s.Redact {
			t.Errorf("%s Redact = false; a secret must never be echoed", key)
		}
		if s.HotReload {
			t.Errorf("%s HotReload = true; its verifier is built at startup, so this would lie to the operator", key)
		}
	}
}

func TestSetRejectsInvalidWithoutWriting(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	if err := r.Set(ctx, KeyOrgMaxPerUser, "bogus"); !errors.Is(err, ErrValidation) {
		t.Fatalf("Set invalid int = %v; want ErrValidation", err)
	}
	// Nothing should have been written: source is still env.
	if src, _ := r.Source(ctx, KeyOrgMaxPerUser); src == SourceRuntime {
		t.Fatal("invalid Set wrote an override; want none")
	}
}

func TestRedacted(t *testing.T) {
	if Redacted("") != "" {
		t.Fatal("empty should redact to empty")
	}
	// Never contains the full plaintext.
	full := "supersecretvalue1234"
	got := Redacted(full)
	if got == full {
		t.Fatal("redacted equals plaintext")
	}
	// Only the last 4 are exposed.
	if want := "1234"; got[len(got)-5:len(got)-1] != want {
		t.Fatalf("redacted %q does not end with last4 %q", got, want)
	}
	// Short value (≤4): must not leak more than itself.
	short := Redacted("ab")
	if short == "" || len(short) == 0 {
		t.Fatal("short redact empty")
	}
}

func TestReloadHookFiredOnSetAndClear(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	var fired []string
	r.OnReload(KeyOrgMaxPerUser, func(key, eff string) error {
		fired = append(fired, key+"="+eff)
		return nil
	})
	if err := r.Set(ctx, KeyOrgMaxPerUser, "7"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := r.Clear(ctx, KeyOrgMaxPerUser); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(fired) != 2 {
		t.Fatalf("hook fired %d times; want 2 (set+clear)", len(fired))
	}
	if fired[0] != "org.max_per_user=7" {
		t.Fatalf("set hook eff = %q; want 7", fired[0])
	}
	if fired[1] != "org.max_per_user=3" {
		t.Fatalf("clear hook eff = %q; want 3 (fallback)", fired[1])
	}
}

func TestReloadHookErrorPropagates(t *testing.T) {
	ctx := context.Background()
	r := testResolver(t, enabledStore(t))
	sentinel := errors.New("rebuild failed")
	r.OnReload(KeyAuthJWTSecret, func(_, _ string) error { return sentinel })
	if err := r.Set(ctx, KeyAuthJWTSecret, "somekey"); !errors.Is(err, sentinel) {
		t.Fatalf("Set hook error = %v; want sentinel", err)
	}
	// The value is still persisted (consistent persistence semantics); the hook
	// itself is responsible for not degrading.
	if src, _ := r.Source(ctx, KeyAuthJWTSecret); src != SourceRuntime {
		t.Fatalf("source = %q; want runtime (value persisted despite hook err)", src)
	}
}
