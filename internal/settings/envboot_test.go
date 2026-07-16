package settings

// Ported (translated + adapted) from ai-sandbox
// internal/controlplane/settings/envboot_test.go. These guard "the registry must
// not lie to operators about environment variables", and they are the reason the
// reference's thirteen dead env vars cannot recur here.

import (
	"testing"
)

// TestEnvBootstrap_EveryDeclaredEnvVarActuallyWorks: every setting that declares
// an EnvVar must ACTUALLY take effect when that variable is set. EnvBootstrap
// reads the registry, so declaring is what makes it work and the two cannot
// drift.
//
// This is a behavioural test on purpose. The reference's original test asserted
// `s.EnvVar == "SANDBOX_SCHED_CPU_OVERSUB"` — a string equalling itself — and
// stayed green while the variable did nothing at all.
func TestEnvBootstrap_EveryDeclaredEnvVarActuallyWorks(t *testing.T) {
	reg := DefaultRegistry()
	var checked int
	for _, s := range reg.All() {
		if s.EnvVar == "" {
			continue
		}
		checked++
		t.Run(s.EnvVar, func(t *testing.T) {
			t.Setenv(s.EnvVar, "probe-value-"+s.Key)
			boot := EnvBootstrap(reg)
			got, ok := boot[s.Key]
			if !ok {
				t.Fatalf("setting %q declares EnvVar=%q, but setting that env var had NO effect on EnvBootstrap.\n"+
					"  The registry is lying to operators: the docs tell them to set this variable, and it does nothing —\n"+
					"  the value silently falls back to a built-in default.\n"+
					"  Either make it really work, or drop the EnvVar declaration and record it in RemovedEnvVars.", s.Key, s.EnvVar)
			}
			if want := "probe-value-" + s.Key; got != want {
				t.Fatalf("EnvBootstrap[%q] = %q, want %q", s.Key, got, want)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no setting in the registry declares an EnvVar — this guard has almost certainly broken " +
			"(it would now be green forever) and must be fixed")
	}
	t.Logf("verified %d bootstrap env var(s) (all genuinely effective)", checked)
}

// TestEnvBootstrap_OnlyBootstrapKeepsEnv: only TRUE bootstrap items may keep an
// EnvVar — things needed before the encrypted store is usable. Everything else is
// settings-only (changed in the admin UI, hot-reloaded, no restart).
//
// This is a policy gate: adding an EnvVar requires arguing here that it really is
// bootstrap, rather than casually adding one more env var — which is exactly the
// path that accumulated 40+ of them in the reference.
func TestEnvBootstrap_OnlyBootstrapKeepsEnv(t *testing.T) {
	allowed := map[string]string{
		"SPECULA_AUTH__JWT_SECRET": "bootstrap: signs/verifies session cookies; needed to build the " +
			"verifier at startup, independently of the encrypted store. Also the koanf env mapping of " +
			"auth.jwt_secret, so it is a variable operators already set.",
	}
	for _, s := range DefaultRegistry().All() {
		if s.EnvVar == "" {
			continue
		}
		if _, ok := allowed[s.EnvVar]; !ok {
			t.Errorf(`setting %q declares EnvVar=%q, which is not on the BOOTSTRAP WHITELIST.
  Rule: only items needed BEFORE the settings store is usable get an env var;
  everything else is a runtime policy knob → settings-only (admin UI, hot-reloaded, no restart).
  If it is genuinely bootstrap, add it to this test's allowed map with the reason; otherwise drop the EnvVar.`,
				s.Key, s.EnvVar)
		}
	}
}

// TestRemovedEnvVars_PointAtRealSettings: every key in the retired-env table must
// name a setting that actually exists, or the startup warning points operators at
// a setting that is not there — another lying error message.
func TestRemovedEnvVars_PointAtRealSettings(t *testing.T) {
	reg := DefaultRegistry()
	for env, key := range RemovedEnvVars {
		if _, ok := reg.Lookup(key); !ok {
			t.Errorf("retired env %q points at setting %q, which is not in the registry — "+
				"the startup warning would send operators to a setting that does not exist", env, key)
		}
	}
	// A retired env var must not also be declared live in the registry
	// (self-contradiction: retired on one hand, effective on the other).
	for _, s := range reg.All() {
		if s.EnvVar == "" {
			continue
		}
		if _, removed := RemovedEnvVars[s.EnvVar]; removed {
			t.Errorf("env %q is recorded as RETIRED and yet still declared live on setting %q — self-contradictory",
				s.EnvVar, s.Key)
		}
	}
}

// TestWarnRemovedEnv_NoPanic: the warning path itself must never panic — it runs
// early in startup, and a panic there means no logs at all.
func TestWarnRemovedEnv_NoPanic(t *testing.T) {
	WarnRemovedEnv(nil) // nil logger must fall back to slog.Default(), not panic
	// And with a hit present, if any env var has been retired by now.
	for env := range RemovedEnvVars {
		t.Setenv(env, "some-value")
		break
	}
	WarnRemovedEnv(nil)
}

// TestRegistryOrderedAndUniqueByKey covers the Registry contract the reference
// documents but never asserted: stable registration order, and a duplicate key
// overwriting in place rather than appearing twice.
func TestRegistryOrderedAndUniqueByKey(t *testing.T) {
	r := NewRegistry(
		Setting{Key: "a.one", Kind: KindString},
		Setting{Key: "b.two", Kind: KindInt},
		Setting{Key: "a.one", Kind: KindBool, Desc: "redeclared"},
	)
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() = %d settings; want 2 (duplicate key must not add a row)", len(all))
	}
	if all[0].Key != "a.one" || all[1].Key != "b.two" {
		t.Fatalf("All() order = %q,%q; want registration order a.one,b.two", all[0].Key, all[1].Key)
	}
	if all[0].Kind != KindBool || all[0].Desc != "redeclared" {
		t.Fatalf("duplicate key must overwrite in place; got %+v", all[0])
	}
	// Lookup trims whitespace.
	if _, ok := r.Lookup("  a.one "); !ok {
		t.Fatal("Lookup must trim surrounding whitespace")
	}
}

// TestDefaultRegistryKeysUnique guards against a copy-paste duplicate silently
// swallowing a setting (NewRegistry overwrites, so a dupe would vanish quietly).
func TestDefaultRegistryKeysUnique(t *testing.T) {
	reg := DefaultRegistry()
	all := reg.All()
	seen := map[string]bool{}
	for _, s := range all {
		if seen[s.Key] {
			t.Errorf("duplicate key %q in DefaultRegistry", s.Key)
		}
		seen[s.Key] = true
		if s.Desc == "" {
			t.Errorf("setting %q has no Desc — the admin UI would show an unexplained knob", s.Key)
		}
		if s.Kind == "" {
			t.Errorf("setting %q has no Kind — it would skip validation entirely", s.Key)
		}
	}
	if len(all) == 0 {
		t.Fatal("DefaultRegistry is empty")
	}
}
