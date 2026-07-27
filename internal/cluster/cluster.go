// Package cluster implements `specula cluster install|integrate|doctor|uninstall`:
// one-command CN-safe bootstrap of Specula onto a Kubernetes cluster via the
// local helm chart (no remote OCI chart pulls) plus node-level integrate.
package cluster

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultRelease   = "boot"
	DefaultNamespace = "specula-boot"
	// DefaultNodePort is the single NodePort: protocols, Admin API, probes and the
	// WebUI all answer on it.
	DefaultNodePort = 30733
	// DefaultServicePort is the Service (not node) port — what the API-server Service
	// proxy addresses when doctor probes from outside the VPC.
	DefaultServicePort = 7733
)

// InstallOptions configures cluster install.
type InstallOptions struct {
	Kubeconfig  string
	Context     string
	Namespace   string
	Release     string
	ChartDir    string // path to deploy/helm/specula-bootstrap
	ImageRepo   string
	ImageTag    string
	CN          bool
	Wait        bool
	WaitTimeout time.Duration
	// LoadImage: when set, try to load ImageRepo:ImageTag into the cluster
	// (minikube image load / kind load). Empty = assume image already present.
	LoadImage bool
	// BinaryPath is used when building a local image (optional).
	BinaryPath string
	HA         bool // reserved: Phase 3 promote (not default)

	// ExplicitFlags names the CLI flags the operator actually typed. With a values
	// profile in play, only these may become helm --set: every flag has a default, so
	// "value is non-empty" cannot distinguish typed from defaulted, and an
	// unconditional --set silently overrides the profile (helm ranks --set above -f
	// regardless of order).
	ExplicitFlags map[string]bool

	// ValuesFiles are extra helm values files, applied in order AFTER the chart's
	// own values and after --cn's values-cn.yaml, so a deployment profile can carry
	// the whole hosted shape (postgres meta + S3 blob + ha + HPA + ingress) in one
	// file instead of a dozen flags. Explicit flags still win, because they are
	// translated to --set, which helm applies last.
	ValuesFiles []string

	// MetaDriver is "sqlite" (default) or "postgres". Postgres needs
	// MetaSecret — the DSN carries credentials and never goes in a ConfigMap.
	// Independent of HA: a single replica with external meta is supported, and is
	// what makes the Pod survive losing its node.
	MetaDriver string
	MetaSecret string
	MetaDSNKey string

	// SkipNodeCleanup leaves node hosts.toml in place on uninstall. Only useful
	// when another Specula still serves that endpoint on the same nodes.
	SkipNodeCleanup bool

	// Persistence: existingClaim > hostPath > created PVC when Persist is true.
	// Persist defaults to true when unset via PersistSet; see ResolvePersistence.
	Persist       bool
	PersistSet    bool // if false, default Persist=true then StorageClass probe may disable
	ExistingClaim string
	HostPath      string
	StorageClass  string
	PVCSize       string
	PinHostname   string // empty = auto-pick Ready worker
	SkipPinNode   bool
}

// Result is a short install outcome.
type Result struct {
	Namespace string
	Release   string
	Endpoint  string // node-local Specula URL
	Notes     []string
}

