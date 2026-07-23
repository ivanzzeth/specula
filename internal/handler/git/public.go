// Package git — public-repo visibility probe (ported from ai-sandbox gitproxy/public.go).
//
// publicChecker probes the upstream hosting service to determine whether a
// repository is anonymously readable. Results are cached for a configurable TTL.
// Probing is supported for github.com, gitlab.com, gitee.com, codeberg.org,
// bitbucket.org, and git.sr.ht; all other allowlisted hosts return (false, error)
// which triggers fail-closed behaviour in the handler when public_only=true.
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
	case "codeberg.org":
		return c.probeCodeberg(ctx, ref)
	case "bitbucket.org":
		return c.probeBitbucket(ctx, ref)
	case "git.sr.ht":
		return c.probeSourceHut(ctx, ref)
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

// probeCodeberg probes the Gitea/Forgejo API v1 (Codeberg) for the "private"
// boolean field. Unauthenticated requests to private or missing repos receive
// 404 or 401, treated as non-public (fail-closed).
func (c *publicChecker) probeCodeberg(ctx context.Context, ref repoRef) (bool, error) {
	parts := strings.SplitN(ref.ProjectPath, "/", 2)
	if len(parts) != 2 {
		return false, nil
	}
	u := fmt.Sprintf("https://codeberg.org/api/v1/repos/%s/%s",
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
		return false, fmt.Errorf("codeberg api: %s", resp.Status)
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

// probeBitbucket probes the Bitbucket Cloud REST API v2.0. A 404 indicates a
// private or missing repository; 200 responses include an "is_private" field.
func (c *publicChecker) probeBitbucket(ctx context.Context, ref repoRef) (bool, error) {
	parts := strings.SplitN(ref.ProjectPath, "/", 2)
	if len(parts) != 2 {
		return false, nil
	}
	u := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s",
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

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("bitbucket api: %s", resp.Status)
	}
	var body struct {
		IsPrivate *bool `json:"is_private"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	if body.IsPrivate == nil {
		return false, nil
	}
	return !*body.IsPrivate, nil
}

// probeSourceHut performs a best-effort HTTP visibility check against
// git.sr.ht. SourceHut does not expose a stable public JSON API for anonymous
// repo metadata, so we treat HTTP 200 on the repo page as publicly readable and
// 404 as private or missing. This is inherently heuristic and may change if
// sr.ht adjusts anonymous access behaviour.
func (c *publicChecker) probeSourceHut(ctx context.Context, ref repoRef) (bool, error) {
	u := fmt.Sprintf("https://git.sr.ht/%s", ref.ProjectPath)
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

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("git.sr.ht: %s", resp.Status)
	}
	return true, nil
}
