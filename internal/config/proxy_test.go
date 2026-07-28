package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A proxy Specula cannot parse must fail at load. Accepting it and dialling direct
// instead would quietly undo the operator's intent, and an origin that is only
// reachable through the proxy would then fail as if the origin itself were down.
func TestUpstreamProxyIsValidated(t *testing.T) {
	good := []string{
		"", // absent is fine: env-based proxying still applies
		"http://10.0.0.5:3128",
		"https://proxy.internal:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"http://user:pass@10.0.0.5:3128",
	}
	for _, p := range good {
		if reason := upstreamProxyProblem(p); reason != "" {
			t.Errorf("proxy %q rejected: %s", p, reason)
		}
	}
	bad := map[string]string{
		"10.0.0.5:3128": "scheme", // bare host:port — ambiguous, must be explicit
		"ftp://p:21":    "scheme",
		"http://":       "host",
		"://nope":       "",
	}
	for p, want := range bad {
		reason := upstreamProxyProblem(p)
		if reason == "" {
			t.Errorf("proxy %q accepted", p)
			continue
		}
		if want != "" && !strings.Contains(reason, want) {
			t.Errorf("proxy %q: reason %q lacks %q", p, reason, want)
		}
	}
}

// End to end through Load: the field parses, survives validation, and a broken one
// names the exact key so an operator can find it.
func TestConfigLoadAcceptsPerUpstreamProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	body := `server:
  listen_addr: "127.0.0.1:7733"
storage:
  blob:
    driver: local
    local:
      root: ` + dir + `/blobs
  meta:
    driver: sqlite
    dsn: ` + dir + `/meta.db
protocols:
  oci:
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 10
        official: true
        proxy: http://10.0.0.5:3128
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ups := cfg.Protocols["oci"].Upstreams
	if len(ups) != 2 {
		t.Fatalf("got %d upstreams", len(ups))
	}
	// The mirror must NOT inherit the origin's proxy — that is the whole point.
	if ups[0].Proxy != "" {
		t.Errorf("mirror picked up a proxy: %q", ups[0].Proxy)
	}
	if ups[1].Proxy != "http://10.0.0.5:3128" {
		t.Errorf("origin proxy = %q", ups[1].Proxy)
	}
}

func TestConfigLoadRejectsABrokenProxyAndNamesTheKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	body := `server:
  listen_addr: "127.0.0.1:7733"
storage:
  blob:
    driver: local
    local:
      root: ` + dir + `/blobs
  meta:
    driver: sqlite
    dsn: ` + dir + `/meta.db
protocols:
  oci:
    upstreams:
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
        proxy: "ftp://nope:21"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a broken proxy loaded without complaint")
	}
	if !strings.Contains(err.Error(), "protocols.oci.upstreams[0].proxy") {
		t.Errorf("error does not name the key: %v", err)
	}
}
