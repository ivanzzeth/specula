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
