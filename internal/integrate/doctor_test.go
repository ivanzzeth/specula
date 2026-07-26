package integrate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoctor_FlagsColonConfigPath(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.toml")
	certs := filepath.Join(root, "certs.d")
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "registry.k8s.io"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "docker.io"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "registry.k8s.io", "hosts.toml"), []byte(`
host."http://127.0.0.1:7732/v2/registry.k8s.io".capabilities = ["pull"]
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "docker.io", "hosts.toml"), []byte(`
host."http://127.0.0.1:7732".capabilities = ["pull"]
`), 0o644))
	require.NoError(t, os.WriteFile(cfg, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '`+certs+`:/etc/docker/certs.d'
`), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rep, err := Doctor(DoctorOptions{
		Home:        root,
		Addr:        srv.URL,
		ConfigTOMLs: []string{cfg},
		CertsDirs:   []string{certs},
		DumpConfig:  func(context.Context) (string, error) { return "", assert.AnError },
	})
	require.NoError(t, err)
	assert.True(t, ReportHasBlockingFindings(rep))
	var found bool
	for _, r := range rep.Results {
		if r.Action == "risk" && strings.Contains(r.Detail, "config_path") {
			found = true
			assert.Equal(t, cfg, r.Path)
		}
	}
	assert.True(t, found, "expected colon config_path risk: %+v", rep.Results)
}

func TestDoctor_FlagsServerFallbackAndMissingHosts(t *testing.T) {
	root := t.TempDir()
	certs := filepath.Join(root, "certs.d")
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "ghcr.io"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "ghcr.io", "hosts.toml"), []byte(`
server = "https://ghcr.io/v2"
host."http://127.0.0.1:7732/v2/ghcr.io".capabilities = ["pull"]
`), 0o644))
	cfg := filepath.Join(root, "config.toml")
	require.NoError(t, os.WriteFile(cfg, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '`+certs+`'
`), 0o644))

	rep, err := Doctor(DoctorOptions{
		Home:        root,
		Addr:        "http://127.0.0.1:9",
		SkipProbe:   true,
		ConfigTOMLs: []string{cfg},
		CertsDirs:   []string{certs},
		DumpConfig:  func(context.Context) (string, error) { return "", assert.AnError },
	})
	require.NoError(t, err)
	assert.True(t, ReportHasBlockingFindings(rep))

	var serverRisk, missingRisk bool
	for _, r := range rep.Results {
		if r.Action != "risk" {
			continue
		}
		if strings.Contains(r.Detail, "server=") {
			serverRisk = true
		}
		if strings.Contains(r.Detail, "missing hosts.toml") {
			missingRisk = true
		}
	}
	assert.True(t, serverRisk, "server= risk: %+v", rep.Results)
	assert.True(t, missingRisk, "missing registry.k8s.io/docker.io: %+v", rep.Results)
}

func TestDoctor_EffectiveDumpColon(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.toml")
	certs := filepath.Join(root, "certs.d")
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "registry.k8s.io"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "docker.io"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "registry.k8s.io", "hosts.toml"), []byte("host.\"http://x\".capabilities=[\"pull\"]\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "docker.io", "hosts.toml"), []byte("host.\"http://x\".capabilities=[\"pull\"]\n"), 0o644))
	// File looks fixed…
	require.NoError(t, os.WriteFile(cfg, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '`+certs+`'
`), 0o644))

	rep, err := Doctor(DoctorOptions{
		Home:        root,
		SkipProbe:   true,
		ConfigTOMLs: []string{cfg},
		CertsDirs:   []string{certs},
		DumpConfig: func(context.Context) (string, error) {
			return `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`, nil
		},
	})
	require.NoError(t, err)
	var dumpRisk bool
	for _, r := range rep.Results {
		if r.Action == "risk" && strings.Contains(r.Path, "config dump") {
			dumpRisk = true
		}
	}
	assert.True(t, dumpRisk, "stale effective dump must be flagged: %+v", rep.Results)
}

func TestDoctor_CleanWhenSingleRootAndHostsOK(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.toml")
	certs := filepath.Join(root, "certs.d")
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "registry.k8s.io"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(certs, "docker.io"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "registry.k8s.io", "hosts.toml"), []byte(`
host."http://127.0.0.1:7732/v2/registry.k8s.io".capabilities = ["pull"]
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "docker.io", "hosts.toml"), []byte(`
host."http://127.0.0.1:7732".capabilities = ["pull"]
`), 0o644))
	require.NoError(t, os.WriteFile(cfg, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '`+certs+`'
`), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	rep, err := Doctor(DoctorOptions{
		Home:        root,
		Addr:        srv.URL,
		ConfigTOMLs: []string{cfg},
		CertsDirs:   []string{certs},
		DumpConfig:  func(context.Context) (string, error) { return "config_path = '" + certs + "'\n", nil },
	})
	require.NoError(t, err)
	assert.False(t, ReportHasBlockingFindings(rep), "clean node: %+v", rep.Results)
}

func TestHostsHasPublicServerFallback(t *testing.T) {
	assert.True(t, hostsHasPublicServerFallback("server = \"https://registry.k8s.io/v2\"\n"))
	assert.False(t, hostsHasPublicServerFallback("# server = \"https://x\"\nhost.\"http://y\".capabilities=[\"pull\"]\n"))
}
