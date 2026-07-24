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
