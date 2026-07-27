package integrate

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveContainerdCertsDirs_NonEmpty(t *testing.T) {
	dirs := resolveContainerdCertsDirs()
	if len(dirs) != 1 {
		t.Fatalf("want exactly one certs.d root, got %#v", dirs)
	}
	if !strings.HasSuffix(filepath.Clean(dirs[0]), "certs.d") {
		t.Fatalf("expected certs.d suffix, got %s", dirs[0])
	}
	if isK3sNode() && dirs[0] != k3sAgentContainerdCerts {
		t.Fatalf("k3s node must use agent certs.d, got %s", dirs[0])
	}
	if !isK3sNode() && dirs[0] != systemContainerdCerts {
		t.Fatalf("non-k3s must use %s, got %s", systemContainerdCerts, dirs[0])
	}
}

func TestK3sAgentPathConstant(t *testing.T) {
	// Pin the path k3s actually reads — bootstrap chart docs + live clusters
	// agree on this agent tree (not /etc/containerd/certs.d).
	want := "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"
	if k3sAgentContainerdCerts != want {
		t.Fatalf("k3s certs.d constant drifted: %s", k3sAgentContainerdCerts)
	}
}

func TestDetectK3sNode_IgnoresStubCertsDir(t *testing.T) {
	// Specula bootstrap DS creates this path via hostPath DirectoryOrCreate on
	// non-k3s nodes — must NOT flip detection. A prior bug also wrote
	// config.toml under the stub tree; that must not count either.
	stubOnly := func(path string) bool {
		switch path {
		case k3sAgentContainerdCerts, "/var/lib/rancher/k3s",
			"/var/lib/rancher/k3s/agent/etc/containerd/config.toml":
			return true
		default:
			return false
		}
	}
	if detectK3sNode(stubOnly, stubOnly) {
		t.Fatal("stub certs.d / rancher tree / stub config.toml must not count as k3s")
	}
	if !detectK3sNode(func(p string) bool { return p == "/usr/local/bin/k3s" }, func(string) bool { return false }) {
		t.Fatal("k3s binary must count as k3s")
	}
	if !detectK3sNode(func(string) bool { return false }, func(p string) bool { return p == "/var/lib/rancher/k3s/server" }) {
		t.Fatal("k3s server data dir must count as k3s")
	}
}
