package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DoctorOptions configures cluster-wide doctor.
type DoctorOptions struct {
	Kubeconfig string
	Context    string
	Namespace  string
	Release    string
	Addr       string // override probe addr (default node IP NodePort)
	SkipProbe  bool
}

// Doctor aggregates Specula reachability + integrate DaemonSet readiness.
// Node-local CRI RISKs on the operator laptop are ignored; check DS logs for those.
func Doctor(opts DoctorOptions) error {
	if err := needBins("kubectl"); err != nil {
		return err
	}
	ns := opts.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	rel := opts.Release
	if rel == "" {
		rel = DefaultRelease
	}

	deploy := rel + "-specula-bootstrap"
	out, err := kubectl(opts.Kubeconfig, opts.Context, "get", "deploy", deploy, "-n", ns,
		"-o", "jsonpath={.status.readyReplicas}/{.status.replicas}")
	if err != nil {
		return fmt.Errorf("cluster doctor: deployment %s: %w", deploy, err)
	}
	fmt.Fprintf(os.Stdout, "cluster doctor: deploy/%s ready=%s\n", deploy, strings.TrimSpace(string(out)))

	// Persistence volume (best-effort).
	claim := rel + "-specula-bootstrap-data"
	if pvcOut, perr := kubectl(opts.Kubeconfig, opts.Context, "get", "pvc", claim, "-n", ns,
		"-o", "jsonpath={.status.phase}"); perr == nil {
		phase := strings.TrimSpace(string(pvcOut))
		fmt.Fprintf(os.Stdout, "cluster doctor: pvc/%s phase=%s\n", claim, phase)
		if phase != "" && phase != "Bound" {
			return fmt.Errorf("cluster doctor: PVC %s not Bound (%s)", claim, phase)
		}
	} else {
		// existingClaim / hostPath / emptyDir — check pod volume briefly
		fmt.Fprintln(os.Stdout, "cluster doctor: chart PVC absent (existingClaim/hostPath/emptyDir OK)")
	}

	addr := strings.TrimSpace(opts.Addr)
	if addr == "" && !opts.SkipProbe {
		addr, err = resolveProbeAddr(opts.Kubeconfig, opts.Context)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cluster doctor: probe addr warn: %v\n", err)
		}
	}
	if addr != "" && !opts.SkipProbe {
		if err := probeHealthz(addr); err != nil {
			return fmt.Errorf("cluster doctor: Specula %s/healthz: %w", addr, err)
		}
		fmt.Fprintf(os.Stdout, "cluster doctor: %s/healthz OK\n", addr)
		code, err := probeV2(addr)
		if err != nil {
			return fmt.Errorf("cluster doctor: Specula %s/v2/: %w", addr, err)
		}
		if code != "200" && code != "401" {
			return fmt.Errorf("cluster doctor: unexpected /v2/ status %s", code)
		}
		fmt.Fprintf(os.Stdout, "cluster doctor: %s/v2/ → %s\n", addr, code)
	}

	ds := rel + "-specula-bootstrap-integrate"
	logs, err := kubectl(opts.Kubeconfig, opts.Context, "logs", "-n", ns, "ds/"+ds, "--tail=60")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster doctor: integrate DS logs warn: %v\n", err)
	} else {
		fmt.Fprintln(os.Stdout, "--- integrate DaemonSet logs (tail) ---")
		fmt.Fprint(os.Stdout, string(logs))
	}

	ready, err := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", ds, "-n", ns,
		"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
	if err != nil {
		return fmt.Errorf("cluster doctor: integrate ds: %w", err)
	}
	fmt.Fprintf(os.Stdout, "cluster doctor: ds/%s ready=%s\n", ds, strings.TrimSpace(string(ready)))
	parts := strings.Split(strings.TrimSpace(string(ready)), "/")
	if len(parts) != 2 || parts[0] != parts[1] || parts[1] == "0" {
		return fmt.Errorf("cluster doctor: integrate DaemonSet not fully ready (%s)", ready)
	}

	fmt.Fprintln(os.Stdout, "cluster doctor: OK")
	return nil
}

func resolveProbeAddr(kubeconfig, context string) (string, error) {
	runtime, _ := DetectRuntime(kubeconfig, context)
	if runtime == "minikube" {
		profile := minikubeProfile(context)
		cmd := exec.Command("minikube", "ip", "-p", profile)
		out, err := cmd.Output()
		if err == nil {
			ip := strings.TrimSpace(string(out))
			if ip != "" {
				return fmt.Sprintf("http://%s:%d", ip, DefaultNodePort), nil
			}
		}
	}
	out, err := kubectl(kubeconfig, context, "get", "nodes",
		"-o", "jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}")
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("no node InternalIP")
	}
	return fmt.Sprintf("http://%s:%d", ip, DefaultNodePort), nil
}

func probeHealthz(addr string) error {
	cmd := exec.Command("curl", "-sfS", "--connect-timeout", "5", "--max-time", "15",
		strings.TrimRight(addr, "/")+"/healthz")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func probeV2(addr string) (string, error) {
	cmd := exec.Command("curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
		"--connect-timeout", "5", "--max-time", "15",
		strings.TrimRight(addr, "/")+"/v2/")
	out, err := cmd.CombinedOutput()
	code := strings.TrimSpace(string(out))
	if err != nil && code == "" {
		return "", err
	}
	return code, nil
}
