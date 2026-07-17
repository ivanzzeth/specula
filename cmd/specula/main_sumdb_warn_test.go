package main

// Regression test for Bug 2: Go sumdb never runs and degrades SILENTLY.
//
// Every other protocol builder (buildGPGVerifier, buildHelmProvVerifier,
// buildGitSignedVerifier) emits a log.Warn when the signed anchor is absent so
// the operator can see at startup that the protocol tops out at tofu tier.
// buildGoSumDBVerifier was the sole exception: it returned nil with no warning,
// leaving go as the only protocol that silently lost its signed anchor.
//
// RED: before the fix, buf is empty and the Contains assertion fires.
// GREEN: after the fix, the warning is logged and both assertions pass.

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ivanzzeth/specula/internal/config"
)

// TestBuildGoSumDBVerifier_AbsentSumDB_LogsWarn checks that buildGoSumDBVerifier
// emits a startup degradation warning when the "go" protocol is configured but
// has no sumdb block — the silent case that the E2E test exposed.
func TestBuildGoSumDBVerifier_AbsentSumDB_LogsWarn(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"go": {
				Upstreams: []config.UpstreamConfig{
					{Name: "goproxy", BaseURL: "https://proxy.golang.org", Priority: 1, Official: true},
				},
				// SumDB is nil — operator configured upstreams but forgot sumdb.
				//
				// A misplaced sumdb block (nested under "verification:") no longer
				// reaches this state: the loader runs koanf/mapstructure with
				// ErrorUnused, so config.Load HARD-FAILS with
				//   'protocols[go].verification' has invalid keys: sumdb
				// (pinned by config.TestLoad_UnknownKey_SumDBMisplacedUnderVerification).
				// It is not silently discarded.
				//
				// The warn-on-absent behaviour asserted below is still required for
				// the case this literal models: sumdb legitimately omitted, which
				// caps go at tofu — silently, unless we say so at startup.
			},
		},
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	v := buildGoSumDBVerifier(cfg, nil, log)

	// The verifier must be nil (no sumdb to configure) — this is unchanged.
	assert.Nil(t, v, "must return nil when sumdb block is absent")

	// Before fix: buf is empty — the function returns nil without logging.
	// After fix: the warning is present and both substring checks pass.
	assert.Contains(t, buf.String(), "go sumdb not configured",
		"must emit degradation warning when sumdb is absent (go is SILENT without fix)")
	assert.Contains(t, buf.String(), "tofu",
		"warning must mention that go tops out at tofu tier")
}

// TestBuildGoSumDBVerifier_GoProtocolAbsent_LogsWarn checks that even when the
// "go" protocol key is entirely absent from config (e.g., a minimal specula.yaml
// with no go: block), the warning is still emitted. This exercises the !ok branch.
func TestBuildGoSumDBVerifier_GoProtocolAbsent_LogsWarn(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			// "go" is completely absent
			"oci": {},
		},
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	v := buildGoSumDBVerifier(cfg, nil, log)

	assert.Nil(t, v, "must return nil when go protocol is absent")
	assert.Contains(t, buf.String(), "go sumdb not configured",
		"must emit degradation warning even when go protocol is entirely absent from config")
}
