package integrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrateGoPrepends(t *testing.T) {
	home := t.TempDir()
	envFile := filepath.Join(home, ".config", "go", "env")
	if err := os.MkdirAll(filepath.Dir(envFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("GOPROXY=https://proxy.golang.org,direct\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GOENV", envFile)

	// Unit-test the merge helper directly (no dependency on `go` binary behavior).
	cur := "https://proxy.golang.org,direct"
	next := prependGoProxy(cur, "http://127.0.0.1:7732/go")
	want := "http://127.0.0.1:7732/go,https://proxy.golang.org,direct"
	if next != want {
		t.Fatalf("got %q want %q", next, want)
	}
	// Idempotent prepend.
	if got := prependGoProxy(want, "http://127.0.0.1:7732/go"); got != want {
		t.Fatalf("idempotent: got %q", got)
	}
	// Dry-run against file-backed GOENV (if `go` is available).
	r := integrateGo(home, "http://127.0.0.1:7732", true)
	if r.Action != "added" && r.Action != "already" {
		t.Fatalf("dry-run: %+v", r)
	}
}

func TestIntegrateNPMPreservesKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".npmrc")
	orig := "//registry.npmjs.org/:_authToken=SECRET\nregistry=https://registry.npmjs.org/\n@scope:registry=https://example.com/\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	r := integrateNPM(home, "http://127.0.0.1:7732", false)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "registry=http://127.0.0.1:7732/npm/") {
		t.Fatalf("missing new registry: %s", s)
	}
	if !strings.Contains(s, "_authToken=SECRET") {
		t.Fatalf("auth key lost: %s", s)
	}
	if !strings.Contains(s, "@scope:registry=https://example.com/") {
		t.Fatalf("scope registry lost: %s", s)
	}
	if !strings.Contains(s, npmBackupKey+"https://registry.npmjs.org/") {
		t.Fatalf("missing backup: %s", s)
	}
	// Idempotent.
	r2 := integrateNPM(home, "http://127.0.0.1:7732", false)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}

func TestIntegratePipSoleIndex(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "pip", "pip.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[global]\nindex-url = https://pypi.org/simple\ntimeout = 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := integratePip(home, "http://127.0.0.1:7732", false)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	cfg, err := readPipConf(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["global"]["index-url"] != "http://127.0.0.1:7732/pypi/simple" {
		t.Fatalf("index-url: %v", cfg["global"])
	}
	if _, ok := cfg["global"]["extra-index-url"]; ok {
		t.Fatalf("extra-index-url must not be set (dep-confusion): %v", cfg["global"])
	}
	if cfg["global"]["timeout"] != "30" {
		t.Fatalf("timeout lost: %v", cfg["global"])
	}
}

func TestAuditPipExtraIndexRisk(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "pip", "pip.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[global]\nindex-url = http://127.0.0.1:7732/pypi/simple\nextra-index-url = https://pypi.org/simple\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	risks := AuditClientRisks(home)
	found := false
	for _, r := range risks {
		if r.Protocol == "pypi" && r.Action == "risk" && strings.Contains(r.Detail, "extra-index-url") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected extra-index-url risk, got %+v", risks)
	}
}

