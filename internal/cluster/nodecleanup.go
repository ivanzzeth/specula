package cluster

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Node cleanup on uninstall.
//
// Install writes certs.d/<registry>/hosts.toml on every node pointing at the
// Specula NodePort. `helm uninstall` removes the workloads and leaves those files,
// so every redirected registry keeps resolving to a port with nothing behind it —
// and CN mode deliberately writes no public `server =` fallback, so the pulls fail
// rather than degrade. Observed on a real ACK cluster: registry.k8s.io/pause:3.10
// → ErrImagePull after the release was gone.
//
// So uninstall runs a short-lived privileged DaemonSet that executes
// `bootstrap-mirror remove` on each node, using the RELEASE'S OWN IMAGE: it is the
// one image those nodes are known to have, and inside CN it may be the only one
// they can get. That has to happen BEFORE helm uninstall, while the image
// reference is still discoverable from the live DaemonSet.

// NodeCleanupSpec parameterises the cleanup DaemonSet.
type NodeCleanupSpec struct {
	Name        string
	Namespace   string
	Image       string
	Endpoint    string
	CertsDir    string
	K3sCertsDir string // optional; omitted entirely when empty
}

// RenderNodeCleanupDaemonSet builds the cleanup DaemonSet manifest.
//
// privileged + hostPath because the files live on the host filesystem; tolerations
// with `operator: Exists` because a control-plane node runs containerd too and its
// hosts.toml was rewritten as well; `--hold` so the container stays up after the
// removal and DaemonSet readiness is a usable "it ran" signal (without it the
// process exits and restartPolicy Always churns the Pod).
func RenderNodeCleanupDaemonSet(s NodeCleanupSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: specula-node-cleanup
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: specula-node-cleanup
  template:
    metadata:
      labels:
        app.kubernetes.io/name: specula-node-cleanup
    spec:
      enableServiceLinks: false
      tolerations:
        - operator: Exists
      containers:
        - name: cleanup
          image: %q
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
            runAsUser: 0
            runAsNonRoot: false
          args:
            - bootstrap-mirror
            - remove
            - --endpoint=%s
            - --certs-dir=/host-certs.d
`, s.Name, s.Namespace, s.Image, s.Endpoint)
	if strings.TrimSpace(s.K3sCertsDir) != "" {
		b.WriteString("            - --k3s-certs-dir=/host-k3s-certs.d\n")
	}
	b.WriteString(`            - --hold
          volumeMounts:
            - name: certs-d
              mountPath: /host-certs.d
`)
	if strings.TrimSpace(s.K3sCertsDir) != "" {
		b.WriteString("            - name: k3s-certs-d\n              mountPath: /host-k3s-certs.d\n")
	}
	fmt.Fprintf(&b, `      volumes:
        - name: certs-d
          hostPath:
            path: %s
            type: DirectoryOrCreate
`, s.CertsDir)
	if strings.TrimSpace(s.K3sCertsDir) != "" {
		fmt.Fprintf(&b, `        - name: k3s-certs-d
          hostPath:
            path: %s
            type: DirectoryOrCreate
`, s.K3sCertsDir)
	}
	return b.String()
}

// parseDaemonSetImage takes the first non-empty line of a jsonpath image list.
func parseDaemonSetImage(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

// CleanupNodeMirrors runs the cleanup DaemonSet and waits for it to land on every
// node, then removes it. Best-effort by design: a cleanup failure must not stop an
// uninstall the operator asked for, so problems are reported and the caller
// continues.
func CleanupNodeMirrors(opts InstallOptions, ns, rel string) error {
	mirrorDS := rel + "-specula-bootstrap-mirror"
	out, err := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", mirrorDS, "-n", ns,
		"-o", `jsonpath={range .spec.template.spec.containers[*]}{.image}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("cannot read the mirror DaemonSet image (nothing to clean?): %w", err)
	}
	image := parseDaemonSetImage(string(out))
	if image == "" {
		return fmt.Errorf("mirror DaemonSet reported no image")
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", DefaultNodePort)
	if got, err := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", mirrorDS, "-n", ns,
		"-o", `jsonpath={.spec.template.spec.containers[0].args}`); err == nil {
		if ep := extractEndpointArg(string(got)); ep != "" {
			endpoint = ep
		}
	}

	name := rel + "-specula-node-cleanup"
	manifest := RenderNodeCleanupDaemonSet(NodeCleanupSpec{
		Name:        name,
		Namespace:   ns,
		Image:       image,
		Endpoint:    endpoint,
		CertsDir:    "/etc/containerd/certs.d",
		K3sCertsDir: "/var/lib/rancher/k3s/agent/etc/containerd/certs.d",
	})
	fmt.Fprintf(os.Stdout, "cluster uninstall: cleaning node hosts.toml for %s (ds/%s)\n", endpoint, name)
	if err := kubectlApplyStdin(opts.Kubeconfig, opts.Context, manifest); err != nil {
		return fmt.Errorf("apply cleanup DaemonSet: %w", err)
	}
	defer func() {
		_, _ = kubectl(opts.Kubeconfig, opts.Context, "delete", "ds", name, "-n", ns, "--ignore-not-found")
	}()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		ready, derr := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", name, "-n", ns,
			"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
		if derr == nil {
			parts := strings.Split(strings.TrimSpace(string(ready)), "/")
			if len(parts) == 2 && parts[0] != "" && parts[0] == parts[1] && parts[1] != "0" {
				fmt.Fprintf(os.Stdout, "cluster uninstall: node hosts.toml cleaned on %s node(s)\n", parts[0])
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("cleanup DaemonSet did not become ready on every node within 2m")
}

// extractEndpointArg pulls the value of --endpoint= out of a rendered args array
// so cleanup targets exactly the endpoint install wrote, not an assumed default.
func extractEndpointArg(args string) string {
	const key = "--endpoint="
	i := strings.Index(args, key)
	if i < 0 {
		return ""
	}
	rest := args[i+len(key):]
	if j := strings.IndexAny(rest, `",]`); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}
