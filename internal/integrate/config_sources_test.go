package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
)

func TestHelmReposFromConfig_Defaults(t *testing.T) {
	repos := helmReposFromConfig(nil)
	if len(repos) < 2 || repos[0].name != "specula-bitnami" {
		t.Fatalf("%+v", repos)
	}
	want := map[string]string{
		"specula-bitnami":              "/helm/bitnami",
		"specula-prometheus-community": "/helm/prometheus-community",
		"specula-longhorn":             "/helm/longhorn",
		"specula-jetstack":             "/helm/jetstack",
		"specula-argo":                 "/helm/argo",
	}
	got := map[string]string{}
	for _, r := range repos {
		got[r.name] = r.path
	}
	for name, path := range want {
		if got[name] != path {
			t.Fatalf("missing/wrong %s: got=%+v", name, repos)
		}
	}
}

func TestHelmReposFromConfig_Custom(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"helm": {
				Helm: &config.HelmConfig{
					Repositories: []config.NamedSource{
						{Name: "bitnami", BaseURL: "https://example/bitnami"},
						{Name: "custom", BaseURL: "https://example/custom"},
					},
				},
			},
		},
	}
	repos := helmReposFromConfig(cfg)
	if len(repos) != 2 {
		t.Fatalf("got %d repos", len(repos))
	}
	if repos[0].name != "specula-bitnami" || repos[0].path != "/helm/bitnami" {
		t.Fatalf("repo0: %+v", repos[0])
	}
	if repos[1].name != "specula-custom" || repos[1].path != "/helm/custom" {
		t.Fatalf("repo1: %+v", repos[1])
	}
}

func TestAptArchiveFromConfig(t *testing.T) {
	if got := aptArchiveFromConfig(nil); got != "ubuntu" {
		t.Fatalf("default: %q", got)
	}
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"apt": {
				Apt: &config.AptConfig{
					Repositories: []config.NamedSource{
						{Name: "debian", BaseURL: "https://deb.debian.org/debian"},
						{Name: "ubuntu", BaseURL: "https://archive.ubuntu.com/ubuntu"},
					},
				},
			},
		},
	}
	if got := aptArchiveFromConfig(cfg); got != "ubuntu" {
		t.Fatalf("prefer ubuntu: %q", got)
	}
	cfg.Protocols["apt"].Apt.Repositories = []config.NamedSource{{Name: "debian", BaseURL: "https://deb.debian.org/debian"}}
	if got := aptArchiveFromConfig(cfg); got != "debian" {
		t.Fatalf("first repo: %q", got)
	}
}

// ~/.condarc lists channels by NAME; the Specula location lives in
// custom_channels. A helper that builds per-channel mirror URLs is exactly the
// shape that leaks into environment.yml, so it no longer exists — see
// TestIntegrateCondaKeepsMirrorOutOfEnvironmentYML.
func TestCondaChannelNamesFromConfig(t *testing.T) {
	names := condaChannelNamesFromConfig(nil)
	if len(names) != 1 || names[0] != "conda-forge" {
		t.Fatalf("%v", names)
	}
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"conda": {
				Conda: &config.CondaConfig{
					Channels: []config.NamedSource{
						{Name: "conda-forge", BaseURL: "https://conda.anaconda.org/conda-forge"},
						{Name: "bioconda", BaseURL: "https://conda.anaconda.org/bioconda"},
					},
				},
			},
		},
	}
	names = condaChannelNamesFromConfig(cfg)
	if len(names) != 2 || names[0] != "conda-forge" || names[1] != "bioconda" {
		t.Fatalf("want the channel names, got %v", names)
	}
	// Names only — an entry carrying a host would end up in environment.yml.
	for _, n := range names {
		if strings.Contains(n, "://") {
			t.Fatalf("channel entry must be a bare name, got %q", n)
		}
	}
}

func TestIntegrateCondaMultiChannelFromConfig(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"conda": {
				Conda: &config.CondaConfig{
					Channels: []config.NamedSource{
						{Name: "conda-forge", BaseURL: "https://conda.anaconda.org/conda-forge"},
						{Name: "bioconda", BaseURL: "https://conda.anaconda.org/bioconda"},
					},
				},
			},
		},
	}
	r := integrateConda(home, "http://127.0.0.1:7732", false, cfg)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Every allowlisted channel must resolve to Specula — but by NAME, via
	// custom_channels. Listing the URLs under `channels:` instead would put this
	// machine's address into every environment.yml exported here, since
	// `conda env export` copies that block verbatim into a committed file.
	for _, name := range []string{"conda-forge", "bioconda"} {
		if !strings.Contains(s, "\n  - "+name+"\n") {
			t.Fatalf("channel %q must be listed by name:\n%s", name, s)
		}
		if !strings.Contains(s, "  "+name+": http://127.0.0.1:7732/conda\n") {
			t.Fatalf("channel %q must map to Specula via custom_channels:\n%s", name, s)
		}
	}
	if strings.Contains(s, "channels:\n  - http://") {
		t.Fatalf("mirror URL must not appear in the channels list:\n%s", s)
	}
}

func TestIntegrateHelmDryRunFromConfig(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"helm": {
				Helm: &config.HelmConfig{
					Repositories: []config.NamedSource{
						{Name: "bitnami", BaseURL: "https://charts.bitnami.com/bitnami"},
					},
				},
			},
		},
	}
	r := integrateHelm("http://127.0.0.1:7732", true, cfg)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Detail, "specula-bitnami") {
		t.Fatalf("detail: %s", r.Detail)
	}
}

func TestRunWithConfigPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	yaml := `
server:
  listen_addr: "0.0.0.0:7733"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 1800
auth:
  jwt_secret: ""
  cookie_secure: false
protocols:
  apt:
    mutable_ttl_seconds: 300
    upstreams:
      - name: debian
        base_url: https://deb.debian.org/debian
        priority: 1
        official: true
    verification:
      tiers: [signed, checksum]
      quorum: 1
    apt:
      repositories:
        - name: debian
          base_url: https://deb.debian.org/debian
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{
		Addr:       "http://127.0.0.1:7732",
		Protocols:  []string{"apt"},
		Home:       t.TempDir(),
		DryRun:     true,
		ConfigPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("%+v", rep)
	}
	if !strings.Contains(rep.Results[0].Detail, "archive=debian") {
		t.Fatalf("detail: %s", rep.Results[0].Detail)
	}
}
