package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeRegistriesYAMLBytes_HTTPHasNoTLSBlock(t *testing.T) {
	// Plain http:// must never get configs.<dial>.tls — that made k3s emit
	// server=https://<ip> against an HTTP-only Specula.
	existing := []byte(`
mirrors:
  docker.io:
    endpoint:
    - https://example.invalid/docker
  other.example.test:
    endpoint:
    - http://192.0.2.1:5000
configs:
  example.invalid:
    tls:
      insecure_skip_verify: true
`)
	out, already, err := mergeRegistriesYAMLBytes(
		existing,
		"specula.abcd1234.chorei.internal",
		"http://10.0.0.1:7732",
		true, // legacy callers may pass true; http still must not get tls
	)
	if err != nil {
		t.Fatal(err)
	}
	if already {
		t.Fatal("expected not already on first upsert")
	}
	var data map[string]any
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	mirrors := data["mirrors"].(map[string]any)
	if _, ok := mirrors["docker.io"]; !ok {
		t.Fatal("must preserve unrelated docker.io mirror")
	}
	spec := mirrors["specula.abcd1234.chorei.internal"].(map[string]any)
	eps := spec["endpoint"].([]any)
	if len(eps) != 1 || eps[0] != "http://10.0.0.1:7732" {
		t.Fatalf("specula mirror: %#v", eps)
	}
	configs, _ := data["configs"].(map[string]any)
	if _, ok := configs["10.0.0.1:7732"]; ok {
		t.Fatalf("http dial host must NOT have a configs tls entry, got %#v", configs["10.0.0.1:7732"])
	}

	out2, already2, err := mergeRegistriesYAMLBytes(out, "specula.abcd1234.chorei.internal", "http://10.0.0.1:7732", true)
	if err != nil {
		t.Fatal(err)
	}
	if !already2 {
		t.Fatal("second upsert should be already")
	}
	_ = out2
}

func TestMergeRegistriesYAMLBytes_HTTPSGetsInsecureSkipVerify(t *testing.T) {
	out, _, err := mergeRegistriesYAMLBytes(
		nil,
		"specula.abcd1234.chorei.internal",
		"https://10.0.0.1:7732",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	configs := data["configs"].(map[string]any)
	cfg := configs["10.0.0.1:7732"].(map[string]any)
	tls := cfg["tls"].(map[string]any)
	if tls["insecure_skip_verify"] != true {
		t.Fatalf("want insecure_skip_verify for https self-signed, got %#v", cfg)
	}
}

func TestWriteRegistryHostCerts_HTTPSSkipVerify(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "https://10.0.0.1:7732"
	r := writeRegistryHostCerts(root, host, ep, true, false, false)
	if r.Action != "added" {
		t.Fatalf("action=%s err=%s detail=%s", r.Action, r.Err, r.Detail)
	}
	body, err := os.ReadFile(filepath.Join(root, host, "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, ep) || !strings.Contains(s, "skip_verify") {
		t.Fatalf("hosts.toml body:\n%s", s)
	}
}

func TestWriteRegistryHostCerts_HTTPNoSkipVerify(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "http://10.0.0.1:7732"
	r := writeRegistryHostCerts(root, host, ep, false, false, false)
	if r.Action != "added" {
		t.Fatalf("action=%s", r.Action)
	}
	body, err := os.ReadFile(filepath.Join(root, host, "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "skip_verify") {
		t.Fatalf("http hosts.toml must not set skip_verify:\n%s", body)
	}
}

func TestEndpointDialHost(t *testing.T) {
	if got := endpointDialHost("http://10.0.0.1:7732"); got != "10.0.0.1:7732" {
		t.Fatalf("got %q", got)
	}
	if got := endpointDialHost("https://registry.example.test"); got != "registry.example.test" {
		t.Fatalf("got %q", got)
	}
}

func TestIntegrateRegistryHost_EmptySkipped(t *testing.T) {
	r := integrateRegistryHost("", "http://10.0.0.1:7732", true, true)
	if r.Action != "skipped" {
		t.Fatalf("want skipped, got %#v", r)
	}
}
