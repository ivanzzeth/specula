package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// LoadImageIntoCluster loads repo:tag into every node for common local drivers.
func LoadImageIntoCluster(kubeconfig, context, runtime, repo, tag string) error {
	ref := repo + ":" + tag
	fmt.Fprintf(os.Stdout, "cluster: loading image %s (runtime=%s)\n", ref, runtime)

	// Ensure image exists locally.
	if err := exec.Command("docker", "image", "inspect", ref).Run(); err != nil {
		return fmt.Errorf("docker image %s not found locally (build/load it first): %w", ref, err)
	}

	switch runtime {
	case "minikube":
		profile := minikubeProfile(context)
		cmd := exec.Command("minikube", "image", "load", ref, "-p", profile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	case "k3s":
		// kind / k3d often expose `k3d image import` or `kind load`. Try kind first.
		if _, err := exec.LookPath("kind"); err == nil {
			if ctx := strings.TrimPrefix(context, "kind-"); ctx != context || strings.Contains(context, "kind") {
				name := context
				if strings.HasPrefix(context, "kind-") {
					name = strings.TrimPrefix(context, "kind-")
				}
				cmd := exec.Command("kind", "load", "docker-image", ref, "--name", name)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err == nil {
					return nil
				}
			}
		}
		if _, err := exec.LookPath("k3d"); err == nil {
			cmd := exec.Command("k3d", "image", "import", ref)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
		return fmt.Errorf("k3s/k3d/kind image load failed — import %s onto nodes manually (ctr -n k8s.io images import)", ref)
	default:
		// Generic: try kind, then warn.
		if _, err := exec.LookPath("kind"); err == nil {
			cmd := exec.Command("kind", "load", "docker-image", ref)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
		fmt.Fprintf(os.Stderr, "cluster: WARN cannot auto-load %s for runtime %q — ensure ImagePullPolicy=IfNotPresent and image is on every node\n", ref, runtime)
		return nil
	}
}

func minikubeProfile(context string) string {
	if context != "" {
		// Arbitrary profile names (specula-cluster) are valid -p values.
		if err := exec.Command("minikube", "status", "-p", context).Run(); err == nil {
			return context
		}
	}
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err == nil {
		ctx := strings.TrimSpace(string(out))
		if err := exec.Command("minikube", "status", "-p", ctx).Run(); err == nil {
			return ctx
		}
	}
	return "minikube"
}
