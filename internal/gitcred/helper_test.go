package gitcred

import (
	"strings"
	"testing"
)

func TestRewriteForUpstream_githubProxyPath(t *testing.T) {
	in := map[string]string{
		"protocol": "http",
		"host":     "127.0.0.1:7732",
		"path":     "git/github.com/ivanzzeth/chorei.git",
	}
	out, ok := RewriteForUpstream(in, nil)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if out["protocol"] != "https" || out["host"] != "github.com" {
		t.Fatalf("got %+v", out)
	}
	if out["path"] != "ivanzzeth/chorei.git" {
		t.Fatalf("path=%q", out["path"])
	}
	// Original Specula host must not leak as the fill target.
	if out["host"] == "127.0.0.1:7732" {
		t.Fatal("still pointing at Specula host")
	}
}

func TestRewriteForUpstream_rejectsNonProxyPath(t *testing.T) {
	in := map[string]string{
		"protocol": "https",
		"host":     "github.com",
		"path":     "ivanzzeth/chorei.git",
	}
	if _, ok := RewriteForUpstream(in, nil); ok {
		t.Fatal("must not rewrite a direct upstream request")
	}
}

func TestRewriteForUpstream_rejectsUnknownHost(t *testing.T) {
	in := map[string]string{
		"protocol": "http",
		"host":     "127.0.0.1:7732",
		"path":     "git/evil.example/owner/repo.git",
	}
	if _, ok := RewriteForUpstream(in, nil); ok {
		t.Fatal("unknown upstream host must not rewrite")
	}
}

func TestParseWriteAttrsRoundTrip(t *testing.T) {
	raw := "protocol=http\nhost=127.0.0.1:7732\npath=git/github.com/o/r.git\n\n"
	attrs, err := ParseAttrs(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteAttrs(&b, attrs); err != nil {
		t.Fatal(err)
	}
	again, err := ParseAttrs(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"protocol", "host", "path"} {
		if again[k] != attrs[k] {
			t.Fatalf("%s: %q != %q", k, again[k], attrs[k])
		}
	}
}

func TestIsSpeculaHelper(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"!/usr/local/bin/specula git-credential", true},
		{"!specula git-credential", true},
		{"!gh auth git-credential", false},
		{"store", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSpeculaHelper(tc.in); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.in, got, tc.want)
		}
	}
}
