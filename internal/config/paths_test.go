package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	got, err := config.ExpandPath("")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	got, err = config.ExpandPath("~")
	require.NoError(t, err)
	assert.Equal(t, home, got)

	got, err = config.ExpandPath("~/.specula/blobs")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".specula", "blobs"), got)

	got, err = config.ExpandPath("/abs/path")
	require.NoError(t, err)
	assert.Equal(t, "/abs/path", got)
}

func TestDefaultDataDir(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	got, err := config.DefaultDataDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".specula"), got)
}

func TestLoad_TildeStoragePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	path := writeYAML(t, `
server:
  data_plane_addr: "0.0.0.0:7732"
  control_plane_addr: "0.0.0.0:7733"
storage:
  blob:
    driver: local
    local:
      root: ~/.specula/blobs
  meta:
    driver: sqlite
    dsn: ~/.specula/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 1800
protocols:
  git:
    upstreams:
      - name: github
        base_url: https://github.com
        priority: 1
        official: true
    git:
      allowed_upstreams: [github.com]
      mirror_dir: ~/.specula/git
    verification:
      tiers: [tofu, checksum]
      quorum: 1
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".specula", "blobs"), cfg.Storage.Blob.Local.Root)
	assert.Equal(t, filepath.Join(home, ".specula", "meta.db"), cfg.Storage.Meta.DSN)
	require.NotNil(t, cfg.Protocols["git"].Git)
	assert.Equal(t, filepath.Join(home, ".specula", "git"), cfg.Protocols["git"].Git.MirrorDir)
}

func TestLoad_EmptyStorageDefaultsToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	path := writeYAML(t, `
server:
  data_plane_addr: "0.0.0.0:7732"
  control_plane_addr: "0.0.0.0:7733"
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 1800
protocols: {}
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "local", cfg.Storage.Blob.Driver)
	assert.Equal(t, filepath.Join(home, ".specula", "blobs"), cfg.Storage.Blob.Local.Root)
	assert.Equal(t, "sqlite", cfg.Storage.Meta.Driver)
	assert.Equal(t, filepath.Join(home, ".specula", "meta.db"), cfg.Storage.Meta.DSN)
}
