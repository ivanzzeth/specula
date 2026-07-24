// Package settings is Specula's general-purpose RUNTIME SETTINGS layer: it
// promotes selected configuration items from "env/YAML-bootstrapped, restart to
// change" to "runtime-overridable, encrypted, persisted". The config file/env is
// the BOOTSTRAP DEFAULT; the encrypted store (internal/configstore) holds the
// runtime override. Resolution order: override > bootstrap.
//
// Ported from ai-sandbox internal/controlplane/settings.
//
// Design:
//   - A Setting descriptor declares one known setting (Key / EnvVar / Kind /
//     Redact / HotReload / Dangerous / Desc / Enum). A Registry is an ordered,
//     key-unique collection of them; DefaultRegistry lists every known key.
//   - Resolver sits on top of (bootstrap snapshot + encrypted store) and offers
//     Effective / Source / Set / Clear, firing that key's reload hook after a
//     Set/Clear so HotReload settings take effect without a restart.
//   - Secret settings (Redact=true, Kind=secret) are NEVER echoed: responses
//     carry set/unset plus length and last-4 only. See Redacted, and the admin
//     handler in internal/admin/settings.go which consults Redact.
//
// This package only reads a bootstrap snapshot and the encrypted store. It never
// touches the artifact data plane.
package settings

import (
	"strings"
)

// Kind is a setting's value type: it drives PUT-time validation and decides
// whether the value is redacted.
type Kind string

const (
	KindString   Kind = "string"
	KindSecret   Kind = "secret"   // key material: responses are redacted (never plaintext)
	KindList     Kind = "list"     // comma-separated list
	KindDuration Kind = "duration" // time.ParseDuration form (e.g. "168h")
	KindBool     Kind = "bool"     // "0"/"1"/"true"/"false"
	KindEnum     Kind = "enum"     // value must be ∈ Setting.Enum (empty = clear, fall back to default)
	KindInt      Kind = "int"      // decimal integer (empty = clear)
	KindFloat    Kind = "float"    // decimal float (empty = clear)
)

// Setting describes one known runtime setting.
type Setting struct {
	Key       string   // setting key (e.g. "auth.jwt_secret"); the admin API and store agree on this
	EnvVar    string   // the env var this setting's bootstrap default comes from
	Kind      Kind     // value type (validation + redaction)
	Redact    bool     // true = responses are redacted (set/unset + length/last-4 only, never plaintext)
	HotReload bool     // true = Set/Clear fires a reload hook and takes effect now; false = restart required
	Dangerous bool     // true = high-risk item; the UI demands a second confirmation
	Desc      string   // human-readable description
	Enum      []string // allowed values when Kind=enum (empty value is always allowed = clear)
}

// Registry is an ordered, key-unique set of known settings.
type Registry struct {
	order []string // stable output order (registration order)
	byKey map[string]Setting
}

// NewRegistry builds a registry from the given settings, in the given order. A
// duplicate Key is overwritten by the later declaration (position preserved).
func NewRegistry(settings ...Setting) *Registry {
	r := &Registry{byKey: make(map[string]Setting, len(settings))}
	for _, s := range settings {
		if _, ok := r.byKey[s.Key]; !ok {
			r.order = append(r.order, s.Key)
		}
		r.byKey[s.Key] = s
	}
	return r
}

// Lookup returns the descriptor for key.
func (r *Registry) Lookup(key string) (Setting, bool) {
	s, ok := r.byKey[strings.TrimSpace(key)]
	return s, ok
}

// All returns every descriptor in registration order.
func (r *Registry) All() []Setting {
	out := make([]Setting, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.byKey[k])
	}
	return out
}

