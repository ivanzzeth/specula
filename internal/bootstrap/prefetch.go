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

	"github.com/ivanzzeth/specula/internal/upstream"
)

// PrefetchOptions configures WarmImages.
type PrefetchOptions struct {
	// Addr is the Specula base URL, e.g. http://specula-bootstrap:7733
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
		client = &http.Client{
			Timeout:   60 * time.Second,
			Transport: upstream.WrapUserAgent(http.DefaultTransport),
		}
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

// firstSegment returns the part before the first "/" (the whole string when absent).
func firstSegment(s string) string {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}

// isRegistryHost reports whether a leading path segment looks like a registry host
// rather than a Docker Hub organisation: hosts carry a dot or a port, org names do not
// ("bitnami" is an org, "ghcr.io" and "reg.example.com:5000" are hosts).
func isRegistryHost(seg string) bool {
	return strings.Contains(seg, ".") || strings.Contains(seg, ":")
}

func parseImageRef(ref string) (path, tag string, err error) {
	// Guard first: an empty ref would otherwise become the repo "library/" once the
	// official-image prefix is applied, and warm a nonexistent path.
	if strings.TrimSpace(ref) == "" {
		return "", "", fmt.Errorf("empty image ref")
	}
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
	// Specula routes non-Hub registries BY PATH: /v2/<registry-host>/<repo>/… (see
	// oci.parseRemoteName). So the host must stay in the path — stripping it, as this
	// used to do for registry.k8s.io and for any host-looking first segment, turns
	// registry.k8s.io/pause into the Hub repo "pause", which does not exist. The warm
	// then fails with a 403 from the Hub mirror that reads like an upstream outage.
	//
	// docker.io is the one host that IS dropped: Hub is the default namespace, and
	// bare/official names additionally need the library/ prefix.
	if h, rest, ok := strings.Cut(repo, "/"); ok && (h == "docker.io" || h == "index.docker.io") {
		repo = rest
	}
	if isRegistryHost(firstSegment(repo)) {
		// Non-Hub registry: keep host/repo exactly as-is.
	} else if !strings.Contains(repo, "/") {
		// Bare official image.
		repo = "library/" + repo
	}
	// Every segment must be a real name. Without this, ":" survives as a "registry
	// host" (it contains a colon) and a bare host with no repo would be warmed.
	segs := strings.Split(repo, "/")
	for _, seg := range segs {
		if strings.TrimSpace(seg) == "" {
			return "", "", fmt.Errorf("invalid image ref %q: empty path segment", ref)
		}
	}
	if isRegistryHost(segs[0]) && len(segs) < 2 {
		return "", "", fmt.Errorf("invalid image ref %q: registry host with no repository", ref)
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
