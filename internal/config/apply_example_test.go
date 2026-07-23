package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/ivanzzeth/specula/internal/config"
)

func TestApplyExample_FillsMissingMultiSourceBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	// Minimal legacy apt/git config — no nested allowlists.
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  data_plane_addr: "127.0.0.1:9"
protocols:
  apt:
    upstreams:
      - name: ubuntu-archive
        base_url: https://archive.ubuntu.com/ubuntu
        priority: 1
        official: true
    verification:
      tiers: [tofu]
      quorum: 1
      tofu: enforce
  git:
    upstreams:
      - name: github
        base_url: https://github.com
        priority: 1
        official: true
    git:
      allowed_upstreams: [github.com]
    verification:
      tiers: [tofu]
      quorum: 1
      tofu: enforce
`), 0o644))

	res, err := config.ApplyExample(path, config.ApplyExampleOptions{Backup: true})
	require.NoError(t, err)
	require.True(t, res.Wrote)
	require.NotEmpty(t, res.BackupPath)
	assert.FileExists(t, res.BackupPath)

	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Protocols["apt"].Apt)
	require.NotEmpty(t, cfg.Protocols["apt"].Apt.Repositories)
	hosts := cfg.Protocols["git"].Git.AllowedUpstreams
	assert.Contains(t, hosts, "github.com")
	assert.Contains(t, hosts, "codeberg.org", "string-list union should add new hosts from example")
}

func TestApplyExample_DryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	orig := []byte("server:\n  data_plane_addr: \"127.0.0.1:9\"\n")
	require.NoError(t, os.WriteFile(path, orig, 0o644))

	res, err := config.ApplyExample(path, config.ApplyExampleOptions{DryRun: true})
	require.NoError(t, err)
	assert.False(t, res.Wrote)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, orig, got)
	assert.NotEmpty(t, res.Added)
}

func TestApplyExample_OverwriteReplacesLeaf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  data_plane_addr: "127.0.0.1:9"
cache:
  default_mutable_ttl_seconds: 999
`), 0o644))

	_, err := config.ApplyExample(path, config.ApplyExampleOptions{Overwrite: true, NoBackup: true})
	require.NoError(t, err)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, int64(300), cfg.Cache.DefaultMutableTTLSeconds)
}

func TestDeepMerge_NamedRepos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  data_plane_addr: "127.0.0.1:9"
protocols:
  helm:
    upstreams:
      - name: bitnami
        base_url: https://charts.bitnami.com/bitnami
        priority: 1
        official: true
    helm:
      repositories:
        - name: bitnami
          base_url: https://custom.example/bitnami
    verification:
      tiers: [tofu]
      quorum: 1
      tofu: enforce
`), 0o644))

	_, err := config.ApplyExample(path, config.ApplyExampleOptions{NoBackup: true})
	require.NoError(t, err)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Protocols["helm"].Helm)
	repos := cfg.Protocols["helm"].Helm.Repositories
	require.GreaterOrEqual(t, len(repos), 2)
	assert.Equal(t, "https://custom.example/bitnami", repos[0].BaseURL,
		"operator base_url for bitnami must win")
	var foundProm bool
	for _, r := range repos {
		if r.Name == "prometheus-community" {
			foundProm = true
		}
	}
	assert.True(t, foundProm, "missing named repo from example should be added")
}

func TestApplyExample_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.yaml")
	res, err := config.ApplyExample(path, config.ApplyExampleOptions{})
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.True(t, res.Wrote)
	_, err = config.Load(path)
	require.NoError(t, err)
}

func TestFormatApplyExampleReport(t *testing.T) {
	s := config.FormatApplyExampleReport(&config.ApplyExampleResult{
		Path:    "specula.yaml",
		DryRun:  true,
		Added:   []string{"protocols.apt.apt"},
		Changed: []string{"protocols.git.git.allowed_upstreams"},
	})
	assert.Contains(t, s, "dry-run")
	assert.Contains(t, s, "+ protocols.apt.apt")
	assert.Contains(t, s, "~ protocols.git.git.allowed_upstreams")
}

func TestApplyExample_FillEmptyReplacesEmptySlice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  data_plane_addr: "127.0.0.1:9"
protocols:
  apt:
    upstreams:
      - name: ubuntu-archive
        base_url: https://archive.ubuntu.com/ubuntu
        priority: 1
        official: true
    apt:
      repositories: []
    verification:
      tiers: [tofu]
      quorum: 1
      tofu: enforce
`), 0o644))

	_, err := config.ApplyExample(path, config.ApplyExampleOptions{FillEmpty: true, NoBackup: true})
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &root))
	// Spot-check via Load
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Protocols["apt"].Apt)
	assert.NotEmpty(t, cfg.Protocols["apt"].Apt.Repositories)
	assert.True(t, strings.Contains(string(raw), "ubuntu") || len(cfg.Protocols["apt"].Apt.Repositories) > 0)
}
