package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCRIReloadStamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h1 := CRIReloadHash("/etc/containerd/certs.d")
	h2 := CRIReloadHash("/var/lib/rancher/k3s/agent/etc/containerd/certs.d")
	if h1 == h2 {
		t.Fatal("hashes should differ")
	}
	if !NeedsCRIReload(dir, h1) {
		t.Fatal("missing stamp => need reload")
	}
	if err := WriteCRIReloadStamp(dir, h1); err != nil {
		t.Fatal(err)
	}
	if NeedsCRIReload(dir, h1) {
		t.Fatal("same hash => no reload")
	}
	if !NeedsCRIReload(dir, h2) {
		t.Fatal("changed hash => need reload")
	}
	raw, err := os.ReadFile(filepath.Join(dir, CRIReloadStampFile))
	if err != nil || string(raw) == "" {
		t.Fatalf("stamp file: %v %q", err, raw)
	}
}
