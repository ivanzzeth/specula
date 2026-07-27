package cluster

import (
	"strings"
	"testing"
)

// Input validation must happen before anything touches the cluster. helm's
// `required` does catch a missing Secret, but only after `helm upgrade --install`
// has run — which creates a release, pulls images and can start the mirror
// DaemonSet. A typo must not have side effects on a live cluster.
func TestValidateMetaRejectsPostgresWithoutSecret(t *testing.T) {
	err := validateMeta(InstallOptions{MetaDriver: "postgres"})
	if err == nil {
		t.Fatal("postgres without --meta-secret must be rejected")
	}
	if !strings.Contains(err.Error(), "--meta-secret") {
		t.Fatalf("error must name the flag: %v", err)
	}
}

func TestValidateMetaRejectsUnknownDriver(t *testing.T) {
	err := validateMeta(InstallOptions{MetaDriver: "mysql"})
	if err == nil || !strings.Contains(err.Error(), "sqlite or postgres") {
		t.Fatalf("want a driver error naming the valid set, got %v", err)
	}
}

func TestValidateMetaAcceptsDefaults(t *testing.T) {
	for _, d := range []string{"", "sqlite", "SQLite", " postgres "} {
		opts := InstallOptions{MetaDriver: d}
		if strings.Contains(strings.ToLower(d), "postgres") {
			opts.MetaSecret = "specula-meta"
		}
		if err := validateMeta(opts); err != nil {
			t.Fatalf("driver %q rejected: %v", d, err)
		}
	}
}

// A typo in --values must fail before any cluster call, not after helm has created
// a release. Ordering matters: the pin-node lookup used to run first and masked it.
func TestValidateInputsRejectsMissingValuesFile(t *testing.T) {
	err := validateInputs(InstallOptions{ValuesFiles: []string{"/definitely/not/here.yaml"}})
	if err == nil || !strings.Contains(err.Error(), "values file") {
		t.Fatalf("want a values-file error, got %v", err)
	}
}

func TestValidateInputsSkipsEmptyEntries(t *testing.T) {
	if err := validateInputs(InstallOptions{ValuesFiles: []string{"", "   "}}); err != nil {
		t.Fatalf("blank entries must be ignored: %v", err)
	}
}
