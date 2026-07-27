package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Install deploys the local bootstrap chart and waits for Deployment + integrate DS.
func Install(opts InstallOptions) (*Result, error) {
	// Validate inputs BEFORE touching the cluster. An invalid combination that only
	// helm rejects has already created a release, pulled images and possibly run the
	// mirror DaemonSet — side effects for a typo.
	if err := validateMeta(opts); err != nil {
		return nil, err
	}
	if err := needBins("kubectl", "helm"); err != nil {
		return nil, err
	}
	ns := opts.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	rel := opts.Release
	if rel == "" {
		rel = DefaultRelease
	}
	chart, err := ResolveChartDir(opts.ChartDir)
	if err != nil {
		return nil, err
	}
	repo := opts.ImageRepo
	if repo == "" {
		repo = "specula"
	}
	tag := opts.ImageTag
	if tag == "" {
		tag = "local"
	}
	waitTO := opts.WaitTimeout
	if waitTO <= 0 {
		waitTO = 5 * time.Minute
	}

	runtime, _ := DetectRuntime(opts.Kubeconfig, opts.Context)
	certsDir := CertsDirForRuntime(runtime)

	if opts.LoadImage {
		if err := LoadImageIntoCluster(opts.Kubeconfig, opts.Context, runtime, repo, tag); err != nil {
			return nil, fmt.Errorf("load image: %w", err)
		}
	}

	persist := resolvePersistence(opts)
	pin := strings.TrimSpace(opts.PinHostname)
	if pin == "" && !opts.SkipPinNode {
		if claim := strings.TrimSpace(opts.ExistingClaim); claim != "" {
			if n, err := PVCSelectedNode(opts.Kubeconfig, opts.Context, ns, claim); err == nil && n != "" {
				pin = n
				fmt.Fprintf(os.Stdout, "cluster install: pinning to PVC selected-node %s\n", pin)
			}
		}
		if pin == "" {
			// Capacity-aware: refuse before helm runs rather than pin to a node that
			// cannot fit the Pod (see AutoPinNode).
			n, perr := AutoPinNode(opts.Kubeconfig, opts.Context, DefaultRequestMi)
			if perr != nil {
				return nil, fmt.Errorf("pick pin node: %w (use --skip-pin-node to schedule anywhere, "+
					"or --pin-node <host> to override)", perr)
			}
			if n != "" {
				pin = n
				fmt.Fprintf(os.Stdout, "cluster install: pinning Specula to node %s\n", pin)
			}
		}
	}

	args := []string{
		"upgrade", "--install", rel, chart,
		"--namespace", ns, "--create-namespace",
		"--set", "image.repository=" + repo,
		"--set", "image.tag=" + tag,
		"--set", "image.pullPolicy=IfNotPresent",
		"--set", "mirror.enabled=true",
		"--set", "integrate.enabled=true",
		"--set", "integrate.restartContainerd=once",
		"--set", "mirror.certsDir=" + certsDir,
		"--set", fmt.Sprintf("mirror.endpoint=http://127.0.0.1:%d", DefaultNodePort),
		"--set", "installer.enabled=false",
	}
	args = append(args, HelmPersistenceArgs(persist)...)
	if md := strings.ToLower(strings.TrimSpace(opts.MetaDriver)); md == "postgres" {
		// DSN itself stays in the Secret; only the reference is passed to helm.
		args = append(args,
			"--set", "meta.driver=postgres",
			"--set", "meta.existingSecret="+opts.MetaSecret)
		if k := strings.TrimSpace(opts.MetaDSNKey); k != "" {
			args = append(args, "--set", "meta.dsnKey="+k)
		}
	}
	if pin != "" {
		args = append(args, "--set", "nodeSelector.kubernetes\\.io/hostname="+pin)
	}
	if opts.CN {
		cnValues := filepath.Join(chart, "values-cn.yaml")
		if _, err := os.Stat(cnValues); err == nil {
			args = append(args, "-f", cnValues)
		}
		args = append(args, "--set", "regionProfile=cn")
	}

	fmt.Fprintf(os.Stdout, "cluster install: helm upgrade --install %s (chart=%s ns=%s image=%s:%s cn=%v runtime=%s persist=%v pin=%s)\n",
		rel, chart, ns, repo, tag, opts.CN, runtime, persist.Enabled || persist.ExistingClaim != "" || persist.HostPath != "", pin)
	if out, err := helm(opts.Kubeconfig, opts.Context, args...); err != nil {
		return nil, err
	} else if len(out) > 0 {
		fmt.Fprint(os.Stdout, string(out))
	}

	res := &Result{
		Namespace: ns,
		Release:   rel,
		Endpoint:  fmt.Sprintf("http://127.0.0.1:%d", DefaultNodePort),
		Notes: []string{
			"nodes dial Specula at " + fmt.Sprintf("http://127.0.0.1:%d", DefaultNodePort) + " (NodePort)",
			"integrate DaemonSet: hosts.toml + CRI config_path; containerd reload=once (healthz + stamp)",
			"self-heal: Pod crash / same-node reboot OK; node loss does not migrate the cache volume",
		},
	}
	if persist.ExistingClaim != "" {
		res.Notes = append(res.Notes, "using existing PVC "+persist.ExistingClaim)
	} else if persist.HostPath != "" {
		res.Notes = append(res.Notes, "using hostPath "+persist.HostPath)
	} else if !persist.Enabled {
		res.Notes = append(res.Notes, "persistence disabled (emptyDir) — cache lost on Pod recreate")
	}
	if pin != "" {
		res.Notes = append(res.Notes, "Specula pinned to node "+pin)
	}
	if opts.HA {
		res.Notes = append(res.Notes, "--ha is reserved: preload Bitnami images then helm install deploy/helm/specula with values-cn.yaml")
	}

	if opts.Wait {
		deploy := rel + "-specula-bootstrap"
		fmt.Fprintf(os.Stdout, "cluster install: waiting for deploy/%s\n", deploy)
		// Fail-fast on a terminal Pod state (unpullable image, missing Secret)
		// instead of burning the whole timeout on a rollout that cannot finish.
		if err := waitRolloutOrFailFast(opts.Kubeconfig, opts.Context, ns,
			"app.kubernetes.io/component=bootstrap", "deploy/"+deploy, waitTO); err != nil {
			return res, fmt.Errorf("wait deployment: %w", err)
		}
		ds := rel + "-specula-bootstrap-integrate"
		fmt.Fprintf(os.Stdout, "cluster install: waiting for ds/%s\n", ds)
		deadline := time.Now().Add(waitTO)
		ready := false
		for time.Now().Before(deadline) {
			out, err := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", ds, "-n", ns,
				"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
			if err == nil {
				parts := strings.Split(strings.TrimSpace(string(out)), "/")
				if len(parts) == 2 && parts[0] != "" && parts[0] == parts[1] && parts[1] != "0" {
					ready = true
					break
				}
			}
			time.Sleep(2 * time.Second)
		}
		if !ready {
			return res, fmt.Errorf("integrate DaemonSet %s not ready within %s", ds, formatDuration(waitTO))
		}
	}

	fmt.Fprintln(os.Stdout, "cluster install: done")
	for _, n := range res.Notes {
		fmt.Fprintf(os.Stdout, "  note: %s\n", n)
	}
	return res, nil
}

