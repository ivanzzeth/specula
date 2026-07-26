package tarball

import "testing"

func TestHostAllowlist_SeedAndDynamicAllow(t *testing.T) {
	a := NewHostAllowlist([]string{"mirror.azure.cn"})
	if !a.Allows("mirror.azure.cn") {
		t.Fatal("seed host must be allowed")
	}
	if a.Allows("github.com") {
		t.Fatal("unrelated host must be denied before Allow")
	}

	a.Allow("github.com")
	if !a.Allows("github.com") {
		t.Fatal("Allow(github.com) must permit github.com")
	}
	// forge CDN siblings expanded for path hosts after redirects
	if !a.Allows("codeload.github.com") {
		t.Fatal("Allow(github.com) must expand codeload.github.com")
	}
	if a.Allows("evil.example") {
		t.Fatal("Allow must not open unrelated hosts")
	}
}

func TestHostAllowlist_NilSafe(t *testing.T) {
	var a *HostAllowlist
	a.Allow("github.com")
	if a.Allows("github.com") {
		t.Fatal("nil allowlist must deny")
	}
}
