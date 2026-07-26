package tarball

import (
	"strings"
	"sync"
)

// HostAllowlist is the SSRF gate for /tarball/<host>/…. Seeded from config at
// mount time; helm (and any future rewriter) may Allow() hosts discovered when
// rewriting absolute chart URLs into /tarball/ paths — otherwise cross-host
// charts (longhorn → github.com releases) 403 after index rewrite.
type HostAllowlist struct {
	mu    sync.RWMutex
	hosts map[string]struct{}
}

// NewHostAllowlist seeds the set (including forge CDN siblings).
func NewHostAllowlist(seed []string) *HostAllowlist {
	a := &HostAllowlist{hosts: make(map[string]struct{})}
	for _, h := range expandTarballAllowedHosts(seed) {
		a.hosts[h] = struct{}{}
	}
	return a
}

// Allow adds host (and forge CDN siblings). Safe for concurrent use.
func (a *HostAllowlist) Allow(host string) {
	if a == nil {
		return
	}
	expanded := expandTarballAllowedHosts([]string{host})
	if len(expanded) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hosts == nil {
		a.hosts = make(map[string]struct{})
	}
	for _, h := range expanded {
		a.hosts[h] = struct{}{}
	}
}

// Allows reports whether host may be proxied. Empty set → deny all.
func (a *HostAllowlist) Allows(host string) bool {
	if a == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.hosts[host]
	return ok
}
