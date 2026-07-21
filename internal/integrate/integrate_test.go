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

func TestIntegratePipMovesExtra(t *testing.T) {
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
	if cfg["global"]["extra-index-url"] != "https://pypi.org/simple" {
		t.Fatalf("extra-index-url: %v", cfg["global"])
	}
	if cfg["global"]["timeout"] != "30" {
		t.Fatalf("timeout lost: %v", cfg["global"])
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

