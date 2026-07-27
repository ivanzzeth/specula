package main

import (
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Both flags are required: an import missing --as would have to guess the name
// clients pull, and guessing wrong seeds a cache nothing ever reads.
func TestCacheImportRequiresFromAndAs(t *testing.T) {
	err := runCacheImport([]string{"--from", "/tmp/x.tar"})
	if err == nil {
		t.Fatal("missing --as accepted")
	}
	if !strings.Contains(err.Error(), "--as") {
		t.Errorf("error should name the missing flag: %v", err)
	}
	if err := runCacheImport([]string{"--as", "redis:7"}); err == nil {
		t.Fatal("missing --from accepted")
	}
}

func TestCacheDispatchRejectsUnknownSubcommand(t *testing.T) {
	if err := runCache([]string{"nope"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := runCache(nil); err == nil {
		t.Fatal("empty subcommand accepted")
	}
	if err := runCache([]string{"--help"}); err != nil {
		t.Errorf("--help should not error: %v", err)
	}
}
