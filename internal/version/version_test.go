package version_test

import (
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/version"
)

func TestStringContainsVersion(t *testing.T) {
	s := version.String()
	if !strings.HasPrefix(s, "specula ") {
		t.Fatalf("got %q", s)
	}
	if !strings.Contains(s, version.Version) {
		t.Fatalf("%q missing Version %q", s, version.Version)
	}
}
