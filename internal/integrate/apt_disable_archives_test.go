package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisableSourcesListLines_CommentsCloudMirror(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.list")
	in := "deb http://mirrors.cloud.aliyuncs.com/ubuntu noble main\n" +
		"deb https://download.docker.com/linux/ubuntu noble stable\n" +
		"# already commented\ndeb http://mirrors.cloud.aliyuncs.com/ubuntu noble-security main\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	n, note, err := disableSourcesListLines(path, defaultUbuntuArchiveMarkers)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("changed=%d want 2 note=%s", n, note)
	}
	out, _ := os.ReadFile(path)
	s := string(out)
	if !strings.Contains(s, "# disabled-by-specula: deb http://mirrors.cloud.aliyuncs.com/ubuntu noble main") {
		t.Fatalf("cloud mirror not commented:\n%s", s)
	}
	if !strings.Contains(s, "deb https://download.docker.com/linux/ubuntu noble stable") {
		t.Fatalf("docker line must stay active:\n%s", s)
	}
}

func TestDisableDeb822IfMatches_RenamesUbuntuSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ubuntu.sources")
	body := "Types: deb\nURIs: http://archive.ubuntu.com/ubuntu\nSuites: noble\nComponents: main\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	n, note, err := disableDeb822IfMatches(path, defaultUbuntuArchiveMarkers)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed=%d note=%s", n, note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected rename away from %s", path)
	}
	if _, err := os.Stat(path + disabledBySpeculaSuffix); err != nil {
		t.Fatal(err)
	}
}

func TestDisableDeb822IfMatches_LeavesUnrelated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker.sources")
	body := "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: noble\nComponents: stable\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	n, _, err := disableDeb822IfMatches(path, defaultUbuntuArchiveMarkers)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("docker sources must not match /ubuntu archive markers, changed=%d", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
