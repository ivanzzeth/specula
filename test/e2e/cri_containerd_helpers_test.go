//go:build integration

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

// requireCRIHarness skips unless SPECULA_E2E_CRI is enabled (default: auto when
// containerd+crictl+passwordless sudo are present; set SPECULA_E2E_CRI=0 to force skip,
// SPECULA_E2E_CRI=1 to force run).
func requireCRIHarness(t *testing.T) {
	t.Helper()
	switch os.Getenv("SPECULA_E2E_CRI") {
	case "0", "false", "no":
		t.Skip("SPECULA_E2E_CRI=0")
	}
	force := os.Getenv("SPECULA_E2E_CRI") == "1" || os.Getenv("SPECULA_E2E_CRI") == "true"
	need := []string{"containerd", "crictl", "ctr"}
	for _, bin := range need {
		if _, err := exec.LookPath(bin); err != nil {
			if force {
				t.Fatalf("%s required for SPECULA_E2E_CRI=1: %v", bin, err)
			}
			t.Skipf("SKIP CRI harness: %s not found", bin)
		}
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		if force {
			t.Fatalf("passwordless sudo required for SPECULA_E2E_CRI=1: %v", err)
		}
		t.Skip("SKIP CRI harness: passwordless sudo not available")
	}
}

// criWorkDir returns a dedicated work directory that we can sudo-rm. Prefer
// this over t.TempDir() for containerd root/state (root-owned files).
func criWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "specula-cri-")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = exec.Command("sudo", "rm", "-rf", dir).Run()
	})
	return dir
}

type criContainerd struct {
	root     string
	sock     string
	certsDir string
	cfgPath  string
	pid      int
	logPath  string
}

func startEphemeralContainerd(t *testing.T, configPath, sock string) *criContainerd {
	t.Helper()
	root := filepath.Dir(configPath)
	c := &criContainerd{
		root:     root,
		sock:     sock,
		certsDir: filepath.Join(root, "certs.d"),
		cfgPath:  configPath,
		logPath:  filepath.Join(root, "containerd.log"),
	}

	logF, err := os.Create(c.logPath)
	require.NoError(t, err)

	cmd := exec.Command("sudo", "containerd", "--config", configPath)
	cmd.Stdout = logF
	cmd.Stderr = logF
	require.NoError(t, cmd.Start())
	c.pid = cmd.Process.Pid
	t.Cleanup(func() {
		_ = exec.Command("sudo", "kill", fmt.Sprintf("%d", c.pid)).Run()
		time.Sleep(300 * time.Millisecond)
		_ = exec.Command("sudo", "kill", "-9", fmt.Sprintf("%d", c.pid)).Run()
		_, _ = cmd.Process.Wait()
		_ = logF.Close()
		// containerd runs as root and leaves root-owned bolt DBs; t.TempDir
		// RemoveAll would otherwise FAIL the test with permission denied.
		_ = exec.Command("sudo", "rm", "-rf",
			filepath.Join(root, "root"),
			filepath.Join(root, "state"),
			sock,
		).Run()
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.sock); err == nil {
			time.Sleep(500 * time.Millisecond)
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	logBytes, _ := os.ReadFile(c.logPath)
	t.Fatalf("containerd sock not ready: %s\nlog:\n%s", c.sock, logBytes)
	return c
}

func writeContainerdConfig(t *testing.T, path, root, state, sock, configPath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// Always set transfer.v1.local config_path to the same hosts root as CRI.
	// Production defaults leave it '' which skips hosts.toml for transfer pulls.
	transferPath := configPath
	if strings.Contains(configPath, ":") {
		// Colon CRI footgun tests: leave transfer empty (production default).
		transferPath = ""
	}
	body := fmt.Sprintf(`version = 3
root = %q
state = %q
[grpc]
  address = %q
[plugins.'io.containerd.cri.v1.images']
  [plugins.'io.containerd.cri.v1.images'.registry]
    config_path = %q
[plugins.'io.containerd.transfer.v1.local']
  config_path = %q
[plugins.'io.containerd.cri.v1.runtime']
  [plugins.'io.containerd.cri.v1.runtime'.containerd]
    default_runtime_name = 'runc'
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
      runtime_type = 'io.containerd.runc.v2'
`, root, state, sock, configPath, transferPath)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func writeSpeculaHosts(t *testing.T, certsDir, speculaBase string, registries []string) {
	t.Helper()
	require.NoError(t, bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   certsDir,
		Endpoint:   speculaBase,
		Registries: registries,
	}))
}

func (c *criContainerd) crictl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	all := append([]string{
		"timeout", "45",
		"crictl",
		"--runtime-endpoint", "unix://" + c.sock,
		"--image-endpoint", "unix://" + c.sock,
	}, args...)
	cmd := exec.Command("sudo", all...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *criContainerd) crictlTimeout(t *testing.T, sec int, args ...string) (string, error) {
	t.Helper()
	all := append([]string{
		"timeout", fmt.Sprintf("%d", sec),
		"crictl",
		"--runtime-endpoint", "unix://" + c.sock,
		"--image-endpoint", "unix://" + c.sock,
	}, args...)
	cmd := exec.Command("sudo", all...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *criContainerd) ctr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	all := append([]string{"ctr", "--address", c.sock}, args...)
	cmd := exec.Command("sudo", all...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *criContainerd) logContains(sub string) bool {
	b, err := os.ReadFile(c.logPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), sub)
}

func (c *criContainerd) logTail(n int) string {
	b, err := os.ReadFile(c.logPath)
	if err != nil {
		return err.Error()
	}
	if n <= 0 || len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

func trimOut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 800 {
		return s[:800] + "…"
	}
	return s
}
