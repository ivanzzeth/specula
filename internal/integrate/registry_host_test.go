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
		"/etc/specula/ca.crt", // ignored for http://
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

	out2, already2, err := mergeRegistriesYAMLBytes(out, "specula.abcd1234.chorei.internal", "http://10.0.0.1:7732", "")
	if err != nil {
		t.Fatal(err)
	}
	if !already2 {
		t.Fatal("second upsert should be already")
	}
	_ = out2
}

func TestMergeRegistriesYAMLBytes_HTTPSGetsInsecureSkipVerify(t *testing.T) {
	const host = "specula.abcd1234.chorei.internal"
	out, _, err := mergeRegistriesYAMLBytes(
		nil,
		host,
		"https://10.0.0.1:7732",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	configs := data["configs"].(map[string]any)
	for _, key := range []string{host, "10.0.0.1:7732"} {
		cfg := configs[key].(map[string]any)
		tls := cfg["tls"].(map[string]any)
		if tls["insecure_skip_verify"] != true {
			t.Fatalf("%s: want insecure_skip_verify, got %#v", key, cfg)
		}
		if _, ok := tls["ca_file"]; ok {
			t.Fatalf("%s: must not set ca_file without --ca-file, got %#v", key, cfg)
		}
	}
}

func TestMergeRegistriesYAMLBytes_HTTPSWithCAFile(t *testing.T) {
	const (
		host   = "specula.abcd1234.chorei.internal"
		caPath = "/etc/specula/ca.crt"
	)
	out, _, err := mergeRegistriesYAMLBytes(
		nil,
		host,
		"https://10.0.0.1:7732",
		caPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	configs := data["configs"].(map[string]any)
	for _, key := range []string{host, "10.0.0.1:7732"} {
		cfg := configs[key].(map[string]any)
		tls := cfg["tls"].(map[string]any)
		if tls["ca_file"] != caPath {
			t.Fatalf("%s: want ca_file=%q, got %#v", key, caPath, cfg)
		}
		if _, ok := tls["insecure_skip_verify"]; ok {
			t.Fatalf("%s: must not set insecure_skip_verify when ca_file is set, got %#v", key, cfg)
		}
	}
}

func TestMergeRegistriesYAMLBytes_HTTPSWithCAFilePreservesHostnameAuth(t *testing.T) {
	const host = "specula.abcd1234.chorei.internal"
	existing := []byte(`
configs:
  specula.abcd1234.chorei.internal:
    auth:
      username: specula
      password: secret
`)
	out, _, err := mergeRegistriesYAMLBytes(
		existing,
		host,
		"https://10.0.0.1:7732",
		"/etc/specula/ca.crt",
	)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	cfg := data["configs"].(map[string]any)[host].(map[string]any)
	auth := cfg["auth"].(map[string]any)
	if auth["username"] != "specula" || auth["password"] != "secret" {
		t.Fatalf("auth not preserved: %#v", cfg)
	}
	tls := cfg["tls"].(map[string]any)
	if tls["ca_file"] != "/etc/specula/ca.crt" {
		t.Fatalf("want ca_file on hostname config, got %#v", cfg)
	}
}

func TestWriteRegistryHostCerts_HTTPSSkipVerify(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "https://10.0.0.1:7732"
	r := writeRegistryHostCerts(root, host, ep, true, "", false, false)
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
	if strings.Contains(s, "ca =") {
		t.Fatalf("must not set ca without --ca-file:\n%s", s)
	}
}

func TestWriteRegistryHostCerts_HTTPSWithCAFile(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "https://10.0.0.1:7732"
	const caPath = "/etc/specula/ca.crt"
	r := writeRegistryHostCerts(root, host, ep, false, caPath, false, false)
	if r.Action != "added" {
		t.Fatalf("action=%s err=%s detail=%s", r.Action, r.Err, r.Detail)
	}
	body, err := os.ReadFile(filepath.Join(root, host, "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `ca = ["/etc/specula/ca.crt"]`) {
		t.Fatalf("want ca line, got:\n%s", s)
	}
	if strings.Contains(s, "skip_verify") {
		t.Fatalf("must not set skip_verify when ca is set:\n%s", s)
	}
}

func TestWriteRegistryHostCerts_HTTPNoSkipVerify(t *testing.T) {
	root := t.TempDir()
	host := "specula.abcd1234.chorei.internal"
	ep := "http://10.0.0.1:7732"
	r := writeRegistryHostCerts(root, host, ep, false, "", false, false)
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
	if strings.Contains(string(body), "ca =") {
		t.Fatalf("http hosts.toml must not set ca:\n%s", body)
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
	r := integrateRegistryHost("", "http://10.0.0.1:7732", "", true, true)
	if r.Action != "skipped" {
		t.Fatalf("want skipped, got %#v", r)
	}
}

func TestTLSTrustForEndpoint(t *testing.T) {
	skip, ca := tlsTrustForEndpoint("http://10.0.0.1:7732", "/etc/specula/ca.crt")
	if skip || ca != "" {
		t.Fatalf("http: skip=%v ca=%q", skip, ca)
	}
	skip, ca = tlsTrustForEndpoint("https://10.0.0.1:7732", "")
	if !skip || ca != "" {
		t.Fatalf("https no ca: skip=%v ca=%q", skip, ca)
	}
	skip, ca = tlsTrustForEndpoint("https://10.0.0.1:7732", "/etc/specula/ca.crt")
	if skip || ca != "/etc/specula/ca.crt" {
		t.Fatalf("https with ca: skip=%v ca=%q", skip, ca)
	}
}
