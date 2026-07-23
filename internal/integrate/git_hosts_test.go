package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestIntegrateGitMultiHostDryRun(t *testing.T) {
	home := t.TempDir()
	r := integrateGit(home, "http://127.0.0.1:7732", true)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	for _, host := range []string{"github.com", "codeberg.org", "git.sr.ht"} {
		if !strings.Contains(r.Detail, host) {
			t.Fatalf("dry-run detail missing %s: %s", host, r.Detail)
		}
	}
}

func TestIntegrateGitWritesInsteadOf(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	// Isolate git config from the real user.
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", home+"/.gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	r := integrateGit(home, "http://127.0.0.1:7732", false)
	if r.Action != "added" && r.Action != "already" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(home + "/.gitconfig")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, host := range DefaultGitHosts {
		needle := fmt.Sprintf("url.http://127.0.0.1:7732/git/%s/.insteadof", host)
		// git config writes [url "http://..."] / insteadOf = https://host/
		if !strings.Contains(s, "/git/"+host+"/") || !strings.Contains(s, "https://"+host+"/") {
			t.Fatalf("missing insteadOf for %s in:\n%s\n(want path %s)", host, s, needle)
		}
	}
	r2 := integrateGit(home, "http://127.0.0.1:7732", false)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
}