func resolvePersistence(opts InstallOptions) PersistenceMode {
	p := PersistenceMode{
		ExistingClaim: strings.TrimSpace(opts.ExistingClaim),
		HostPath:      strings.TrimSpace(opts.HostPath),
		StorageClass:  strings.TrimSpace(opts.StorageClass),
		Size:          strings.TrimSpace(opts.PVCSize),
	}
	if p.ExistingClaim != "" || p.HostPath != "" {
		p.Enabled = true
		return p
	}
	enabled := true
	if opts.PersistSet {
		enabled = opts.Persist
	}
	if enabled {
		if !HasDefaultStorageClass(opts.Kubeconfig, opts.Context) && p.StorageClass == "" {
			// Name the classes that DO exist: on a managed cluster (ACK ships four
			// alicloud-disk-* with none default) the flag alone is not actionable.
			fmt.Fprintln(os.Stderr, NoDefaultStorageClassHint(ListStorageClasses(opts.Kubeconfig, opts.Context)))
			enabled = false
		}
	}
	p.Enabled = enabled
	return p
}

// HasDefaultStorageClass reports whether a default StorageClass exists.
func HasDefaultStorageClass(kubeconfig, context string) bool {
	out, err := kubectl(kubeconfig, context, "get", "storageclass",
		"-o", `jsonpath={range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		// Older annotation key
		out, err = kubectl(kubeconfig, context, "get", "storageclass",
			"-o", `jsonpath={range .items[?(@.metadata.annotations.storageclass\.beta\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}`)
		if err != nil {
			return false
		}
	}
	return strings.TrimSpace(string(out)) != ""
}

// AutoPinHostname lists Ready nodes and picks a worker (or CP fallback).
func AutoPinHostname(kubeconfig, context string) (string, error) {
	out, err := kubectl(kubeconfig, context, "get", "nodes",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.labels}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}`)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		lines = append(lines, FormatNodePinLine(parts[0], parts[1], parts[2]))
	}
	return PickPinHostname(lines), nil
}

// PVCSelectedNode reads WaitForFirstConsumer annotation when present.
func PVCSelectedNode(kubeconfig, context, ns, claim string) (string, error) {
	if ns == "" {
		ns = DefaultNamespace
	}
	out, err := kubectl(kubeconfig, context, "get", "pvc", claim, "-n", ns,
		"-o", `jsonpath={.metadata.annotations.volume\.kubernetes\.io/selected-node}`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Integrate forces a re-run of the integrate DaemonSet (delete pods).
func Integrate(opts InstallOptions) error {
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
	ds := rel + "-specula-bootstrap-integrate"
	fmt.Fprintf(os.Stdout, "cluster integrate: deleting pods of ds/%s in %s\n", ds, ns)
	_, err := kubectl(opts.Kubeconfig, opts.Context, "delete", "pod", "-n", ns,
		"-l", "app.kubernetes.io/component=integrate",
		"--wait=false")
	if err != nil {
		_, err = kubectl(opts.Kubeconfig, opts.Context, "rollout", "restart", "ds/"+ds, "-n", ns)
	}
	if err != nil {
		return err
	}
	if opts.Wait {
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			out, e := kubectl(opts.Kubeconfig, opts.Context, "get", "ds", ds, "-n", ns,
				"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
			if e == nil {
				parts := strings.Split(strings.TrimSpace(string(out)), "/")
				if len(parts) == 2 && parts[0] == parts[1] && parts[1] != "0" {
					fmt.Fprintln(os.Stdout, "cluster integrate: DaemonSet ready")
					return nil
				}
			}
			time.Sleep(2 * time.Second)
		}
		return fmt.Errorf("integrate DaemonSet not ready in time")
	}
	return nil
}

// Uninstall removes the bootstrap helm release.
func Uninstall(opts InstallOptions) error {
	if err := needBins("helm"); err != nil {
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
	// Undo the node-side hosts.toml BEFORE the release goes away: the cleanup
	// DaemonSet needs the release's image (the one image those nodes are known to
	// have), and leaving the drop-ins behind breaks every redirected registry
	// because CN mode keeps no public fallback.
	if !opts.SkipNodeCleanup {
		if err := CleanupNodeMirrors(opts, ns, rel); err != nil {
			// Best-effort: an operator who asked to uninstall gets the uninstall.
			fmt.Fprintf(os.Stderr, "cluster uninstall: node cleanup skipped: %v\n", err)
			fmt.Fprintf(os.Stderr, "cluster uninstall: nodes may still point at Specula — "+
				"run `specula bootstrap-mirror remove --endpoint http://127.0.0.1:%d` on each node\n",
				DefaultNodePort)
		}
	}
	fmt.Fprintf(os.Stdout, "cluster uninstall: helm uninstall %s -n %s\n", rel, ns)
	_, err := helm(opts.Kubeconfig, opts.Context, "uninstall", rel, "--namespace", ns, "--wait")
	return err
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "1s"
	}
	s := int(d.Seconds())
	if s%60 == 0 {
		return strconv.Itoa(s/60) + "m"
	}
	return strconv.Itoa(s) + "s"
}

// validateMeta rejects a bad metadata-store combination up front.
//
// postgres without a Secret is the one that matters: helm's `required` catches it,
// but only after `helm upgrade --install` has already been invoked against the
// cluster. The DSN carries credentials, so it is never passed as a helm value.
func validateMeta(opts InstallOptions) error {
	md := strings.ToLower(strings.TrimSpace(opts.MetaDriver))
	switch md {
	case "", "sqlite":
		return nil
	case "postgres":
		if strings.TrimSpace(opts.MetaSecret) == "" {
			return fmt.Errorf("meta driver postgres requires --meta-secret <k8s-secret> holding the DSN " +
				"(postgres://user:pass@host:5432/specula?sslmode=require)")
		}
		return nil
	default:
		return fmt.Errorf("meta driver %q: want sqlite or postgres", opts.MetaDriver)
	}
}
