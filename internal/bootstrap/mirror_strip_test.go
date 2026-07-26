package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

func TestStripPublicServerFallback_RemovesResidualServer(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "registry.k8s.io")
	require.NoError(t, os.MkdirAll(legacy, 0o755))
	body := "server = \"https://registry.k8s.io\"\n\n[host.\"http://127.0.0.1:7732/v2/registry.k8s.io\"]\n  capabilities = [\"pull\", \"resolve\"]\n  override_path = true\n"
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "hosts.toml"), []byte(body), 0o644))

	n, err := bootstrap.StripPublicServerFallback(dir)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := os.ReadFile(filepath.Join(legacy, "hosts.toml"))
	require.NoError(t, err)
	require.NotContains(t, string(got), "server =")
	require.Contains(t, string(got), "override_path")
}

func TestWriteContainerdHosts_StripsSiblingServerFallback(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "orphan.example")
	require.NoError(t, os.MkdirAll(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "hosts.toml"),
		[]byte("server = \"https://orphan.example\"\n"), 0o644))

	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "http://127.0.0.1:7732",
		Registries: []string{"docker.io"},
	}))

	got, err := os.ReadFile(filepath.Join(orphan, "hosts.toml"))
	require.NoError(t, err)
	require.NotContains(t, string(got), "server =")
}