// ResolveChartDir finds the bootstrap chart directory.
func ResolveChartDir(explicit string) (string, error) {
	if explicit != "" {
		if st, err := os.Stat(filepath.Join(explicit, "Chart.yaml")); err == nil && !st.IsDir() {
			return explicit, nil
		}
		return "", fmt.Errorf("chart-dir %q missing Chart.yaml", explicit)
	}
	if env := strings.TrimSpace(os.Getenv("SPECULA_BOOTSTRAP_CHART")); env != "" {
		if st, err := os.Stat(filepath.Join(env, "Chart.yaml")); err == nil && !st.IsDir() {
			return env, nil
		}
		return "", fmt.Errorf("SPECULA_BOOTSTRAP_CHART=%q missing Chart.yaml", env)
	}
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	for _, start := range candidates {
		dir := start
		for i := 0; i < 8; i++ {
			p := filepath.Join(dir, "deploy", "helm", "specula-bootstrap")
			if st, err := os.Stat(filepath.Join(p, "Chart.yaml")); err == nil && !st.IsDir() {
				return p, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("cannot find deploy/helm/specula-bootstrap (pass --chart-dir or set SPECULA_BOOTSTRAP_CHART)")
}

// DetectRuntime returns "k3s", "minikube", or "kubernetes" based on context/nodes.
func DetectRuntime(kubeconfig, context string) (string, error) {
	ctx := strings.TrimSpace(context)
	if ctx == "" {
		if out, err := kubectl(kubeconfig, "", "config", "current-context"); err == nil {
			ctx = strings.TrimSpace(string(out))
		}
	}
	lower := strings.ToLower(ctx)
	if strings.Contains(lower, "minikube") {
		return "minikube", nil
	}
	if strings.HasPrefix(lower, "kind-") {
		return "k3s", nil // kind load path
	}
	if strings.Contains(lower, "k3s") || strings.Contains(lower, "k3d") {
		return "k3s", nil
	}
	// Minikube profiles are often arbitrary names (e.g. "specula-cluster").
	if _, err := exec.LookPath("minikube"); err == nil && ctx != "" {
		if err := exec.Command("minikube", "status", "-p", ctx).Run(); err == nil {
			return "minikube", nil
		}
	}
	if out, err := kubectl(kubeconfig, context, "get", "nodes",
		"-o", "jsonpath={.items[0].spec.providerID}"); err == nil {
		pid := strings.ToLower(strings.TrimSpace(string(out)))
		if strings.HasPrefix(pid, "minikube://") {
			return "minikube", nil
		}
		if strings.HasPrefix(pid, "k3s://") || strings.HasPrefix(pid, "k3d://") {
			return "k3s", nil
		}
	}
	nodes, err := kubectl(kubeconfig, context, "get", "nodes", "-o", "jsonpath={.items[*].status.nodeInfo.osImage}")
	if err == nil && strings.Contains(strings.ToLower(string(nodes)), "k3os") {
		return "k3s", nil
	}
	return "kubernetes", nil
}

// CertsDirForRuntime returns the primary containerd certs.d root.
func CertsDirForRuntime(runtime string) string {
	if runtime == "k3s" {
		return "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"
	}
	return "/etc/containerd/certs.d"
}

func kubectl(kubeconfig, context string, args ...string) ([]byte, error) {
	all := make([]string, 0, len(args)+4)
	if kubeconfig != "" {
		all = append(all, "--kubeconfig", kubeconfig)
	}
	if context != "" {
		all = append(all, "--context", context)
	}
	all = append(all, args...)
	cmd := exec.Command("kubectl", all...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func helm(kubeconfig, context string, args ...string) ([]byte, error) {
	all := make([]string, 0, len(args)+6)
	if kubeconfig != "" {
		all = append(all, "--kubeconfig", kubeconfig)
	}
	if context != "" {
		all = append(all, "--kube-context", context)
	}
	all = append(all, args...)
	cmd := exec.Command("helm", all...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("helm %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func needBins(names ...string) error {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%s not found on PATH", n)
		}
	}
	return nil
}

// kubectlApplyStdin applies a manifest passed on stdin. Used for the short-lived
// node-cleanup DaemonSet, which is not part of the helm chart on purpose: it has
// to outlive the release it is cleaning up after.
func kubectlApplyStdin(kubeconfig, context, manifest string) error {
	all := make([]string, 0, 8)
	if kubeconfig != "" {
		all = append(all, "--kubeconfig", kubeconfig)
	}
	if context != "" {
		all = append(all, "--context", context)
	}
	all = append(all, "apply", "-f", "-")
	cmd := exec.Command("kubectl", all...)
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		return fmt.Errorf("kubectl apply -f -: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
