package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// egress.official_proxy must stamp every official:true upstream without
// rewriting the built-in CN mirror chain (mirrors stay Proxy-empty / direct).
func TestOfficialEgressProxyStampsOfficialOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	proxy := "http://trust-proxy.trust-proxy.svc:21584"
	body := `
server:
  listen_addr: "127.0.0.1:9"
egress:
  official_proxy: ` + proxy + `
storage:
  blob:
    driver: local
    local:
      root: ` + filepath.Join(dir, "blobs") + `
  meta:
    driver: sqlite
    dsn: ` + filepath.Join(dir, "meta.db") + `
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	oci, ok := cfg.Protocols["oci"]
	if !ok {
		t.Fatal("oci protocol missing after defaults")
	}
	var sawOfficial, sawMirror bool
	for _, up := range oci.Upstreams {
		if up.Official {
			sawOfficial = true
			if up.Proxy != proxy {
				t.Errorf("official %q proxy=%q want %q", up.Name, up.Proxy, proxy)
			}
			continue
		}
		sawMirror = true
		if strings.TrimSpace(up.Proxy) != "" {
			t.Errorf("mirror %q unexpectedly has proxy %q", up.Name, up.Proxy)
		}
	}
	if !sawOfficial {
		t.Error("no official upstream in oci chain")
	}
	if !sawMirror {
		t.Error("CN mirrors vanished — official_proxy must not replace the chain")
	}
}

// An explicit per-upstream proxy wins over egress.official_proxy.
func TestOfficialEgressProxyDoesNotOverrideExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	pinned := "socks5://127.0.0.1:1080"
	body := `
server:
  listen_addr: "127.0.0.1:9"
egress:
  official_proxy: http://trust-proxy.trust-proxy.svc:21584
storage:
  blob:
    driver: local
    local:
      root: ` + filepath.Join(dir, "blobs") + `
  meta:
    driver: sqlite
    dsn: ` + filepath.Join(dir, "meta.db") + `
protocols:
  oci:
    upstreams:
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
        proxy: ` + pinned + `
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	up := cfg.Protocols["oci"].Upstreams[0]
	if up.Proxy != pinned {
		t.Fatalf("explicit proxy overwritten: got %q want %q", up.Proxy, pinned)
	}
}

func TestOfficialEgressProxyValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	body := `
server:
  listen_addr: "127.0.0.1:9"
egress:
  official_proxy: "10.0.0.5:3128"
storage:
  blob:
    driver: local
    local:
      root: ` + filepath.Join(dir, "blobs") + `
  meta:
    driver: sqlite
    dsn: ` + filepath.Join(dir, "meta.db") + `
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected Load to reject bare host:port official_proxy")
	}
	if !strings.Contains(err.Error(), "egress.official_proxy") {
		t.Fatalf("error should name egress.official_proxy, got: %v", err)
	}
}
