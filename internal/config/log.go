package config

import (
	"log/slog"
	"strings"
)

// LogConfig controls daemon log verbosity.
//
// Why this exists as configuration at all: the level used to be a hardcoded
// slog.LevelInfo, which made every DEBUG statement in the tree dead code. That
// included the per-mirror consensus poll results in internal/verify/consensus.go
// — logs written specifically so that an aggregate "0 of 2 mirrors responded"
// could be attributed to a mirror and an error after the fact. A diagnostic you
// cannot turn on is a diagnostic you do not have.
//
// Set it in YAML:
//
//	log:
//	  level: debug
//
// or, preferably during an incident, via the environment:
//
//	SPECULA_LOG__LEVEL=debug
//
// The env form is the one that matters: raising verbosity on a running cluster
// must not require editing and re-rolling a ConfigMap.
type LogConfig struct {
	// Level is one of debug, info, warn (warning), error. Case-insensitive.
	// Empty means info.
	Level string `koanf:"level"`
}

// SlogLevel maps the configured name onto a slog.Level.
//
// An unrecognised value falls back to info rather than failing the daemon or
// going silent. Rationale: this is a knob an operator reaches for *while
// something is already wrong*. A typo at that moment should cost them the extra
// verbosity they asked for — not the logs they already had, and not the process.
func (c LogConfig) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
