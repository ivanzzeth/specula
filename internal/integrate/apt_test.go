package integrate

import "testing"

func TestDetectAptSuiteFallback(t *testing.T) {
	// On this runner we expect a real codename; just assert non-empty.
	s := detectAptSuite()
	if s == "" {
		t.Fatal("empty suite")
	}
}

func TestIntegrateAptDryRun(t *testing.T) {
	r := integrateApt("http://127.0.0.1:7732", true, false)
	if r.Action != "added" && r.Action != "already" {
		t.Fatalf("%+v", r)
	}
	if r.Action == "added" && !contains(r.Detail, "suite=") {
		t.Fatalf("expected suite in detail: %s", r.Detail)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
