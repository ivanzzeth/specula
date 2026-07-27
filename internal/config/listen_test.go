package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Specula now serves everything — artifact protocols, /token, Admin API, probes and
// the WebUI — on ONE port. Operators expose a single 7733 and put an Ingress in front
// of it; there is no second TCP port to publish, firewall or document.
//
// server.data_plane_addr is REMOVED, not deprecated: leaving it silently ignored
// would have people believe 7732 is still being served.
func TestDataPlaneAddrIsRejected(t *testing.T) {
	cfg := minimalConfig()
	cfg.Server.DataPlaneAddr = "0.0.0.0:7732"
	err := Validate(cfg)
	if err == nil {
		t.Fatal("server.data_plane_addr must be a hard error, not ignored")
	}
	msg := err.Error()
	for _, want := range []string{"data_plane_addr", "listen_addr"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must name the removed key and its replacement (%q): %s", want, msg)
		}
	}
}

// listen_addr is the canonical field.
func TestEffectiveListenAddrPrefersListenAddr(t *testing.T) {
	cfg := minimalConfig()
	cfg.Server.ListenAddr = "0.0.0.0:9000"
	cfg.Server.ControlPlaneAddr = "0.0.0.0:7733"
	if got := cfg.EffectiveListenAddr(); got != "0.0.0.0:9000" {
		t.Fatalf("EffectiveListenAddr = %q, want the explicit listen_addr", got)
	}
}

// control_plane_addr keeps working as an alias: every existing config, chart and
// ConfigMap in the wild sets it, and it already pointed at the port we keep.
func TestEffectiveListenAddrFallsBackToControlPlaneAddr(t *testing.T) {
	cfg := minimalConfig()
	cfg.Server.ListenAddr = "" // only the alias is set
	cfg.Server.ControlPlaneAddr = "127.0.0.1:8443"
	if got := cfg.EffectiveListenAddr(); got != "127.0.0.1:8443" {
		t.Fatalf("EffectiveListenAddr = %q, want the control_plane_addr alias", got)
	}
}

// Neither set: the default, and it must never be empty (an empty Addr makes
// net/http listen on :80).
func TestEffectiveListenAddrDefaults(t *testing.T) {
	cfg := minimalConfig()
	cfg.Server.ListenAddr = ""
	cfg.Server.ControlPlaneAddr = ""
	got := cfg.EffectiveListenAddr()
	if got != DefaultListenAddr {
		t.Fatalf("EffectiveListenAddr = %q, want %q", got, DefaultListenAddr)
	}
	if !strings.HasSuffix(got, ":7733") {
		t.Fatalf("default must stay on 7733, got %q", got)
	}
	var nilCfg *Config
	if nilCfg.EffectiveListenAddr() != DefaultListenAddr {
		t.Fatal("nil Config must still yield the default, never an empty Addr")
	}
}

// A loaded config gets the default applied, so nothing downstream has to re-derive
// it, and 7732 appears nowhere.
func TestLoadedDefaultsAreSinglePort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "specula.yaml")
	if _, err := WriteExampleIfMissing(path, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if cfg.Server.DataPlaneAddr != "" {
		t.Fatalf("the example still sets data_plane_addr: %q", cfg.Server.DataPlaneAddr)
	}
	if got := cfg.EffectiveListenAddr(); !strings.HasSuffix(got, ":7733") {
		t.Fatalf("example listen addr = %q, want :7733", got)
	}
	if strings.Contains(string(ExampleYAML), "7732") {
		t.Error("the embedded example still mentions 7732")
	}
	if strings.Contains(string(ExampleYAML), "data_plane_addr") {
		t.Error("the embedded example still sets data_plane_addr")
	}
}

// minimalConfig is a config that passes Validate, so a test can isolate one field.
func minimalConfig() *Config {
	cfg := &Config{}
	cfg.Server.ListenAddr = DefaultListenAddr
	cfg.Storage.Blob.Driver = "local"
	cfg.Storage.Blob.Local.Root = "/tmp/specula-blobs"
	cfg.Storage.Meta.Driver = "sqlite"
	cfg.Storage.Meta.DSN = "/tmp/specula-meta.db"
	cfg.Storage.QuarantineDir = "/tmp/specula-quarantine"
	return cfg
}
