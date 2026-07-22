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

	got2, err := os.ReadFile(filepath.Join(dir, "registry.k8s.io", "hosts.toml"))
	require.NoError(t, err)
	require.Contains(t, string(got2), `server = "https://registry.k8s.io"`)
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
	require.NotContains(t, string(got), "skip_verify")
}