func TestIntegrateDockerPrependsMirrors(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "docker", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := map[string]any{
		"registry-mirrors": []any{"https://mirror.example.com"},
		"log-driver":       "json-file",
	}
	b, _ := json.MarshalIndent(orig, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	r := integrateDocker(home, "http://127.0.0.1:7732", false, true) // skipRoot → user path
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	cfg, err := readDockerDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	mirrors := dockerMirrors(cfg)
	if len(mirrors) < 2 || mirrors[0] != "http://127.0.0.1:7732" {
		t.Fatalf("mirrors=%v", mirrors)
	}
	if mirrors[1] != "https://mirror.example.com" {
		t.Fatalf("existing mirror lost: %v", mirrors)
	}
	insecs := dockerInsecures(cfg)
	if !containsFold(insecs, "127.0.0.1:7732") {
		t.Fatalf("insecure-registries missing Specula host: %v", insecs)
	}
	if cfg["log-driver"] != "json-file" {
		t.Fatalf("other keys lost: %v", cfg)
	}
	// Idempotent.
	r2 := integrateDocker(home, "http://127.0.0.1:7732", false, true)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}

func TestDockerInsecureHost(t *testing.T) {
	if got := dockerInsecureHost("http://127.0.0.1:7732/"); got != "127.0.0.1:7732" {
		t.Fatalf("got %q", got)
	}
	if got := dockerInsecureHost("https://reg.example.com"); got != "" {
		t.Fatalf("https should skip insecure, got %q", got)
	}
}

func TestIntegrateContainerdCertsOverridePath(t *testing.T) {
	home := t.TempDir()
	r := integrateContainerdCerts(home, "http://127.0.0.1:7732", false, true)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	codeberg := filepath.Join(home, ".config", "specula", "certs.d", "codeberg.org", "hosts.toml")
	b, err := os.ReadFile(codeberg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "override_path = true") {
		t.Fatalf("missing override_path:\n%s", s)
	}
	if !strings.Contains(s, `host."http://127.0.0.1:7732/v2/codeberg.org"`) {
		t.Fatalf("missing path-style host key:\n%s", s)
	}
	docker := filepath.Join(home, ".config", "specula", "certs.d", "docker.io", "hosts.toml")
	db, err := os.ReadFile(docker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(db), "override_path") {
		t.Fatalf("docker.io must not use override_path:\n%s", db)
	}
	r2 := integrateContainerdCerts(home, "http://127.0.0.1:7732", false, true)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}

func TestIntegrateOCIDryRunIncludesContainerd(t *testing.T) {
	home := t.TempDir()
	rep, err := Run(Options{
		Addr:      "http://127.0.0.1:7732",
		Protocols: []string{"oci"},
		Home:      home,
		DryRun:    true,
		SkipRoot:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Protocol != "oci" {
		t.Fatalf("%+v", rep)
	}
	if !strings.Contains(rep.Results[0].Detail, "containerd") {
		t.Fatalf("detail missing containerd: %+v", rep.Results[0])
	}
}

func TestRunUnknownProtocol(t *testing.T) {
	rep, err := Run(Options{
		Addr:      "http://127.0.0.1:7732",
		Protocols: []string{"nope"},
		DryRun:    true,
		Home:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Action != "error" {
		t.Fatalf("%+v", rep)
	}
}

func TestHostOfAddr(t *testing.T) {
	if got := hostOfAddr("http://127.0.0.1:7732/"); got != "127.0.0.1" {
		t.Fatalf("got %q", got)
	}
	if got := hostOfAddr("https://specula.example:8443/go"); got != "specula.example" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteEnvFileIncludesNoProxy(t *testing.T) {
	home := t.TempDir()
	rep := Report{Addr: "http://127.0.0.1:7732", Results: []Result{{Protocol: "go", Action: "added"}}}
	if err := writeEnvFile(home, "http://127.0.0.1:7732", rep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(envPath(home))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "NO_PROXY=") || !strings.Contains(s, "127.0.0.1") {
		t.Fatalf("missing NO_PROXY in env.sh:\n%s", s)
	}
}

func TestIntegrateCargoSparseReplace(t *testing.T) {
	home := t.TempDir()
	r := integrateCargo(home, "http://127.0.0.1:7732", false)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(home, ".cargo", "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `registry = "sparse+http://127.0.0.1:7732/cargo/index/"`) {
		t.Fatalf("missing sparse registry:\n%s", s)
	}
	if !strings.Contains(s, `replace-with = "specula"`) {
		t.Fatalf("missing replace-with:\n%s", s)
	}
	r2 := integrateCargo(home, "http://127.0.0.1:7732", false)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}

func TestIntegrateCargoDryRun(t *testing.T) {
	home := t.TempDir()
	r := integrateCargo(home, "http://127.0.0.1:7732", true)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, ".cargo", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write config: %v", err)
	}
}

func TestIntegrateCondaChannel(t *testing.T) {
	home := t.TempDir()
	r := integrateConda(home, "http://127.0.0.1:7732", false, nil)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(home, ".condarc")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "http://127.0.0.1:7732/conda/conda-forge") {
		t.Fatalf("missing channel:\n%s", s)
	}
	r2 := integrateConda(home, "http://127.0.0.1:7732", false, nil)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}

func TestIntegrateHFEnvEndpoint(t *testing.T) {
	home := t.TempDir()
	rep, err := Run(Options{
		Addr:      "http://127.0.0.1:7732",
		Protocols: []string{"hf"},
		Home:      home,
		DryRun:    false,
		SkipRoot:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Protocol != "hf" || rep.Results[0].Action != "added" {
		t.Fatalf("%+v", rep)
	}
	b, err := os.ReadFile(envPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "HF_ENDPOINT=") || !strings.Contains(string(b), "http://127.0.0.1:7732/hf") {
		t.Fatalf("missing HF_ENDPOINT:\n%s", b)
	}
}

