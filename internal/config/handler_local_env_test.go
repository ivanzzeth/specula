package config_test

import (
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_HandlerLocalEnvVar_DoesNotCrash is the regression test for BUG 2.
//
// SPECULA_GIT_UPSTREAM_SCHEME is a HANDLER-LOCAL variable: the git handler reads
// it directly via os.Getenv (internal/handler/git/git.go), it is documented
// there, and scripts/realclient-git.sh sets it. It is deliberately NOT a config
// key. But the koanf env provider slurped every SPECULA_* var into the config
// tree, so this one landed as the unknown root key "git_upstream_scheme" and the
// strict (ErrorUnused) decode fataled with:
//
//	config: unmarshal: '' has invalid keys: git_upstream_scheme
//
// which meant SPECULA_GIT_UPSTREAM_SCHEME=http scripts/realclient-git.sh could
// not even start specula against HEAD.
//
// RED before the fix: Load returns that error. GREEN after: Load succeeds.
func TestLoad_HandlerLocalEnvVar_DoesNotCrash(t *testing.T) {
	setenv(t, "SPECULA_GIT_UPSTREAM_SCHEME", "http")

	path := writeYAML(t, minimalYAML())
	_, err := config.Load(path)
	require.NoError(t, err,
		"a handler-local SPECULA_ env var (read via os.Getenv, not a config key) "+
			"must not crash the strict config decode")
}

// TestLoad_MisplacedEnvKey_StillRejected pins the load-bearing guarantee the fix
// must NOT weaken: an env var that maps to a genuinely unknown CONFIG key must
// still be rejected by the strict decode. Tolerating the handful of documented
// handler-local vars must not reopen the "misplaced key silently ignored" hole
// (the whole reason ErrorUnused exists — see main_sumdb_warn_test.go).
func TestLoad_MisplacedEnvKey_StillRejected(t *testing.T) {
	setenv(t, "SPECULA_TOTALLY_BOGUS_KEY", "x") // → unknown root key "totally_bogus_key"

	path := writeYAML(t, minimalYAML())
	_, err := config.Load(path)
	require.Error(t, err,
		"an unknown env-provided config key must still be rejected (strict decode intact)")
	assert.Contains(t, err.Error(), "totally_bogus_key",
		"error must name the unknown key so the operator can fix it")
}
