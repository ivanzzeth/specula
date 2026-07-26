package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	body, err := renderUnit("/opt/specula/bin/specula", "/opt/specula/specula.yaml", "cache")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "User=cache") || !strings.Contains(body, "Group=cache") {
		t.Fatalf("user not patched:\n%s", body)
	}
	if !strings.Contains(body, "ExecStart=/opt/specula/bin/specula --config /opt/specula/specula.yaml") {
		t.Fatalf("exec not patched:\n%s", body)
	}
	if !strings.Contains(body, "WantedBy=multi-user.target") {
		t.Fatal("missing WantedBy")
	}
}

func TestPatchConfigForSystemInstall(t *testing.T) {
	in := "root: ~/.specula/blobs\ndsn: ~/.specula/meta.db\nmirror_dir: ~/.specula/git\n"
	out := patchConfigForSystemInstall(in)
	if strings.Contains(out, "~/.specula") {
		t.Fatalf("still has ~/.specula: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/blobs") {
		t.Fatalf("missing blobs path: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/meta.db") {
		t.Fatalf("missing meta path: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/git") {
		t.Fatalf("missing git path: %s", out)
	}
}

// TestChownPathRecursive: service install must fix nested data dirs, not only
// /var/lib/specula itself — otherwise a root wipe of blobs/ leaves the daemon
// with permission denied on mkdir shard (no hand-chown required; reinstall fixes it).
func TestChownPathRecursive(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "blobs", "ab")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(nested, "x")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := chownPath(dir, u.Username); err != nil {
		t.Fatalf("chownPath: %v", err)
	}
	// Walk succeeded over nested paths (regression: old chownPath only touched dir).
	var seen int
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			seen++
		}
		return err
	})
	if seen < 3 {
		t.Fatalf("expected nested walk ≥3 paths, got %d", seen)
	}
}
