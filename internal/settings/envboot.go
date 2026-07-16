package settings

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
)

// envboot.go: the SINGLE entry point for env bootstrap, plus explicit warnings
// for environment variables that have been retired.
//
// The reference (ai-sandbox) arrived at this file the hard way, and the lesson is
// worth restating because it is what this file exists to prevent:
//
//	Setting.EnvVar was pure metadata — NO production code ever read an env var
//	through it. The real bootstrap was a hand-copied map in main.go. Thirteen
//	settings declared an EnvVar that nobody had copied across, so setting those
//	env vars did NOTHING: the value silently fell back to a built-in default
//	while the registry and the docs kept telling operators "just set this env
//	var". The registry was lying. A unit test even asserted
//	`s.EnvVar == "SANDBOX_SCHED_CPU_OVERSUB"` and passed — it checked only that a
//	string equalled itself, never that the variable had any effect, which made
//	the dead variables look tested.
//
// The rule now: EnvBootstrap reads env vars BY WALKING THE REGISTRY. Declaring
// an EnvVar is what makes it work, so the two cannot drift apart. envboot_test.go
// asserts exactly that, behaviourally.
//
// Specula-specific note: an EnvVar declared here is read from the process
// environment DIRECTLY, not through internal/config's koanf loader. For
// auth.jwt_secret the two agree by construction (the declared
// SPECULA_AUTH__JWT_SECRET is precisely the koanf env mapping of
// auth.jwt_secret), and cmd/specula additionally overlays the parsed config
// value onto this snapshot so a jwt_secret set in the YAML file bootstraps too.
func EnvBootstrap(reg *Registry) map[string]string {
	out := make(map[string]string)
	if reg == nil {
		return out
	}
	for _, s := range reg.All() {
		if s.EnvVar == "" {
			continue
		}
		if v, ok := os.LookupEnv(s.EnvVar); ok && v != "" {
			out[s.Key] = v
		}
	}
	return out
}

// RemovedEnvVars maps environment variables that USED to exist onto the setting
// key that replaced them.
//
// Specula has retired none yet — settings arrived here before the env sprawl did,
// which is the whole point of porting this layer early. The map, WarnRemovedEnv
// and their tests are ported anyway: they are the mechanism that makes the next
// retirement honest. When a knob moves from env to settings-only, it gets an
// entry here and the operator sees an exact migration instruction in the startup
// log — instead of their configuration silently doing nothing and the truth
// surfacing months later during an incident.
var RemovedEnvVars = map[string]string{}

// WarnRemovedEnv checks at startup whether any retired env var is still set,
// warning per hit with a migration pointer. It warns rather than failing: these
// are policy values, and refusing to start is worse than a configuration that
// has moved.
func WarnRemovedEnv(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	var hits []string
	for env := range RemovedEnvVars {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			hits = append(hits, env)
		}
	}
	if len(hits) == 0 {
		return
	}
	sort.Strings(hits)
	var b strings.Builder
	b.WriteString("specula: " + strconv.Itoa(len(hits)) + " RETIRED environment variable(s) are still set — " +
		"they NO LONGER have any effect. Configure them under System Settings instead " +
		"(runtime-changeable, no restart needed):")
	for _, env := range hits {
		b.WriteString("\n  - " + env + " → setting " + RemovedEnvVars[env])
	}
	b.WriteString("\n  (These are managed via the admin UI / PUT /api/v1/admin/settings/{key}; " +
		"hot-reload settings take effect immediately.)")
	log.Warn(b.String())
}
