package main

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	body, err := renderUnit("/opt/specula/bin/specula", "/opt/specula/specula.yaml", "cache")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "User=cache") || !strings.Contains(body, "Group=cache") {
		t.Fatalf("user not patched:\n%s", body)
	}
	if !strings.Contains(body, "ExecStart=/opt/specula/bin/specula --config /opt/specula/specula.yaml") {
		t.Fatalf("exec not patched:\n%s", body)
	}
	if !strings.Contains(body, "WantedBy=multi-user.target") {
		t.Fatal("missing WantedBy")
	}
}

func TestPatchConfigForSystemInstall(t *testing.T) {
	in := "root: ~/.specula/blobs\ndsn: ~/.specula/meta.db\nmirror_dir: ~/.specula/git\n"
	out := patchConfigForSystemInstall(in)
	if strings.Contains(out, "~/.specula") {
		t.Fatalf("still has ~/.specula: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/blobs") {
		t.Fatalf("missing blobs path: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/meta.db") {
		t.Fatalf("missing meta path: %s", out)
	}
	if !strings.Contains(out, "/var/lib/specula/git") {
		t.Fatalf("missing git path: %s", out)
	}
}
