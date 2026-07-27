package cluster

import (
	"strings"
	"testing"
)

// helm ranks --set ABOVE every -f regardless of order. That single fact broke a
// deployment profile twice on a real cluster:
//
//   - `--image` defaults to specula:local, so an unconditional
//     `--set image.repository=…` overrode the profile's ACR coordinate and the Pod
//     went ImagePullBackOff against docker.io;
//   - auto-pinning added `--set nodeSelector…hostname=<node>` to a STATELESS
//     multi-replica deployment, capping it at one node's capacity while HPA tried to
//     scale past it.
//
// Both were invisible to tests because the argument list was built inline in
// Install(), which needs a live cluster. Hence this file.

func joined(args []string) string { return strings.Join(args, " ") }

func baseInput() HelmArgsInput {
	return HelmArgsInput{
		Opts:     InstallOptions{ImageRepo: "specula", ImageTag: "local"},
		Release:  "boot",
		Chart:    "/chart",
		NS:       "specula-boot",
		Repo:     "specula",
		Tag:      "local",
		CertsDir: "/etc/containerd/certs.d",
	}
}

// Without a profile every default must still be applied — the per-cluster mirror
// path depends on it.
func TestBuildHelmArgsNoProfileAppliesDefaults(t *testing.T) {
	got := joined(BuildHelmArgs(baseInput()))
	for _, want := range []string{
		"image.repository=specula", "image.tag=local",
		"mirror.enabled=true", "integrate.enabled=true", "installer.enabled=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in: %s", want, got)
		}
	}
}

// THE regression: with a profile and no typed flags, the defaulted image must NOT
// be pushed as --set, or the profile's image is silently replaced.
func TestBuildHelmArgsProfileKeepsItsImage(t *testing.T) {
	in := baseInput()
	in.Opts.ValuesFiles = []string{"/p.yaml"}
	got := joined(BuildHelmArgs(in))
	if strings.Contains(got, "image.repository=") {
		t.Fatalf("profile image overridden by a defaulted flag: %s", got)
	}
	if !strings.Contains(got, "-f /p.yaml") {
		t.Fatalf("profile file not passed: %s", got)
	}
}

// A typed --image still wins: the operator asked for it explicitly.
func TestBuildHelmArgsTypedImageOverridesProfile(t *testing.T) {
	in := baseInput()
	in.Opts.ValuesFiles = []string{"/p.yaml"}
	in.Opts.ExplicitFlags = map[string]bool{"image": true}
	in.Repo, in.Tag = "reg/ns/specula", "v1"
	got := joined(BuildHelmArgs(in))
	if !strings.Contains(got, "image.repository=reg/ns/specula") ||
		!strings.Contains(got, "image.tag=v1") {
		t.Fatalf("typed --image must override the profile: %s", got)
	}
}

// Persistence flags are the same story: defaults must not clobber a stateless
// profile that declares persistence.enabled=false.
func TestBuildHelmArgsProfileKeepsItsPersistence(t *testing.T) {
	in := baseInput()
	in.Opts.ValuesFiles = []string{"/p.yaml"}
	in.Persist = PersistenceMode{Enabled: true, Size: "20Gi"}
	if got := joined(BuildHelmArgs(in)); strings.Contains(got, "persistence.enabled") {
		t.Fatalf("profile persistence overridden: %s", got)
	}

	in.Opts.ExplicitFlags = map[string]bool{"storage-class": true}
	if got := joined(BuildHelmArgs(in)); !strings.Contains(got, "persistence.enabled") {
		t.Fatalf("typed --storage-class must apply persistence args: %s", got)
	}
}

// Pinning a stateless multi-replica deployment to one node caps it at that node's
// capacity. The caller decides; BuildHelmArgs must only emit what it is handed.
func TestBuildHelmArgsPinOnlyWhenGiven(t *testing.T) {
	in := baseInput()
	if strings.Contains(joined(BuildHelmArgs(in)), "nodeSelector") {
		t.Fatal("no pin was supplied, so no nodeSelector may be set")
	}
	in.Pin = "node-a"
	got := joined(BuildHelmArgs(in))
	if !strings.Contains(got, `nodeSelector.kubernetes\.io/hostname=node-a`) {
		t.Fatalf("pin not applied: %s", got)
	}
}

// The DSN must never become a helm value — only the Secret reference.
func TestBuildHelmArgsPostgresPassesOnlyTheSecretReference(t *testing.T) {
	in := baseInput()
	in.Opts.MetaDriver = "postgres"
	in.Opts.MetaSecret = "specula-meta"
	in.Opts.MetaDSNKey = "dsn"
	got := joined(BuildHelmArgs(in))
	for _, want := range []string{"meta.driver=postgres", "meta.existingSecret=specula-meta", "meta.dsnKey=dsn"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "meta.dsn=") {
		t.Fatalf("a DSN must never be passed as a helm value: %s", got)
	}
}

// Blank entries in --values must not become bare `-f` with no path.
func TestBuildHelmArgsSkipsBlankValuesFiles(t *testing.T) {
	in := baseInput()
	in.Opts.ValuesFiles = []string{"", "   ", "/real.yaml"}
	got := BuildHelmArgs(in)
	fs := 0
	for i, a := range got {
		if a == "-f" {
			fs++
			if i+1 >= len(got) || strings.TrimSpace(got[i+1]) == "" {
				t.Fatalf("bare -f with no path: %v", got)
			}
		}
	}
	if fs != 1 {
		t.Fatalf("expected exactly one -f, got %d: %v", fs, got)
	}
}

// --cn adds regionProfile even under a profile: it is a deliberate flag, and the CN
// upstream chain is not something a profile is expected to restate.
func TestBuildHelmArgsCNAlwaysSetsRegionProfile(t *testing.T) {
	in := baseInput()
	in.Opts.CN = true
	in.Opts.ValuesFiles = []string{"/p.yaml"}
	if !strings.Contains(joined(BuildHelmArgs(in)), "regionProfile=cn") {
		t.Fatal("--cn must still set regionProfile")
	}
}
