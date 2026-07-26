package bootstrap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

func TestWriteContainerdHosts_DockerIO(t *testing.T) {
	dir := t.TempDir()
	err := bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "http://127.0.0.1:30732",
		Registries: []string{"docker.io", "registry.k8s.io"},
		SkipVerify: true,
	})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "docker.io", "hosts.toml"))
	require.NoError(t, err)
	s := string(got)
	// No public `server =` fallback — that lets containerd dial Hub/pkg.dev
	// when Specula is slow (CN kubeadm hang class).
	require.NotContains(t, s, "server =")
	require.Contains(t, s, `[host."http://127.0.0.1:30732"]`)
	require.Contains(t, s, `capabilities = ["pull", "resolve"]`)
	require.Contains(t, s, `skip_verify = true`)
	require.NotContains(t, s, "override_path")

	got2, err := os.ReadFile(filepath.Join(dir, "registry.k8s.io", "hosts.toml"))
	require.NoError(t, err)
	s2 := string(got2)
	require.NotContains(t, s2, "server =")
	require.Contains(t, s2, `[host."http://127.0.0.1:30732/v2/registry.k8s.io"]`)
	require.Contains(t, s2, `override_path = true`)
}

// TestWriteContainerdHosts_NoPublicServerFallback pins the CN failure mode:
// hosts.toml must not list registry.k8s.io / pkg.dev as a containerd fallback.
func TestWriteContainerdHosts_NoPublicServerFallback(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "https://127.0.0.1:7732",
		Registries: []string{"registry.k8s.io", "ghcr.io"},
		CaFile:     "/etc/specula/ca.crt",
	}))
	for _, reg := range []string{"registry.k8s.io", "ghcr.io"} {
		body, err := os.ReadFile(filepath.Join(dir, reg, "hosts.toml"))
		require.NoError(t, err)
		s := string(body)
		require.NotContains(t, s, "server =", reg)
		require.NotContains(t, s, "pkg.dev", reg)
		require.Contains(t, s, "127.0.0.1:7732", reg)
	}
}

func TestWriteContainerdHosts_RequiresFields(t *testing.T) {
	err := bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "certs-dir") ||
		strings.Contains(err.Error(), "endpoint") ||
		strings.Contains(err.Error(), "registry"))
}

func TestWriteContainerdHosts_NoSkipVerify(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "https://mirror.example:443",
		Registries: []string{"ghcr.io"},
		SkipVerify: false,
	}))
	got, err := os.ReadFile(filepath.Join(dir, "ghcr.io", "hosts.toml"))
	require.NoError(t, err)
	s := string(got)
	require.NotContains(t, s, "skip_verify")
	require.NotContains(t, s, "ca =")
	require.Contains(t, s, `override_path = true`)
	require.Contains(t, s, `[host."https://mirror.example:443/v2/ghcr.io"]`)
}

func TestWriteContainerdHosts_CAFile(t *testing.T) {
	dir := t.TempDir()
	const caPath = "/etc/specula/ca.crt"
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "https://mirror.example:443",
		Registries: []string{"ghcr.io"},
		CaFile:     caPath,
	}))
	got, err := os.ReadFile(filepath.Join(dir, "ghcr.io", "hosts.toml"))
	require.NoError(t, err)
	s := string(got)
	require.Contains(t, s, `ca = ["/etc/specula/ca.crt"]`)
	require.NotContains(t, s, "skip_verify")
}
