package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Why removal exists.
//
// `cluster uninstall` deleted the helm release — Deployment, DaemonSets, Service —
// and left every node's certs.d/<registry>/hosts.toml still pointing at
// http://127.0.0.1:30732. CN mode deliberately writes no public `server =`
// fallback, so those nodes then fail EVERY image pull for the redirected
// registries. Verified on a real ACK cluster: a probe Pod pulling
// registry.k8s.io/pause:3.10 got ErrImagePull while nothing was listening on the
// NodePort. Uninstall has to be able to put the node back.
func TestRemoveContainerdHostsRemovesOnlyOurs(t *testing.T) {
	certs := t.TempDir()
	endpoint := "http://127.0.0.1:30732"

	if err := WriteContainerdHosts(MirrorOptions{
		CertsDir:   certs,
		Endpoint:   endpoint,
		Registries: []string{"docker.io", "registry.k8s.io"},
		SkipVerify: true,
	}); err != nil {
		t.Fatal(err)
	}

	// An unrelated drop-in the operator wrote themselves. Removal must not touch it.
	foreign := filepath.Join(certs, "my-registry.internal")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignBody := "[host.\"https://mirror.corp\"]\n  capabilities = [\"pull\"]\n"
	if err := os.WriteFile(filepath.Join(foreign, "hosts.toml"), []byte(foreignBody), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := RemoveContainerdHosts(certs, endpoint)
	if err != nil {
		t.Fatalf("RemoveContainerdHosts: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d files, want 2", n)
	}
	for _, reg := range []string{"docker.io", "registry.k8s.io"} {
		if _, err := os.Stat(filepath.Join(certs, reg, "hosts.toml")); !os.IsNotExist(err) {
			t.Fatalf("%s/hosts.toml survived (err=%v)", reg, err)
		}
		// The now-empty directory should go too, so `ls certs.d` looks untouched.
		if _, err := os.Stat(filepath.Join(certs, reg)); !os.IsNotExist(err) {
			t.Fatalf("%s dir survived", reg)
		}
	}
	got, err := os.ReadFile(filepath.Join(foreign, "hosts.toml"))
	if err != nil {
		t.Fatalf("foreign drop-in was deleted: %v", err)
	}
	if string(got) != foreignBody {
		t.Fatalf("foreign drop-in modified:\n%s", got)
	}
}

// A hosts.toml that mentions a DIFFERENT Specula endpoint (another cluster, a
// different NodePort) is not ours to delete.
func TestRemoveContainerdHostsLeavesOtherEndpoints(t *testing.T) {
	certs := t.TempDir()
	if err := WriteContainerdHosts(MirrorOptions{
		CertsDir:   certs,
		Endpoint:   "http://127.0.0.1:31000",
		Registries: []string{"docker.io"},
		SkipVerify: true,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := RemoveContainerdHosts(certs, "http://127.0.0.1:30732")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("removed %d, want 0 — a different endpoint is not ours", n)
	}
	if _, err := os.Stat(filepath.Join(certs, "docker.io", "hosts.toml")); err != nil {
		t.Fatalf("file for another endpoint was deleted: %v", err)
	}
}

// Empty endpoint would match everything — refuse rather than wipe certs.d.
func TestRemoveContainerdHostsRequiresEndpoint(t *testing.T) {
	if _, err := RemoveContainerdHosts(t.TempDir(), ""); err == nil {
		t.Fatal("empty endpoint must be refused")
	}
	if _, err := RemoveContainerdHosts("", "http://127.0.0.1:30732"); err == nil {
		t.Fatal("empty certs dir must be refused")
	}
}

// Idempotent: running cleanup twice, or on a node that never had Specula, is a
// no-op rather than an error — the DaemonSet may land on both kinds of node.
func TestRemoveContainerdHostsIdempotent(t *testing.T) {
	certs := t.TempDir()
	endpoint := "http://127.0.0.1:30732"
	if err := WriteContainerdHosts(MirrorOptions{
		CertsDir: certs, Endpoint: endpoint,
		Registries: []string{"docker.io"}, SkipVerify: true,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := RemoveContainerdHosts(certs, endpoint); err != nil || n != 1 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}
	if n, err := RemoveContainerdHosts(certs, endpoint); err != nil || n != 0 {
		t.Fatalf("second pass must be a no-op: n=%d err=%v", n, err)
	}
	if n, err := RemoveContainerdHosts(filepath.Join(certs, "absent"), endpoint); err != nil || n != 0 {
		t.Fatalf("missing certs.d must be a no-op: n=%d err=%v", n, err)
	}
}

// Host-and-port matching must not be fooled by scheme or a trailing slash: the
// DaemonSet may be configured with any of these spellings.
func TestRemoveContainerdHostsMatchesEndpointSpellings(t *testing.T) {
	for _, spelling := range []string{
		"http://127.0.0.1:30732",
		"http://127.0.0.1:30732/",
		"127.0.0.1:30732",
	} {
		certs := t.TempDir()
		if err := WriteContainerdHosts(MirrorOptions{
			CertsDir: certs, Endpoint: "http://127.0.0.1:30732",
			Registries: []string{"docker.io"}, SkipVerify: true,
		}); err != nil {
			t.Fatal(err)
		}
		n, err := RemoveContainerdHosts(certs, spelling)
		if err != nil || n != 1 {
			t.Fatalf("spelling %q: n=%d err=%v", spelling, n, err)
		}
	}
}

// Sanity: the file we write really does contain the endpoint we match on, or the
// whole removal contract is vacuous.
func TestWrittenHostsContainEndpoint(t *testing.T) {
	certs := t.TempDir()
	if err := WriteContainerdHosts(MirrorOptions{
		CertsDir: certs, Endpoint: "http://127.0.0.1:30732",
		Registries: []string{"registry.k8s.io"}, SkipVerify: true,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(certs, "registry.k8s.io", "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "127.0.0.1:30732") {
		t.Fatalf("written hosts.toml does not mention the endpoint:\n%s", b)
	}
}
