// Package git — public-repo visibility probe (ported from ai-sandbox gitproxy/public.go).
//
// publicChecker probes the upstream hosting service to determine whether a
// repository is anonymously readable. Results are cached for a configurable TTL.
// Probing is supported for github.com and gitlab.com; all other hosts return
// (false, error) which triggers fail-closed behaviour in the handler.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultPublicProbeTTL = 15 * time.Minute

type publicEntry struct {
	public    bool
	expiresAt time.Time
}

// publicChecker caches visibility probes to avoid redundant API calls.
type publicChecker struct {
	ttl    time.Duration
	client *http.Client

	mu    sync.Mutex
	cache map[string]publicEntry
}

// newPublicChecker constructs a publicChecker that uses transport for HTTP
// requests. Pass nil to use the default transport.
func newPublicChecker(ttl time.Duration, transport http.RoundTripper) *publicChecker {
	if ttl <= 0 {
		ttl = defaultPublicProbeTTL
	}
	c := &http.Client{Timeout: 15 * time.Second}
	if transport != nil {
		c.Transport = transport
	}
	return &publicChecker{
		ttl:    ttl,
		client: c,
		cache:  map[string]publicEntry{},
	}
}

// IsPublic returns true when upstream confirms the repo is anonymously readable.
// Results are cached for the probe TTL. Returns (false, error) on probe failure
// (caller should apply failClosed logic).
func (c *publicChecker) IsPublic(ctx context.Context, ref repoRef) (bool, error) {
	key := ref.Host + "\x00" + ref.ProjectPath
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Now().Before(e.expiresAt) {
		c.mu.Unlock()
		return e.public, nil
	}
	c.mu.Unlock()

	pub, err := c.probe(ctx, ref)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	c.cache[key] = publicEntry{public: pub, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return pub, nil
}

// probe dispatches to the host-specific API.
func (c *publicChecker) probe(ctx context.Context, ref repoRef) (bool, error) {
	switch ref.Host {
	case "github.com":
		return c.probeGitHub(ctx, ref)
	case "gitlab.com":
		return c.probeGitLab(ctx, ref)
	case "gitee.com":
		return c.probeGitee(ctx, ref)
	default:
		return false, fmt.Errorf("git: public visibility probe not supported for host %q", ref.Host)
	}
}

func (c *publicChecker) probeGitHub(ctx context.Context, ref repoRef) (bool, error) {
	parts := strings.SplitN(ref.ProjectPath, "/", 2)
	if len(parts) != 2 {
		return false, nil
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s",
		url.PathEscape(parts[0]), url.PathEscape(parts[1]))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "specula-git-handler/v0.2")

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("github api: %s", resp.Status)
	}
	var body struct {
		Private *bool `json:"private"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	if body.Private == nil {
		return false, nil
	}
	return !*body.Private, nil
}

func (c *publicChecker) probeGitLab(ctx context.Context, ref repoRef) (bool, error) {
	// GitLab API uses URL-encoded project path (namespace%2Frepo).
	enc := strings.ReplaceAll(url.PathEscape(ref.ProjectPath), "%2F", "%2F") // already escaped
	u := fmt.Sprintf("https://gitlab.com/api/v4/projects/%s", enc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "specula-git-handler/v0.2")

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("gitlab api: %s", resp.Status)
	}
	var body struct {
		Visibility string `json:"visibility"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return strings.EqualFold(body.Visibility, "public"), nil
}

// probeGitee probes Gitee's API v5 to determine whether a repository is
// anonymously readable. The Gitee API returns a "private" boolean field;
// unauthenticated requests to private or non-existent repos receive 404 or
// 401, which we treat as non-public (fail-closed).
func (c *publicChecker) probeGitee(ctx context.Context, ref repoRef) (bool, error) {
	parts := strings.SplitN(ref.ProjectPath, "/", 2)
	if len(parts) != 2 {
		return false, nil
	}
	u := fmt.Sprintf("https://gitee.com/api/v5/repos/%s/%s",
		url.PathEscape(parts[0]), url.PathEscape(parts[1]))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "specula-git-handler/v0.2")

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("gitee api: %s", resp.Status)
	}
	var body struct {
		Private *bool `json:"private"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	if body.Private == nil {
		return false, nil
	}
	return !*body.Private, nil
}
