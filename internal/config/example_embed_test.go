package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleYAMLMatchesRepoRoot(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", "specula.example.yaml"))
	require.NoError(t, err, "repo-root specula.example.yaml is the human-edited source")
	assert.Equal(t, string(root), string(config.ExampleYAML),
		"internal/config/example.yaml must stay identical to specula.example.yaml "+
			"(copy after editing the root file so //go:embed stays in sync)")
}

func TestWriteExampleIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")

	created, err := config.WriteExampleIfMissing(path, nil)
	require.NoError(t, err)
	assert.True(t, created)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, config.ExampleYAML, body)

	created, err = config.WriteExampleIfMissing(path, nil)
	require.NoError(t, err)
	assert.False(t, created, "second call must not overwrite")
}

func TestWriteExampleIfMissing_Transform(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "specula.yaml")
	created, err := config.WriteExampleIfMissing(path, func(s string) string {
		return s + "\n# patched\n"
	})
	require.NoError(t, err)
	assert.True(t, created)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "# patched")
}

func TestLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "specula.yaml")
	cfg, created, err := config.LoadOrCreate(path)
	require.NoError(t, err)
	assert.True(t, created)
	require.NotNil(t, cfg)
	assert.Equal(t, "0.0.0.0:7732", cfg.Server.DataPlaneAddr)

	cfg2, created2, err := config.LoadOrCreate(path)
	require.NoError(t, err)
	assert.False(t, created2)
	require.NotNil(t, cfg2)
}
