package main

// main_ttl_sentinel_test.go — the TTL=0 sentinel is unreachable per-protocol.
//
// ARCHITECTURE §3 and specula.example.yaml both document the mutable-TTL
// sentinels as:
//
//	-1 = never revalidate
//	 0 = always revalidate (every request)
//
// internal/cache/cache.go isMutableFresh() implements both correctly. Only
// config RESOLUTION is wrong: mutableTTL() read
//
//	if pc.MutableTTLSeconds != 0 { return pc.MutableTTLSeconds }
//	return cfg.Cache.DefaultMutableTTLSeconds
//
// which conflates the documented SENTINEL 0 with "the operator did not set
// this", so a per-protocol 0 is silently replaced by the global default.
//
// This is not academic: our own shipped specula.example.yaml sets
// apt.mutable_ttl_seconds: 0 with the comment "always revalidate: InRelease has
// its own expiry field", and that 0 became 300s.
//
// Go's zero value makes this a systematic hazard rather than a one-off typo:
// any int64 config field whose zero is MEANINGFUL cannot express "unset" at
// all. The fix is at the type level — *int64, where nil is unset and 0 is the
// sentinel — so the bug becomes unrepresentable rather than merely corrected.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/config"
)

// writeCfg writes a YAML config to a temp file and loads it.
func writeCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "specula.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	cfg, err := config.Load(p)
	require.NoError(t, err)
	return cfg
}

const ttlCfgHeader = `
server:
  data_plane_addr: "127.0.0.1:0"
  control_plane_addr: "127.0.0.1:0"
storage:
  blob:
    driver: local
    local:
      root: /tmp/specula-ttl-test
  meta:
    driver: sqlite
    dsn: /tmp/specula-ttl-test.db
`

// TestMutableTTL_PerProtocolZero_IsTheSentinel_NotUnset is the RED for bug 2.
//
// The operator wrote exactly what the docs told them to write. The resolved TTL
// must be the sentinel 0, NOT the global default.
func TestMutableTTL_PerProtocolZero_IsTheSentinel_NotUnset(t *testing.T) {
	cfg := writeCfg(t, ttlCfgHeader+`
cache:
  default_mutable_ttl_seconds: 300
protocols:
  pypi:
    mutable_ttl_seconds: 0
    upstreams:
      - name: up
        base_url: https://example.invalid
        priority: 1
`)

	got := mutableTTL(cfg.Protocols["pypi"], cfg)

	assert.Equal(t, config.TTLAlwaysRevalidate, got,
		"per-protocol mutable_ttl_seconds: 0 is the DOCUMENTED sentinel "+
			"'revalidate every request' (ARCHITECTURE §3), not 'unset'. Resolving it "+
			"to the global default (300) silently breaks our own shipped "+
			"specula.example.yaml, where apt.mutable_ttl_seconds: 0 carries the "+
			"comment 'always revalidate: InRelease has its own expiry field'.")
}

// TestMutableTTL_PerProtocolUnset_FallsBackToGlobalDefault pins the OTHER half
// of the contract. Without this, "return pc.MutableTTLSeconds unconditionally"
// would pass the test above while destroying the fallback — the fix has to
// distinguish unset from 0, not just stop conflating them in one direction.
func TestMutableTTL_PerProtocolUnset_FallsBackToGlobalDefault(t *testing.T) {
	cfg := writeCfg(t, ttlCfgHeader+`
cache:
  default_mutable_ttl_seconds: 300
protocols:
  pypi:
    upstreams:
      - name: up
        base_url: https://example.invalid
        priority: 1
`)

	got := mutableTTL(cfg.Protocols["pypi"], cfg)

	assert.Equal(t, int64(300), got,
		"a protocol that does not mention mutable_ttl_seconds must inherit the "+
			"global default")
}

// TestMutableTTL_PerProtocolNeverRevalidate_ReachesHandler checks the -1
// sentinel for the same class of bug. -1 happens to survive the `!= 0` test, so
// it was never broken — this pins it so a future "fix" cannot break it.
func TestMutableTTL_PerProtocolNeverRevalidate_ReachesHandler(t *testing.T) {
	cfg := writeCfg(t, ttlCfgHeader+`
cache:
  default_mutable_ttl_seconds: 300
protocols:
  tarball:
    mutable_ttl_seconds: -1
    upstreams:
      - name: up
        base_url: https://example.invalid
        priority: 1
`)

	got := mutableTTL(cfg.Protocols["tarball"], cfg)

	assert.Equal(t, config.TTLNeverRevalidate, got,
		"per-protocol mutable_ttl_seconds: -1 is the 'never revalidate' sentinel")
}

// TestMutableTTL_GlobalZeroSentinel_ReachesUnsetProtocol — a GLOBAL default of 0
// is itself the sentinel, and a protocol that does not override it must inherit
// the sentinel rather than some substituted "sensible" number.
func TestMutableTTL_GlobalZeroSentinel_ReachesUnsetProtocol(t *testing.T) {
	cfg := writeCfg(t, ttlCfgHeader+`
cache:
  default_mutable_ttl_seconds: 0
protocols:
  pypi:
    upstreams:
      - name: up
        base_url: https://example.invalid
        priority: 1
`)

	got := mutableTTL(cfg.Protocols["pypi"], cfg)

	assert.Equal(t, config.TTLAlwaysRevalidate, got,
		"a global default of 0 is the always-revalidate sentinel and must reach a "+
			"protocol that does not override it")
}

// TestMutableTTL_ShippedExampleConfig_AptAlwaysRevalidates is the regression
// that matters most: it loads the ACTUAL shipped specula.example.yaml and
// asserts the behaviour its own comment promises. A test that only used a
// synthetic fixture would have stayed green while the shipped example lied.
func TestMutableTTL_ShippedExampleConfig_AptAlwaysRevalidates(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "specula.example.yaml"))
	require.NoError(t, err, "the shipped example config must load")

	apt, ok := cfg.Protocols["apt"]
	require.True(t, ok, "specula.example.yaml must configure apt")

	got := mutableTTL(apt, cfg)

	assert.Equal(t, config.TTLAlwaysRevalidate, got,
		"specula.example.yaml sets apt.mutable_ttl_seconds: 0 with the comment "+
			"'always revalidate: InRelease has its own expiry field'. Our own shipped "+
			"example must do what its comment says.")
}