// Known setting keys, declared centrally so wiring and tests reference a
// constant rather than scattered string literals.
const (
	// KeyAuthJWTSecret is the HS256 session-cookie signing secret.
	//
	// This is the high-value one. Before this package existed, an unset secret
	// meant cmd/specula generated an EPHEMERAL one at every boot: sessions died
	// on restart and were never valid across HA replicas (each replica signed
	// with a different key, so a login on replica A was rejected by replica B).
	// Now it is generated ONCE and persisted into the encrypted store, so it
	// both survives restarts and is shared by every replica automatically.
	// Not HotReload: the verifier is built at startup, and rotating it would
	// invalidate every live session — that is a deliberate, restart-gated act.
	KeyAuthJWTSecret = "auth.jwt_secret"

	// KeyRegistryTokenKey is the RS256 signing key (PKCS#8 PEM) for hosted
	// registry Bearer tokens — the Docker v2 token flow. Same HA argument as
	// auth.jwt_secret and then some: registrytoken.EnsureKeyPair writes a PEM to
	// local disk, which is node-local, so a token minted by replica A does not
	// verify on replica B and `docker push` fails behind a load balancer.
	// Storing it here makes the keypair shared and durable. Not HotReload: the
	// token Service is constructed at startup.
	KeyRegistryTokenKey = "registry.token_key"

	// KeyOrgMaxPerUser caps how many orgs one user may self-create
	// (0 = unlimited; default 1). System-admin paths are not subject to it.
	// HotReload: the create-org handler reads the effective value per request.
	KeyOrgMaxPerUser = "org.max_per_user"

	// KeyCacheMaxBytes is the pull-through cache capacity ceiling in bytes
	// (0 = unlimited). HotReload: updates the CacheManager ceiling immediately;
	// eviction runs on the next Store that exceeds the limit.
	KeyCacheMaxBytes = "cache.max_bytes"
)

// DefaultOrgMaxPerUser is the bootstrap default for org.max_per_user: one
// self-created org per user.
const DefaultOrgMaxPerUser = 1

// DefaultRegistry returns every known Specula setting.
//
// Registry policy (enforced by envboot_test.go): a setting only keeps an EnvVar
// if it is a genuine BOOTSTRAP item — something needed before the encrypted
// store is usable. Everything else is settings-only: the operator changes it in
// the admin UI and HotReload makes it effective immediately, with no env var and
// no restart. That rule is what stops this from growing into another sprawl of
// forty environment variables nobody can audit.
func DefaultRegistry() *Registry {
	return NewRegistry(
		// Bootstrap: needed to build the session verifier at startup, i.e.
		// before/independently of the encrypted store. EnvVar matches Specula's
		// koanf env mapping (auth.jwt_secret → SPECULA_AUTH__JWT_SECRET), so
		// the registry tells operators the truth about the var they already set.
		Setting{Key: KeyAuthJWTSecret, EnvVar: "SPECULA_AUTH__JWT_SECRET",
			Kind: KindSecret, Redact: true, HotReload: false,
			Desc: "HS256 signing secret for login session cookies. Empty = generated once and " +
				"persisted into the encrypted store, so sessions survive restarts and every HA " +
				"replica shares the key. Changing it invalidates all sessions; takes effect after a restart."},
		// Bootstrap: the registry token Service is constructed at startup.
		//
		// Deliberately NO EnvVar. Its bootstrap default is the existing on-disk
		// PEM (auth.registry_token_key_path), which cmd/specula seeds into the
		// bootstrap snapshot and migrates into the encrypted store on first
		// start — so an existing single-node deployment keeps its exact keypair
		// and simply gains HA-shareability. An RSA private key does not belong
		// in an environment variable, and adding one would fail the bootstrap
		// whitelist in envboot_test.go for good reason.
		Setting{Key: KeyRegistryTokenKey,
			Kind: KindSecret, Redact: true, HotReload: false, Dangerous: true,
			Desc: "RS256 private key (PKCS#8 PEM) signing hosted-registry Bearer tokens (Docker v2 token flow). " +
				"Empty = generated once and persisted into the encrypted store, so every HA replica verifies " +
				"tokens minted by any other. Replacing it invalidates all outstanding registry tokens " +
				"(in-flight docker push/pull will fail); takes effect after a restart."},
		// Settings-only: a pure runtime policy knob, read per request. It has no
		// env var precisely because it never needs one — this is the shape every
		// non-bootstrap setting should take.
		Setting{Key: KeyOrgMaxPerUser, Kind: KindInt, HotReload: true,
			Desc: "Maximum orgs a single user may self-create (default 1; 0 = unlimited). " +
				"System admins are not subject to this limit. Hot-reloaded: takes effect on the next request."},
		Setting{Key: KeyCacheMaxBytes, Kind: KindInt, HotReload: true,
			Desc: "Pull-through cache capacity ceiling in bytes (0 = unlimited). " +
				"When usage exceeds this ceiling, the oldest unpinned entries are evicted on the next store. " +
				"Hot-reloaded: the new ceiling applies immediately; hosted (org-owned) content is never counted."},
	)
}

// SplitCSV splits a comma-separated setting value into trimmed, non-empty items;
// when v is empty, def is split instead. Both empty → nil (meaning "no filter").
func SplitCSV(v, def string) []string {
	if strings.TrimSpace(v) == "" {
		v = def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
