package specula_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/embed"
	"github.com/ivanzzeth/specula/pkg/specula"
)

func TestNew_RequiresStore(t *testing.T) {
	_, err := specula.New(context.Background(), specula.Options{})
	require.Error(t, err)
}

func TestNew_DataDir_LookupMiss(t *testing.T) {
	dir := t.TempDir()
	s, err := specula.New(context.Background(), specula.Options{
		DataDir: dir,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Get(context.Background(), artifact.ArtifactRef{
		Protocol: "gomod",
		Name:     "example.com/foo",
		Version:  "v1.0.0.mod",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no upstreams")

	require.NotNil(t, s.CacheManager())
	require.NotNil(t, s.Chain())

	_, err = os.Stat(filepath.Join(dir, "quarantine"))
	require.NoError(t, err)
}

func TestEmbed_MountsEnabledProtocols(t *testing.T) {
	dir := t.TempDir()
	s, err := specula.New(context.Background(), specula.Options{DataDir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mux := http.NewServeMux()
	embed.Mount(mux, s, embed.Options{Protocols: []string{"gomod"}})
	require.NotNil(t, embed.Handler(s, embed.Options{Protocols: []string{"gomod"}}))
}

func TestVerifyFile_RunsChain(t *testing.T) {
	dir := t.TempDir()
	s, err := specula.New(context.Background(), specula.Options{DataDir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	path := filepath.Join(dir, "blob.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	ref := artifact.ArtifactRef{Protocol: "tarball", Name: "x", Version: "1"}
	art := &artifact.Artifact{
		Path:   path,
		Digest: "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		Size:   5,
	}
	res, err := s.VerifyFile(context.Background(), ref, art)
	require.NoError(t, err)
	require.NotEqual(t, artifact.StatusFail, res.Status)

	_ = cache.ErrCacheMiss
}
