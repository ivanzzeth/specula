package config_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/config"
)

// The log level used to be hardcoded to slog.LevelInfo in cmd/specula/main.go,
// which made every DEBUG diagnostic in the tree unreachable — including the
// per-mirror consensus poll results that exist specifically so that an
// aggregate "0 responded" can be root-caused after the fact. Diagnostics you
// cannot turn on are diagnostics you do not have.

func TestLoad_LogLevel_DefaultsToInfo(t *testing.T) {
	path := writeYAML(t, minimalYAML())

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, slog.LevelInfo, cfg.Log.SlogLevel(),
		"an unset log.level must stay at info — quiet by default, not silent and not chatty")
}

func TestLoad_LogLevel_FromYAML(t *testing.T) {
	path := writeYAML(t, minimalYAML()+`
log:
  level: debug
`)

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, slog.LevelDebug, cfg.Log.SlogLevel())
}

// The env override is the one that matters operationally: raising verbosity
// during an incident must not require editing (and re-deploying) a ConfigMap.
func TestLoad_LogLevel_EnvOverride(t *testing.T) {
	path := writeYAML(t, minimalYAML()+`
log:
  level: warn
`)
	setenv(t, "SPECULA_LOG__LEVEL", "debug")

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, slog.LevelDebug, cfg.Log.SlogLevel(),
		"SPECULA_LOG__LEVEL must win over the file so an operator can raise verbosity without a redeploy")
}

func TestLogConfig_SlogLevel_Table(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"empty defaults to info", "", slog.LevelInfo},
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"uppercase is accepted", "DEBUG", slog.LevelDebug},
		{"surrounding space is trimmed", "  debug  ", slog.LevelDebug},
		// A typo must not silence the daemon. Falling back to info keeps the
		// operator's mistake visible (they still get logs) instead of turning
		// the process mute at the moment they were trying to see more.
		{"unknown falls back to info", "trace", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.LogConfig{Level: tc.in}
			assert.Equal(t, tc.want, cfg.SlogLevel())
		})
	}
}
