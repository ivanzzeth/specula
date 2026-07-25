package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeRegistriesYAMLBytes_UpsertPreservesOthers(t *testing.T) {
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
		true,
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
	if _, ok := mirrors["other.example.test"]; !ok {
		t.Fatal("must preserve other.example.test")
	}
	spec := mirrors["specula.abcd1234.chorei.internal"].(map[string]any)
	eps := spec["endpoint"].([]any)
	if len(eps) != 1 || eps[0] != "http://10.0.0.1:7732" {
		t.Fatalf("specula mirror: %#v", eps)
	}
	configs := data["configs"].(map[string]any)
	if _, ok := configs["10.0.0.1:7732"]; !ok {
		t.Fatalf("want insecure config for dial host, got %#v", configs)
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

func TestWriteRegistryHostCerts_TempDir(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "http://10.0.0.1:7732"
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
	r2 := writeRegistryHostCerts(root, host, ep, true, false, false)
	if r2.Action != "already" {
		t.Fatalf("want already, got %s", r2.Action)
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
