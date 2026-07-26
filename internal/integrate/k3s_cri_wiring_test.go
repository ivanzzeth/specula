package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

// TestK3sStyleLayout_HostsAndCRIConfigPath pins the production CP failure mode
// on a hermetic k3s-shaped tree:
//
//	$root/var/lib/rancher/k3s/agent/etc/containerd/certs.d/<reg>/hosts.toml
//	$root/var/lib/rancher/k3s/agent/etc/containerd/config.toml  (colon config_path)
//	$root/etc/rancher/k3s/registries.yaml
//
// Writing hosts alone is NOT enough — CRI ignores them until config_path is a
// single directory (containerd 2.2 transfer bug).
func TestK3sStyleLayout_HostsAndCRIConfigPath(t *testing.T) {
	root := t.TempDir()
	certs := filepath.Join(root, "var/lib/rancher/k3s/agent/etc/containerd/certs.d")
	cfgPath := filepath.Join(root, "var/lib/rancher/k3s/agent/etc/containerd/config.toml")
	regYAML := filepath.Join(root, "etc/rancher/k3s/registries.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(regYAML), 0o755))

	const endpoint = "http://10.0.0.1:7732"
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
version = 3
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '`+certs+`:/etc/docker/certs.d'
`), 0o644))

	// 1) hosts.toml (what bootstrap / integrate already wrote on the broken CP)
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   certs,
		Endpoint:   endpoint,
		Registries: []string{"registry.k8s.io", "docker.io"},
	}))
	k8sHosts, err := os.ReadFile(filepath.Join(certs, "registry.k8s.io", "hosts.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(k8sHosts), endpoint+"/v2/registry.k8s.io")
	assert.Contains(t, string(k8sHosts), "override_path")
	assert.NotContains(t, string(k8sHosts), "server =")

	// 2) CRI config_path still broken until we rewrite
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	needs, reason := criConfigPathNeedsFix(string(raw))
	require.True(t, needs, reason)

	r := fixOneContainerdConfigPath(cfgPath, certs, false, false)
	require.Equal(t, "added", r.Action, r.Detail)
	fixed, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Contains(t, string(fixed), "config_path = '"+certs+"'")
	assert.Contains(t, string(fixed), "io.containerd.transfer.v1.local")
	assert.NotContains(t, string(fixed), ":/etc/docker")
	needs, _ = containerdHostsConfigNeedsFix(string(fixed), certs)
	assert.False(t, needs)

	// 3) k3s registries.yaml for cluster-local Specula hostname (no TLS on http)
	out, already, err := mergeRegistriesYAMLBytes(nil, "specula.cluster.local", endpoint, "")
	require.NoError(t, err)
	require.False(t, already)
	require.NoError(t, os.WriteFile(regYAML, out, 0o644))
	body := string(out)
	assert.Contains(t, body, "specula.cluster.local")
	assert.Contains(t, body, endpoint)
	assert.NotContains(t, body, "insecure_skip_verify",
		"http:// Specula must not get k3s tls block (emits server=https://)")
}

// TestVanillaKubeadmLayout_ColonDefaultFixed is the non-k3s CP shape:
// /etc/containerd/certs.d + config.toml with the 2.2 default colon list.
func TestVanillaKubeadmLayout_ColonDefaultFixed(t *testing.T) {
	root := t.TempDir()
	certs := filepath.Join(root, "etc/containerd/certs.d")
	cfgPath := filepath.Join(root, "etc/containerd/config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o755))

	require.NoError(t, os.WriteFile(cfgPath, []byte(`
# containerd 2.2 default (from containerd config default)
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`), 0o644))

	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   certs,
		Endpoint:   "http://127.0.0.1:7732",
		Registries: bootstrap.DefaultOCIRegistries,
	}))

	r := fixOneContainerdConfigPath(cfgPath, certs, false, false)
	require.Equal(t, "added", r.Action)
	got, _ := os.ReadFile(cfgPath)
	assert.True(t, strings.Contains(string(got), "config_path = '"+certs+"'") ||
		strings.Contains(string(got), `config_path = "`+certs+`"`),
		string(got))
	assert.Contains(t, string(got), "transfer.v1.local")
	assert.NotContains(t, string(got), ":/etc/docker/certs.d")

	// Residual server= on an orphan registry must be stripped by WriteContainerdHosts.
	orphan := filepath.Join(certs, "orphan.example")
	require.NoError(t, os.MkdirAll(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "hosts.toml"),
		[]byte("server = \"https://orphan.example\"\n"), 0o644))
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   certs,
		Endpoint:   "http://127.0.0.1:7732",
		Registries: []string{"docker.io"},
	}))
	ob, _ := os.ReadFile(filepath.Join(orphan, "hosts.toml"))
	assert.NotContains(t, string(ob), "server =")
}

// TestBrokenHostDirJoin documents why colon config_path fails: transfer treats
// the whole string as one directory name.
func TestBrokenHostDirJoin(t *testing.T) {
	joined := filepath.Join("/etc/containerd/certs.d:/etc/docker/certs.d", "registry.k8s.io", "hosts.toml")
	assert.Equal(t,
		"/etc/containerd/certs.d:/etc/docker/certs.d/registry.k8s.io/hosts.toml",
		joined)
	_, err := os.Stat(joined)
	assert.True(t, os.IsNotExist(err), "literal colon path must not exist on disk")
}
