package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ivanzzeth/specula/internal/cluster"
)

func runCluster(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, clusterUsage)
		return nil
	}
	switch args[0] {
	case "install":
		return runClusterInstall(args[1:])
	case "integrate":
		return runClusterIntegrate(args[1:])
	case "doctor":
		return runClusterDoctor(args[1:])
	case "uninstall":
		return runClusterUninstall(args[1:])
	default:
		return fmt.Errorf("unknown cluster subcommand %q\n%s", args[0], clusterUsage)
	}
}

const clusterUsage = `Usage:
  specula cluster install [flags]
  specula cluster integrate [flags]
  specula cluster doctor [flags]
  specula cluster uninstall [flags]

One-command CN-safe bootstrap: local helm chart (no oci://ghcr pulls) +
node integrate DaemonSet (hosts.toml + CRI config_path rewrite).

Examples:
  specula cluster install --cn --image specula:local --load-image --wait
  specula cluster doctor
  specula cluster integrate --wait
  specula cluster uninstall
`

func clusterFlags(fs *flag.FlagSet) (kubeconfig, context, ns, release, chartDir, image *string, cn, wait, load *bool) {
	kubeconfig = fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path (default: KUBECONFIG / ~/.kube/config)")
	context = fs.String("context", "", "kubectl/helm context")
	ns = fs.String("namespace", cluster.DefaultNamespace, "helm release namespace")
	release = fs.String("release", cluster.DefaultRelease, "helm release name")
	chartDir = fs.String("chart-dir", "", "path to deploy/helm/specula-bootstrap")
	image = fs.String("image", "specula:local", "container image repo:tag")
	cn = fs.Bool("cn", false, "apply values-cn.yaml + regionProfile=cn (Huawei SWR k8s chain)")
	wait = fs.Bool("wait", true, "wait for Deployment + integrate DaemonSet")
	load = fs.Bool("load-image", false, "load --image into the cluster (minikube/kind)")
	return
}

func parseImage(ref string) (repo, tag string) {
	repo, tag = ref, "latest"
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == ':' {
			// ignore digests / ports — simple split on last colon if no slash after
			if j := i + 1; j < len(ref) && !containsSlash(ref[j:]) {
				return ref[:i], ref[i+1:]
			}
			break
		}
	}
	return ref, "latest"
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}

func runClusterInstall(args []string) error {
	fs := flag.NewFlagSet("cluster install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig, context, ns, release, chartDir, image, cn, wait, load := clusterFlags(fs)
	ha := fs.Bool("ha", false, "note only: promote to HA chart after preload (not auto)")
	timeout := fs.Duration("timeout", 5*time.Minute, "helm/rollout wait timeout")
	pvc := fs.String("pvc", "", "use existing PersistentVolumeClaim (skips creating one)")
	hostPath := fs.String("host-path", "", "node-local hostPath for Specula data (no provisioner)")
	storageClass := fs.String("storage-class", "", "StorageClass for created PVC")
	pvcSize := fs.String("pvc-size", "20Gi", "size for created PVC")
	persist := fs.Bool("persist", true, "create a PVC when --pvc/--host-path unset (falls back if no StorageClass)")
	pin := fs.String("pin-node", "", "kubernetes.io/hostname to pin Specula (default: auto-pick Ready worker)")
	skipPin := fs.Bool("skip-pin-node", false, "do not set nodeSelector on Specula Deployment")
	valuesFiles := newStringList(fs, "values", "deployment profile (helm values file); repeatable. "+
		"Carries the whole hosted shape — postgres meta, S3 blob, ha, HPA — in one file")
	metaDriver := fs.String("meta-driver", "sqlite", "metadata store: sqlite (on the data volume) or postgres (external)")
	metaSecret := fs.String("meta-secret", "", "k8s Secret holding the postgres DSN (required for --meta-driver postgres)")
	metaDSNKey := fs.String("meta-dsn-key", "dsn", "key inside --meta-secret holding the DSN")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  specula cluster install --cn --image specula:local --load-image --wait
  specula cluster install --cn --pvc my-cache-pvc --wait

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, tag := parseImage(*image)
	_, err := cluster.Install(cluster.InstallOptions{
		Kubeconfig:    *kubeconfig,
		Context:       *context,
		Namespace:     *ns,
		Release:       *release,
		ChartDir:      *chartDir,
		ImageRepo:     repo,
		ImageTag:      tag,
		CN:            *cn,
		Wait:          *wait,
		WaitTimeout:   *timeout,
		LoadImage:     *load,
		HA:            *ha,
		Persist:       *persist,
		PersistSet:    true,
		ExistingClaim: *pvc,
		HostPath:      *hostPath,
		StorageClass:  *storageClass,
		PVCSize:       *pvcSize,
		PinHostname:   *pin,
		SkipPinNode:   *skipPin,
		ValuesFiles:   *valuesFiles,
		MetaDriver:    *metaDriver,
		MetaSecret:    *metaSecret,
		MetaDSNKey:    *metaDSNKey,
	})
	return err
}

func runClusterIntegrate(args []string) error {
	fs := flag.NewFlagSet("cluster integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig, context, ns, release, _, _, _, wait, _ := clusterFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return cluster.Integrate(cluster.InstallOptions{
		Kubeconfig: *kubeconfig,
		Context:    *context,
		Namespace:  *ns,
		Release:    *release,
		Wait:       *wait,
	})
}

func runClusterDoctor(args []string) error {
	fs := flag.NewFlagSet("cluster doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig, context, ns, release, _, _, _, _, _ := clusterFlags(fs)
	addr := fs.String("addr", "", "Specula base URL to probe (default: node IP NodePort)")
	skipProbe := fs.Bool("skip-probe", false, "skip /healthz + /v2/ probe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return cluster.Doctor(cluster.DoctorOptions{
		Kubeconfig: *kubeconfig,
		Context:    *context,
		Namespace:  *ns,
		Release:    *release,
		Addr:       *addr,
		SkipProbe:  *skipProbe,
	})
}

func runClusterUninstall(args []string) error {
	fs := flag.NewFlagSet("cluster uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kubeconfig, context, ns, release, _, _, _, _, _ := clusterFlags(fs)
	skipNodeCleanup := fs.Bool("skip-node-cleanup", false,
		"leave node certs.d hosts.toml in place (default: remove it, or nodes keep "+
			"pointing at a Specula that no longer exists and every redirected registry fails)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return cluster.Uninstall(cluster.InstallOptions{
		Kubeconfig:      *kubeconfig,
		Context:         *context,
		Namespace:       *ns,
		Release:         *release,
		SkipNodeCleanup: *skipNodeCleanup,
	})
}

// newStringList registers a repeatable string flag (--values a --values b).
func newStringList(fs *flag.FlagSet, name, usage string) *[]string {
	var vals []string
	fs.Func(name, usage, func(v string) error {
		vals = append(vals, v)
		return nil
	})
	return &vals
}
