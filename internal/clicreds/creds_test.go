package clicreds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveLoadClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	require.NoError(t, Save(Credentials{
		ControlPlane: "http://127.0.0.1:7733/",
		Token:        "spck_testkey",
	}))

	p, err := Path()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "specula", "credentials.json"), p)

	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7733", got.ControlPlane)
	assert.Equal(t, "spck_testkey", got.Token)

	require.NoError(t, Clear())
	got, err = Load()
	require.NoError(t, err)
	assert.Empty(t, got.Token)
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	require.NoError(t, Save(Credentials{
		ControlPlane: "http://file:7733",
		Token:        "spck_file",
	}))

	t.Setenv(EnvToken, "spck_env")
	t.Setenv(EnvAddr, "http://env:7733")

	got, err := Resolve("http://flag:7733", "spck_flag", "http://default:7733")
	require.NoError(t, err)
	assert.Equal(t, "http://flag:7733", got.ControlPlane)
	assert.Equal(t, "spck_flag", got.Token)

	got, err = Resolve("", "", "http://default:7733")
	require.NoError(t, err)
	assert.Equal(t, "http://env:7733", got.ControlPlane)
	assert.Equal(t, "spck_env", got.Token)

	t.Setenv(EnvToken, "")
	t.Setenv(EnvAddr, "")
	got, err = Resolve("", "", "http://default:7733")
	require.NoError(t, err)
	assert.Equal(t, "http://file:7733", got.ControlPlane)
	assert.Equal(t, "spck_file", got.Token)
}
