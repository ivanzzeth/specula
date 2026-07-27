package cluster

import (
	"strings"
	"testing"
)

// Why uninstall needs a node-cleanup pass.
//
// `cluster uninstall` removed the helm release and left every node's
// certs.d/<registry>/hosts.toml pointing at the NodePort. CN mode writes no public
// `server =` fallback on purpose, so those registries do not degrade once Specula
// is gone — they fail. Verified on a real ACK cluster: registry.k8s.io/pause:3.10
// → ErrImagePull with nothing behind the NodePort. Uninstall must undo what
// install wrote, using the release's own image (the only one guaranteed present on
// those nodes) before the release disappears.
func TestRenderNodeCleanupDaemonSet(t *testing.T) {
	y := RenderNodeCleanupDaemonSet(NodeCleanupSpec{
		Name:        "boot-specula-cleanup",
		Namespace:   "specula-boot",
		Image:       "crpi-x.cn-chengdu.personal.cr.aliyuncs.com/ivanzz/specula:v0.11.0",
		Endpoint:    "http://127.0.0.1:30732",
		CertsDir:    "/etc/containerd/certs.d",
		K3sCertsDir: "/var/lib/rancher/k3s/agent/etc/containerd/certs.d",
	})

	for _, want := range []string{
		"kind: DaemonSet",
		"name: boot-specula-cleanup",
		"namespace: specula-boot",
		"image: \"crpi-x.cn-chengdu.personal.cr.aliyuncs.com/ivanzz/specula:v0.11.0\"",
		"- bootstrap-mirror",
		"- remove",
		"--endpoint=http://127.0.0.1:30732",
		"--certs-dir=/host-certs.d",
		"--k3s-certs-dir=/host-k3s-certs.d",
		"--hold",
		// Must reach the host's certs.d, which means privileged + hostPath.
		"privileged: true",
		"hostPath:",
		"path: /etc/containerd/certs.d",
		"path: /var/lib/rancher/k3s/agent/etc/containerd/certs.d",
		// Must land on every node, including tainted ones — a control-plane node
		// runs containerd too and its hosts.toml was rewritten as well.
		"operator: Exists",
	} {
		if !strings.Contains(y, want) {
			t.Fatalf("manifest missing %q:\n%s", want, y)
		}
	}
}

// --hold keeps the Pod Ready after the removal so readiness is a usable "done"
// signal; without it the container exits and restartPolicy Always churns it.
func TestRenderNodeCleanupDaemonSetHolds(t *testing.T) {
	y := RenderNodeCleanupDaemonSet(NodeCleanupSpec{
		Name: "c", Namespace: "n", Image: "i", Endpoint: "http://127.0.0.1:30732",
		CertsDir: "/etc/containerd/certs.d",
	})
	if !strings.Contains(y, "--hold") {
		t.Fatal("cleanup container must hold so DaemonSet readiness means 'removal ran'")
	}
}

// k3s root is optional: an empty value must not emit a hostPath mount for it,
// or the Pod fails to start on a cluster that has no such directory.
func TestRenderNodeCleanupDaemonSetOmitsEmptyK3s(t *testing.T) {
	y := RenderNodeCleanupDaemonSet(NodeCleanupSpec{
		Name: "c", Namespace: "n", Image: "i", Endpoint: "http://127.0.0.1:30732",
		CertsDir: "/etc/containerd/certs.d",
	})
	if strings.Contains(y, "k3s-certs-dir") || strings.Contains(y, "host-k3s-certs") {
		t.Fatalf("empty k3s root must not be mounted:\n%s", y)
	}
}

// The image has to come from the release, not a guess: it is the one image proven
// pullable on those nodes, and in CN it may be the ONLY one.
func TestParseDaemonSetImage(t *testing.T) {
	got := parseDaemonSetImage("  crpi-x.cn-chengdu.personal.cr.aliyuncs.com/ivanzz/specula:v0.11.0\n")
	if got != "crpi-x.cn-chengdu.personal.cr.aliyuncs.com/ivanzz/specula:v0.11.0" {
		t.Fatalf("got %q", got)
	}
	if parseDaemonSetImage("") != "" {
		t.Fatal("empty input must yield empty, so the caller can skip cleanup")
	}
	// Several containers → first non-empty wins.
	if got := parseDaemonSetImage("\nimg-a\nimg-b\n"); got != "img-a" {
		t.Fatalf("got %q", got)
	}
}
