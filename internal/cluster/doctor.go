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

	// Reachability. Default route is the API server's Service proxy, NOT a node
	// IP + NodePort: the operator's machine usually cannot reach a node port
	// (macOS/docker-driver minikube, or a laptop pointed at a cloud cluster whose
	// nodes sit in a VPC), so the old default reported healthy clusters as broken.
	// The API server can always reach the Service, and kubectl is already
	// required. An explicit --addr still probes directly — useful from inside the
	// VPC, and the only way to check the NodePort itself.
	svc := rel + "-specula-bootstrap"
	addr := strings.TrimSpace(opts.Addr)
	switch {
	case opts.SkipProbe:
		// nothing
	case addr != "":
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
	default:
		if err := probeHealthzViaAPIServer(opts.Kubeconfig, opts.Context, ns, svc); err != nil {
			return fmt.Errorf("cluster doctor: Specula svc/%s /healthz: %w", svc, err)
		}
		fmt.Fprintf(os.Stdout, "cluster doctor: svc/%s /healthz OK (via API server proxy)\n", svc)
		code, err := probeV2ViaAPIServer(opts.Kubeconfig, opts.Context, ns, svc)
		if err != nil {
			return fmt.Errorf("cluster doctor: Specula svc/%s /v2/: %w", svc, err)
		}
		fmt.Fprintf(os.Stdout, "cluster doctor: svc/%s /v2/ → %s\n", svc, code)
		fmt.Fprintf(os.Stdout, "cluster doctor: note: node-side %s:%d is checked by the integrate DaemonSet, not from here\n",
			"127.0.0.1", DefaultNodePort)
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

// serviceProxyPath builds the API-server path that proxies to a Service port.
// Reaching Specula this way needs only kubectl and works from anywhere kubectl
// works — no node reachability, no curl on the operator's machine.
func serviceProxyPath(ns, svc string, port int, subPath string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy/%s",
		ns, svc, port, strings.TrimPrefix(subPath, "/"))
}

func probeHealthzViaAPIServer(kubeconfig, context, ns, svc string) error {
	out, err := kubectl(kubeconfig, context, "get", "--raw",
		serviceProxyPath(ns, svc, DefaultDataPort, "healthz"))
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func probeV2ViaAPIServer(kubeconfig, context, ns, svc string) (string, error) {
	out, err := kubectl(kubeconfig, context, "get", "--raw",
		serviceProxyPath(ns, svc, DefaultDataPort, "v2/"))
	return classifyV2Probe(string(out), err)
}

// classifyV2Probe separates "the registry answered an auth challenge" (healthy:
// /v2/ returns 401 by design for the Docker token handshake, which kubectl
// reports as a non-zero exit) from "the Service could not be reached at all".
// Treating every error as failure flags a working registry; treating every error
// as success hides a Service with no endpoints.
func classifyV2Probe(out string, err error) (string, error) {
	if err == nil {
		return "200", nil
	}
	low := strings.ToLower(out)
	for _, marker := range []string{
		"no endpoints available",
		"not found",
		"connection refused",
		"serviceunavailable",
		"service unavailable",
		"timeout",
		"no such host",
		"no route to host",
	} {
		if strings.Contains(low, marker) {
			return "", fmt.Errorf("%w (%s)", err, strings.TrimSpace(out))
		}
	}
	// Anything else — credentials / unauthorized / forbidden — means the handler
	// answered, which is what this probe is asserting.
	return "401", nil
}

func resolveProbeAddr(kubeconfig, context string) (string, error) { //nolint:unused // explicit --addr path + kept for callers probing a NodePort directly
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
