package integrate

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsColonSeparatedCertsPath(t *testing.T) {
	assert.True(t, isColonSeparatedCertsPath(`/etc/containerd/certs.d:/etc/docker/certs.d`))
	assert.True(t, isColonSeparatedCertsPath(`/etc/containerd/certs.d:/etc/docker/certs.d`))
	assert.False(t, isColonSeparatedCertsPath(`/etc/containerd/certs.d`))
	assert.False(t, isColonSeparatedCertsPath(``))
	assert.False(t, isColonSeparatedCertsPath(`/etc/nri/conf.d`))
}

func TestCriConfigPathNeedsFix_Colon(t *testing.T) {
	content := `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`
	needs, reason := criConfigPathNeedsFix(content)
	assert.True(t, needs)
	assert.Contains(t, reason, "colon")
}

func TestCriConfigPathNeedsFix_Missing(t *testing.T) {
	content := `# bare kubeadm node — inherits 2.2 default colon path
version = 2
`
	needs, reason := criConfigPathNeedsFix(content)
	assert.True(t, needs)
	assert.Contains(t, reason, "missing")
}

func TestCriConfigPathNeedsFix_DisabledCRI(t *testing.T) {
	content := `disabled_plugins = ["cri"]
`
	needs, _ := criConfigPathNeedsFix(content)
	assert.False(t, needs)
}

func TestCriConfigPathNeedsFix_AlreadySingle(t *testing.T) {
	content := `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
`
	needs, reason := criConfigPathNeedsFix(content)
	assert.False(t, needs)
	assert.Contains(t, reason, "single-root")
}

func TestTransferConfigPathNeedsFix_Empty(t *testing.T) {
	content := `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
[plugins.'io.containerd.transfer.v1.local']
  config_path = ''
`
	needs, reason := transferConfigPathNeedsFix(content, "/etc/containerd/certs.d")
	assert.True(t, needs)
	assert.Contains(t, reason, "empty")
	needsAll, reasonAll := containerdHostsConfigNeedsFix(content, "/etc/containerd/certs.d")
	assert.True(t, needsAll)
	assert.Contains(t, reasonAll, "empty")
}

func TestEnsureTransferConfigPath_SetsEmpty(t *testing.T) {
	in := `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
[plugins.'io.containerd.transfer.v1.local']
  max_concurrent_downloads = 3
  config_path = ''
`
	out, changed := ensureTransferConfigPath(in, "/etc/containerd/certs.d")
	require.True(t, changed)
	assert.Contains(t, out, `config_path = '/etc/containerd/certs.d'`)
	assert.NotContains(t, out, `config_path = ''`)
	needs, _ := transferConfigPathNeedsFix(out, "/etc/containerd/certs.d")
	assert.False(t, needs)
}

func TestRewriteContainerdHostsConfigPaths_InjectsBoth(t *testing.T) {
	out, changed := rewriteContainerdHostsConfigPaths("version = 3\n", "/etc/containerd/certs.d")
	require.True(t, changed)
	assert.Contains(t, out, `io.containerd.cri.v1.images`)
	assert.Contains(t, out, `io.containerd.transfer.v1.local`)
	assert.Contains(t, out, `config_path = "/etc/containerd/certs.d"`)
}

func TestFixOneContainerdConfigPath_FixesTransferWhenCRIAlreadyOK(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"
	require.NoError(t, os.WriteFile(path, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
[plugins.'io.containerd.transfer.v1.local']
  config_path = ''
`), 0o644))
	r := fixOneContainerdConfigPath(path, "/etc/containerd/certs.d", false, false)
	assert.Equal(t, "added", r.Action, r.Detail)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "[plugins.'io.containerd.transfer.v1.local']")
	assert.Regexp(t, `config_path = ['"]/etc/containerd/certs\.d['"]`, string(raw))
	assert.NotContains(t, string(raw), `config_path = ''`)
}

func TestRewriteCRIConfigPath_ColonToSingle(t *testing.T) {
	in := `
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`
	out, changed := rewriteCRIConfigPath(in, "/etc/containerd/certs.d")
	require.True(t, changed)
	assert.Contains(t, out, `config_path = '/etc/containerd/certs.d'`)
	assert.NotContains(t, out, `:/etc/docker`)
}

func TestRewriteCRIConfigPath_InjectWhenMissing(t *testing.T) {
	in := "version = 3\n"
	out, changed := rewriteCRIConfigPath(in, "/etc/containerd/certs.d")
	require.True(t, changed)
	assert.Contains(t, out, `io.containerd.cri.v1.images`)
	assert.Contains(t, out, `config_path = "/etc/containerd/certs.d"`)
}

func TestRewriteCRIConfigPath_PreservesUnrelated(t *testing.T) {
	in := `
plugin_config_path = '/etc/nri/conf.d'
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`
	out, changed := rewriteCRIConfigPath(in, "/etc/containerd/certs.d")
	require.True(t, changed)
	assert.Contains(t, out, `plugin_config_path = '/etc/nri/conf.d'`)
}

func TestFixOneContainerdConfigPath_DryRun(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"
	require.NoError(t, os.WriteFile(path, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`), 0o644))
	r := fixOneContainerdConfigPath(path, "/etc/containerd/certs.d", true, false)
	assert.Equal(t, "added", r.Action)
	assert.Contains(t, r.Detail, "would")
	raw, _ := os.ReadFile(path)
	assert.Contains(t, string(raw), `:/etc/docker`)
}

func TestFixOneContainerdConfigPath_WritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"
	require.NoError(t, os.WriteFile(path, []byte(`
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d:/etc/docker/certs.d'
`), 0o644))
	r := fixOneContainerdConfigPath(path, "/etc/containerd/certs.d", false, false)
	assert.Equal(t, "added", r.Action)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `config_path = '/etc/containerd/certs.d'`)
	assert.Contains(t, string(raw), `io.containerd.transfer.v1.local`)
	assert.NotContains(t, string(raw), `:/etc/docker`)
	bak, err := os.ReadFile(path + ".bak.specula")
	require.NoError(t, err)
	assert.Contains(t, string(bak), `:/etc/docker`)
}
