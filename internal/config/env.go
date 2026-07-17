package config

import (
	"os"
	"strings"
)

// envProvider implements koanf.Provider for SPECULA_* environment variable
// overrides. It satisfies the interface used when koanf.Load is called with
// a nil Parser (Read() path).
//
// Convention:
//
//	SPECULA_<LEVEL>__<LEVEL>__<KEY>
//
// The prefix (EnvPrefix) is stripped and the remainder is lowercased.
// Double underscores (__) denote a hierarchy boundary; single underscores
// within a segment are kept verbatim.
//
// Examples:
//
//	SPECULA_SERVER__DATA_PLANE_ADDR       → server.data_plane_addr
//	SPECULA_STORAGE__BLOB__DRIVER         → storage.blob.driver
//	SPECULA_PROTOCOLS__OCI__MUTABLE_TTL_SECONDS → protocols.oci.mutable_ttl_seconds
type envProvider struct {
	prefix string
}

func newEnvProvider(prefix string) *envProvider {
	return &envProvider{prefix: prefix}
}

// handlerLocalEnvVars are SPECULA_-prefixed variables that are deliberately
// read directly (os.Getenv) by a handler or an operator/test script and are NOT
// config keys. The env provider must NOT slurp them into the strict-decoded
// config tree: doing so lands them as unknown root keys (e.g. SPECULA_GIT_-
// UPSTREAM_SCHEME → "git_upstream_scheme") and the ErrorUnused decode fatals,
// so a documented var that a shipped script sets cannot start specula at all.
//
// This is an EXPLICIT allowlist, not a relaxation of strict decode: every real
// config key still round-trips through koanf and a genuinely misplaced or
// misspelled config key is still a hard error (that guarantee — see
// cmd/specula/main_sumdb_warn_test.go and config_test.go's UnknownKey tests — is
// load-bearing and must not be reopened). Add a name here only when a handler or
// script reads that exact variable via os.Getenv.
var handlerLocalEnvVars = map[string]struct{}{
	// Read by internal/handler/git/git.go; set by scripts/realclient-git.sh.
	"SPECULA_GIT_UPSTREAM_SCHEME": {},
	// Read by scripts/trust-oracle-signed.sh (cosign path); it also unset-s the
	// var defensively, but tolerating it here removes the collision at the source.
	"SPECULA_COSIGN_BIN": {},
}

// ReadBytes returns nil because this provider uses the Read() path.
func (e *envProvider) ReadBytes() ([]byte, error) {
	return nil, nil
}

// Read scans os.Environ() for variables with the configured prefix,
// transforms each matching key into a nested map path, and returns the
// resulting map. String values are left as strings; koanf's WeaklyTypedInput
// decoder handles coercion to int64/bool/etc at unmarshal time.
func (e *envProvider) Read() (map[string]any, error) {
	root := make(map[string]any)
	for _, env := range os.Environ() {
		idx := strings.Index(env, "=")
		if idx < 0 {
			continue
		}
		key, val := env[:idx], env[idx+1:]
		if !strings.HasPrefix(key, e.prefix) {
			continue
		}
		// Handler-local vars are read directly via os.Getenv and are not config
		// keys; skip them so strict decode never sees an unknown root key.
		if _, ok := handlerLocalEnvVars[key]; ok {
			continue
		}
		// Strip prefix, lowercase: SERVER__DATA_PLANE_ADDR → server__data_plane_addr
		stripped := strings.ToLower(strings.TrimPrefix(key, e.prefix))
		// Split on __ to get hierarchy levels.
		parts := strings.Split(stripped, "__")
		insertNested(root, parts, val)
	}
	return root, nil
}

// insertNested recursively inserts val into m at the path described by keys.
// Intermediate maps are created on demand; conflicting scalars are overwritten.
func insertNested(m map[string]any, keys []string, val any) {
	if len(keys) == 0 {
		return
	}
	if len(keys) == 1 {
		m[keys[0]] = val
		return
	}
	sub, ok := m[keys[0]].(map[string]any)
	if !ok {
		sub = make(map[string]any)
		m[keys[0]] = sub
	}
	insertNested(sub, keys[1:], val)
}
