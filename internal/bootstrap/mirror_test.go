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
	require.Contains(t, s, `server = "https://registry-1.docker.io"`)
	require.Contains(t, s, `[host."http://127.0.0.1:30732"]`)
	require.Contains(t, s, `capabilities = ["pull", "resolve"]`)
	require.Contains(t, s, `skip_verify = true`)
	require.NotContains(t, s, "override_path")

	got2, err := os.ReadFile(filepath.Join(dir, "registry.k8s.io", "hosts.toml"))
	require.NoError(t, err)
	s2 := string(got2)
	require.Contains(t, s2, `server = "https://registry.k8s.io"`)
	require.Contains(t, s2, `[host."http://127.0.0.1:30732/v2/registry.k8s.io"]`)
	require.Contains(t, s2, `override_path = true`)
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
	require.Contains(t, s, `override_path = true`)
	require.Contains(t, s, `[host."https://mirror.example:443/v2/ghcr.io"]`)
}
