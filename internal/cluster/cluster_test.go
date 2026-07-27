package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveChartDir(t *testing.T) {
	t.Parallel()
	// Walk from repo root (test cwd is package dir when go test runs from module).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/cluster → repo root
	root := filepath.Clean(filepath.Join(wd, "../.."))
	chart := filepath.Join(root, "deploy/helm/specula-bootstrap")
	if _, err := os.Stat(filepath.Join(chart, "Chart.yaml")); err != nil {
		t.Skip("chart not found from test cwd")
	}
	got, err := ResolveChartDir(chart)
	if err != nil {
		t.Fatal(err)
	}
	if got != chart {
		t.Fatalf("got %q want %q", got, chart)
	}
	_, err = ResolveChartDir("/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing chart")
	}
}

func TestCertsDirForRuntime(t *testing.T) {
	t.Parallel()
	if CertsDirForRuntime("k3s") != "/var/lib/rancher/k3s/agent/etc/containerd/certs.d" {
		t.Fatal("k3s certs dir")
	}
	if CertsDirForRuntime("minikube") != "/etc/containerd/certs.d" {
		t.Fatal("minikube certs dir")
	}
}

func TestParseImageViaSplit(t *testing.T) {
	// Mirrors cmd/specula parseImage logic lightly.
	cases := map[string][2]string{
		"specula:local":     {"specula", "local"},
		"ivanzz/specula:v1": {"ivanzz/specula", "v1"},
	}
	for ref, want := range cases {
		repo, tag := ref, "latest"
		for i := len(ref) - 1; i >= 0; i-- {
			if ref[i] == ':' {
				ok := true
				for _, c := range ref[i+1:] {
					if c == '/' {
						ok = false
						break
					}
				}
				if ok {
					repo, tag = ref[:i], ref[i+1:]
				}
				break
			}
		}
		if repo != want[0] || tag != want[1] {
			t.Fatalf("%s: got %s:%s want %s:%s", ref, repo, tag, want[0], want[1])
		}
	}
}
