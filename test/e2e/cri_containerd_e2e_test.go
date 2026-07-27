//go:build integration

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
)

// TestCRI_ColonConfigPath_BypassesSpecula reproduces containerd#12808:
// hosts.toml → Specula, but colon config_path makes CRI ignore it and dial
// public registry.k8s.io / *.pkg.dev. Hermetic origin behind Specula sees 0 hits.
func TestCRI_ColonConfigPath_BypassesSpecula(t *testing.T) {
	requireCRIHarness(t)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	img := newMinimalOCIImage(t, "pause", "3.9", 32*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	regs := k8sRemoteChain(t, "registry.k8s.io", nil, origin.URL())
	speculaHost := newRemoteSpeculaServer(t, s, nil, regs)
	speculaBase := "http://" + speculaHost

	root := criWorkDir(t)
	state := filepath.Join(root, "state")
	sock := filepath.Join(root, "containerd.sock")
	certs := filepath.Join(root, "certs.d")
	cfg := filepath.Join(root, "config.toml")
	require.NoError(t, os.MkdirAll(state, 0o755))
	require.NoError(t, os.MkdirAll(certs, 0o755))
	writeSpeculaHosts(t, certs, speculaBase, []string{"registry.k8s.io"})
	writeContainerdConfig(t, cfg, filepath.Join(root, "root"), state, sock,
		certs+":/etc/docker/certs.d")

	ctd := startEphemeralContainerd(t, cfg, sock)
	before := origin.Requests()

	out, err := ctd.crictlTimeout(t, 20, "pull", "registry.k8s.io/pause:3.9")
	t.Logf("colon crictl pull: err=%v out=%s", err, trimOut(out))
	time.Sleep(300 * time.Millisecond)

	assert.Equal(t, before, origin.Requests(),
		"CRI with colon config_path must NOT reach Specula→origin (hosts.toml ignored)")
	assert.True(t,
		ctd.logContains("pkg.dev") || err != nil,
		"expected public-origin attempt or pull error; log=%s", ctd.logTail(8<<10))
}

// TestCRI_SingleConfigPath_PullsViaSpecula: same hosts.toml, single-root
// config_path — CRI pulls through Specula to the hermetic origin.
func TestCRI_SingleConfigPath_PullsViaSpecula(t *testing.T) {
	requireCRIHarness(t)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	img := newMinimalOCIImage(t, "pause", "3.9", 32*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	regs := k8sRemoteChain(t, "registry.k8s.io", nil, origin.URL())
	speculaHost := newRemoteSpeculaServer(t, s, nil, regs)
	speculaBase := "http://" + speculaHost

	root := criWorkDir(t)
	state := filepath.Join(root, "state")
	sock := filepath.Join(root, "containerd.sock")
	certs := filepath.Join(root, "certs.d")
	cfg := filepath.Join(root, "config.toml")
	require.NoError(t, os.MkdirAll(state, 0o755))
	require.NoError(t, os.MkdirAll(certs, 0o755))
	writeSpeculaHosts(t, certs, speculaBase, []string{"registry.k8s.io"})
	writeContainerdConfig(t, cfg, filepath.Join(root, "root"), state, sock, certs)

	ctd := startEphemeralContainerd(t, cfg, sock)
	out, err := ctd.crictl(t, "pull", "registry.k8s.io/pause:3.9")
	require.NoError(t, err, "crictl pull via Specula: %s\nlog:\n%s", out, ctd.logTail(8<<10))
	assert.Greater(t, origin.Requests(), int64(0),
		"origin behind Specula must see traffic when config_path is single-root")
	assert.False(t, ctd.logContains("pkg.dev"),
		"must not fall through to public pkg.dev; log=%s", ctd.logTail(4<<10))
}

// TestCRI_CtrHostsDir_WorksEvenWithColonPath documents the operator-facing
// asymmetry: ctr --hosts-dir works while crictl with the same hosts.toml does not.
func TestCRI_CtrHostsDir_WorksEvenWithColonPath(t *testing.T) {
	requireCRIHarness(t)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	img := newMinimalOCIImage(t, "pause", "ctr", 16*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	regs := k8sRemoteChain(t, "registry.k8s.io", nil, origin.URL())
	speculaHost := newRemoteSpeculaServer(t, s, nil, regs)
	speculaBase := "http://" + speculaHost

	root := criWorkDir(t)
	state := filepath.Join(root, "state")
	sock := filepath.Join(root, "containerd.sock")
	certs := filepath.Join(root, "certs.d")
	cfg := filepath.Join(root, "config.toml")
	require.NoError(t, os.MkdirAll(state, 0o755))
	require.NoError(t, os.MkdirAll(certs, 0o755))
	writeSpeculaHosts(t, certs, speculaBase, []string{"registry.k8s.io"})
	writeContainerdConfig(t, cfg, filepath.Join(root, "root"), state, sock,
		certs+":/etc/docker/certs.d")

	ctd := startEphemeralContainerd(t, cfg, sock)
	out, err := ctd.ctr(t, "--namespace", "k8s.io", "images", "pull",
		"--hosts-dir", certs, "registry.k8s.io/pause:ctr")
	require.NoError(t, err, "ctr --hosts-dir must succeed even with colon CRI config_path: %s", out)
	assert.Greater(t, origin.Requests(), int64(0))
}

// TestCRI_LiveK8sPauseAndEtcd pulls real kubeadm images through Specula via CRI.
func TestCRI_LiveK8sPauseAndEtcd(t *testing.T) {
	requireCRIHarness(t)
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 for live registry.k8s.io CRI pulls")
	}

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := ocihandler.RemoteRegistriesFromSpecs([]ocihandler.RemoteRegistrySpec{{
		Host: "registry.k8s.io",
		Upstreams: []ocihandler.RemoteUpstreamSpec{
			{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io", Priority: 1},
			{Name: "1ms", BaseURL: "https://k8s.1ms.run", Priority: 2},
		},
	}})
	speculaHost := newRemoteSpeculaServer(t, s, nil, regs)
	speculaBase := "http://" + speculaHost

	root := criWorkDir(t)
	state := filepath.Join(root, "state")
	sock := filepath.Join(root, "containerd.sock")
	certs := filepath.Join(root, "certs.d")
	cfg := filepath.Join(root, "config.toml")
	require.NoError(t, os.MkdirAll(state, 0o755))
	require.NoError(t, os.MkdirAll(certs, 0o755))
	writeSpeculaHosts(t, certs, speculaBase, []string{"registry.k8s.io", "docker.io"})
	writeContainerdConfig(t, cfg, filepath.Join(root, "root"), state, sock, certs)

	ctd := startEphemeralContainerd(t, cfg, sock)
	for _, ref := range []string{
		"registry.k8s.io/pause:3.10.1",
		"registry.k8s.io/pause:3.9",
		"registry.k8s.io/etcd:3.5.24-0",
	} {
		t.Run(strings.ReplaceAll(ref, "/", "_"), func(t *testing.T) {
			out, err := ctd.crictl(t, "pull", ref)
			require.NoError(t, err, "crictl pull %s: %s\nlog:\n%s", ref, out, ctd.logTail(8<<10))
			assert.False(t, ctd.logContains("pkg.dev"),
				"%s must not dial pkg.dev", ref)
		})
	}
}

// TestCRI_HostsToml_NoServerFallback covers k8s/k3s registry drop-ins:
// every DefaultOCIRegistries hosts.toml must omit public server= fallback.
func TestCRI_HostsToml_NoServerFallback(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   dir,
		Endpoint:   "http://10.43.0.1:7733",
		Registries: bootstrap.DefaultOCIRegistries,
	}))
	for _, reg := range bootstrap.DefaultOCIRegistries {
		body, err := os.ReadFile(filepath.Join(dir, reg, "hosts.toml"))
		require.NoError(t, err, reg)
		assert.NotContains(t, string(body), "server =", reg)
		if reg != "docker.io" {
			assert.Contains(t, string(body), "override_path", reg)
		}
	}
}
