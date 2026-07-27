package embed

import "testing"

// enabled() and build() must agree: a name in the default list that build()
// cannot construct mounts nothing, and a protocol build() supports but the list
// omits is a handler the embedder can never reach without naming it. Both
// directions are the same defect the daemon had — a supported protocol dark
// because a list did not mention it.
func TestDefaultProtocolListMatchesWhatBuildSupports(t *testing.T) {
	defaults := enabled(nil)
	if len(defaults) == 0 {
		t.Fatal("default protocol list is empty")
	}

	// Every default must have a mount pattern that is not the generic fallback
	// only by accident, and must be one of build()'s cases. build() needs a real
	// server to construct handlers, so assert on the case list via patternFor
	// plus an explicit expectation.
	want := map[string]string{
		"oci": "/v2/", "gomod": "/gomod/", "pypi": "/pypi/", "npm": "/npm/",
		"apt": "/apt/", "helm": "/helm/", "cargo": "/cargo/", "conda": "/conda/",
		"hf": "/hf/", "tarball": "/tarball/", "git": "/git/",
	}
	if len(defaults) != len(want) {
		t.Fatalf("default list has %d protocols, expected %d: %v", len(defaults), len(want), defaults)
	}
	for _, p := range defaults {
		pattern, ok := want[p]
		if !ok {
			t.Errorf("protocol %q is enabled by default but not expected", p)
			continue
		}
		if got := patternFor("", p); got != pattern {
			t.Errorf("patternFor(%q) = %q, want %q", p, got, pattern)
		}
	}
}

// The three protocols this test file was added for: they existed as handlers and
// as pkg/handler façades, and pkg/embed mounted none of them.
func TestCargoCondaHFAreMountable(t *testing.T) {
	for _, p := range []string{"cargo", "conda", "hf"} {
		var found bool
		for _, d := range enabled(nil) {
			if d == p {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not enabled by default", p)
		}
		if got := patternFor("", p); got != "/"+p+"/" {
			t.Errorf("patternFor(%q) = %q", p, got)
		}
	}
}

func TestPathPrefixAppliesToTheNewProtocols(t *testing.T) {
	if got := patternFor("/mirror", "conda"); got != "/mirror/conda/" {
		t.Errorf("patternFor prefix = %q", got)
	}
}

// enabled() passes explicit names through, normalising the config spelling "go"
// to the handler name "gomod" — an embedder who copies a protocol name out of a
// Specula config should not get a silently unmounted handler.
func TestEnabledNormalisesGoAndPassesNamesThrough(t *testing.T) {
	got := enabled([]string{"go", " NPM ", "hf"})
	want := []string{"gomod", "npm", "hf"}
	if len(got) != len(want) {
		t.Fatalf("enabled = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("enabled[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
