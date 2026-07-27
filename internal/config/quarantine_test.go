package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bug this locks down: storage.quarantine_dir did not exist, no handler was
// ever given WithQuarantineDir, so cache.Quarantine fell back to os.TempDir().
// Consequences observed on a real cluster:
//   - an image without /tmp (scratch, read-only rootfs) failed EVERY cache fill
//     with "quarantine create temp: no such file or directory" while /healthz
//     stayed 200 — a daemon that looks healthy and caches nothing;
//   - multi-GB OCI layers (blob.go DurablePartialPath) streamed into /tmp, which
//     is a tmpfs (RAM) on many systemd hosts and the ephemeral container layer
//     under Kubernetes — never the data volume the operator sized for it.
//
// So the invariant is: after Load/defaults the quarantine dir is a non-empty
// path under the data dir, and NEVER the system temp dir.
func TestQuarantineDirDefaultsUnderDataDir(t *testing.T) {
	cfg := &Config{}
	if err := applyStorageDefaults(cfg); err != nil {
		t.Fatalf("applyStorageDefaults: %v", err)
	}

	got := cfg.Storage.QuarantineDir
	if got == "" {
		t.Fatal("quarantine dir is empty — cache.Quarantine would fall back to os.TempDir()")
	}
	dataDir, err := DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataDir, "quarantine"); got != want {
		t.Fatalf("quarantine dir = %q, want %q", got, want)
	}
	if got == os.TempDir() || strings.HasPrefix(got, os.TempDir()+string(os.PathSeparator)) {
		t.Fatalf("quarantine dir %q sits under the system temp dir — multi-GB layers would land on tmpfs", got)
	}
}

// An explicit value must win: operators put this on the data volume.
func TestQuarantineDirExplicitWins(t *testing.T) {
	cfg := &Config{}
	cfg.Storage.QuarantineDir = "/data/specula/quarantine"
	if err := applyStorageDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Storage.QuarantineDir; got != "/data/specula/quarantine" {
		t.Fatalf("explicit quarantine dir overwritten: %q", got)
	}
}

// "~/…" must expand like every other path field, or the daemon creates a
// literal "~" directory next to its working dir.
func TestQuarantineDirExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cfg := &Config{}
	cfg.Storage.QuarantineDir = "~/.specula/quarantine"
	if err := expandConfigPaths(cfg); err != nil {
		t.Fatalf("expandConfigPaths: %v", err)
	}
	if want := filepath.Join(home, ".specula", "quarantine"); cfg.Storage.QuarantineDir != want {
		t.Fatalf("quarantine dir = %q, want %q", cfg.Storage.QuarantineDir, want)
	}
}

// EffectiveQuarantineDir is what the mount* functions call. It must never hand a
// handler an empty string — that is exactly the /tmp fallback this fixes — and
// must tolerate a nil receiver.
func TestEffectiveQuarantineDirNeverEmpty(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.EffectiveQuarantineDir(); got == "" {
		t.Fatal("nil Config yielded an empty quarantine dir")
	}

	cfg := &Config{}
	if got := cfg.EffectiveQuarantineDir(); got == "" {
		t.Fatal("zero Config yielded an empty quarantine dir")
	}

	cfg.Storage.QuarantineDir = "/explicit/dir"
	if got := cfg.EffectiveQuarantineDir(); got != "/explicit/dir" {
		t.Fatalf("EffectiveQuarantineDir = %q, want the configured value", got)
	}
}

// The embedded example must carry the key, because `specula install` derives
// /etc/specula/specula.yaml from it and patchConfigForSystemInstall rewrites
// ~/.specula paths to /var/lib/specula. A missing key there means every systemd
// install silently keeps quarantining into /tmp.
func TestEmbeddedExampleDeclaresQuarantineDir(t *testing.T) {
	if !strings.Contains(string(ExampleYAML), "quarantine_dir") {
		t.Fatal("embedded example config does not mention quarantine_dir")
	}
	path := filepath.Join(t.TempDir(), "specula.yaml")
	if _, err := WriteExampleIfMissing(path, nil); err != nil {
		t.Fatalf("write example: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if cfg.Storage.QuarantineDir == "" {
		t.Fatal("embedded example parses to an empty quarantine dir")
	}
}
