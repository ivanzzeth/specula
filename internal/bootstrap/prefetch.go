package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PrefetchOptions configures WarmImages.
type PrefetchOptions struct {
	// Addr is the Specula data-plane base URL, e.g. http://specula-bootstrap:7732
	Addr string
	// Images are refs like docker.io/bitnami/postgresql:latest or library/hello-world:latest
	Images []string
	// HTTPClient optional; defaults to a 60s timeout client.
	HTTPClient *http.Client
}

// WarmResult is one image warm attempt.
type WarmResult struct {
	Ref        string
	Path       string
	StatusCode int
	Err        error
}

// WarmImages walks Docker Registry v2 token → manifest against a Specula mirror
// so metadata is cached before HA dependency pulls.
func WarmImages(ctx context.Context, opts PrefetchOptions) ([]WarmResult, error) {
	addr := strings.TrimRight(strings.TrimSpace(opts.Addr), "/")
	if addr == "" {
		return nil, fmt.Errorf("bootstrap: addr is required")
	}
	if len(opts.Images) == 0 {
		return nil, fmt.Errorf("bootstrap: at least one image is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := make([]WarmResult, 0, len(opts.Images))
	for _, ref := range opts.Images {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		path, tag, err := parseImageRef(ref)
		r := WarmResult{Ref: ref, Path: path}
		if err != nil {
			r.Err = err
			out = append(out, r)
			continue
		}
		code, werr := warmOne(ctx, client, addr, path, tag)
		r.StatusCode = code
		r.Err = werr
		out = append(out, r)
	}
	return out, nil
}

func parseImageRef(ref string) (path, tag string, err error) {
	tag = "latest"
	repo := ref
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i+1:], "/") {
		// tag, not port in host:port
		hostPart := ref[:i]
		if strings.Count(hostPart, "/") >= 1 || !strings.Contains(hostPart, ".") {
			repo = hostPart
			tag = ref[i+1:]
		}
	}
	repo = strings.TrimPrefix(repo, "docker.io/")
	// Bare names (redis) need library/
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	// Strip a registry host prefix for non-docker.io (registry.k8s.io/metrics-server/...)
	// Specula OCI paths are registry-relative under the configured upstream; for
	// docker.io-style paths we keep org/name. For registry.k8s.io keep full path
	// after the host.
	if strings.HasPrefix(repo, "registry.k8s.io/") {
		repo = strings.TrimPrefix(repo, "registry.k8s.io/")
	} else if i := strings.Index(repo, "/"); i > 0 {
		first := repo[:i]
		if strings.Contains(first, ".") || strings.Contains(first, ":") {
			// host/name/... → drop host for docker-hub-style; keep for others as path
			if first != "docker.io" {
				repo = repo[i+1:]
			}
		}
	}
	if repo == "" || tag == "" {
		return "", "", fmt.Errorf("invalid image ref %q", ref)
	}
	return repo, tag, nil
}

func warmOne(ctx context.Context, client *http.Client, addr, path, tag string) (int, error) {
	tok, err := fetchToken(ctx, client, addr, path)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		addr+"/v2/"+path+"/manifests/"+url.PathEscape(tag), nil)
	if err != nil {
		return 0, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", "))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("manifest GET: HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func fetchToken(ctx context.Context, client *http.Client, addr, path string) (string, error) {
	u := addr + "/token?service=specula&scope=" + url.QueryEscape("repository:"+path+":pull")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Some setups allow anonymous pull without token.
		return "", nil
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("token json: %w", err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	return payload.AccessToken, nil
}
