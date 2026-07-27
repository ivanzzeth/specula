package cluster

import (
	"strings"
	"testing"
)

func TestHelmPersistenceArgs_Priority(t *testing.T) {
	t.Parallel()
	args := HelmPersistenceArgs(PersistenceMode{ExistingClaim: "my-pvc", HostPath: "/x", Enabled: true})
	m := setMap(args)
	if m["persistence.existingClaim"] != "my-pvc" {
		t.Fatalf("claim: %#v", m)
	}
	if m["persistence.hostPath"] != "" {
		t.Fatalf("hostPath should be cleared: %#v", m)
	}

	args = HelmPersistenceArgs(PersistenceMode{HostPath: "/var/lib/specula-bootstrap", Enabled: true})
	m = setMap(args)
	if m["persistence.hostPath"] != "/var/lib/specula-bootstrap" {
		t.Fatalf("hostPath: %#v", m)
	}

	args = HelmPersistenceArgs(PersistenceMode{Enabled: false})
	m = setMap(args)
	if m["persistence.enabled"] != "false" {
		t.Fatalf("disabled: %#v", m)
	}

	args = HelmPersistenceArgs(PersistenceMode{Enabled: true, StorageClass: "local-path", Size: "10Gi"})
	m = setMap(args)
	if m["persistence.enabled"] != "true" || m["persistence.size"] != "10Gi" || m["persistence.storageClass"] != "local-path" {
		t.Fatalf("created pvc: %#v", m)
	}
}

func TestPickPinHostname(t *testing.T) {
	t.Parallel()
	got := PickPinHostname([]string{
		"cp-1\tcp\tTrue",
		"w-1\tworker\tTrue",
		"w-2\tworker\tFalse",
	})
	if got != "w-1" {
		t.Fatalf("want worker w-1, got %q", got)
	}
	got = PickPinHostname([]string{"cp-1\tcp\tTrue"})
	if got != "cp-1" {
		t.Fatalf("fallback cp: %q", got)
	}
	if PickPinHostname(nil) != "" {
		t.Fatal("empty")
	}
}

func TestFormatNodePinLine(t *testing.T) {
	t.Parallel()
	line := FormatNodePinLine("n1", "control-plane,master", "True")
	if !strings.Contains(line, "\tcp\t") {
		t.Fatalf("got %q", line)
	}
}

func setMap(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--set" {
			continue
		}
		kv := strings.SplitN(args[i+1], "=", 2)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return m
}
