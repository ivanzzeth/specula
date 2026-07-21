// Package bench runs end-to-end throughput probes against a live Specula
// data plane: for each protocol it downloads a representative artifact once
// (cold) and again (warm), reporting bytes, wall time, and MB/s.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultProtocols is used when --protocols is omitted.
var DefaultProtocols = []string{"go", "npm", "pypi", "oci", "helm", "tarball", "apt", "git"}

// Options configures a bench run.
type Options struct {
	Addr      string
	Protocols []string
	// WarmRounds is how many additional passes after the first (cold) fetch.
	// Default 1 → one cold + one warm.
	WarmRounds int
	// Timeout per individual download.
	Timeout time.Duration
	// HTTPClient overrides the default client (tests).
	HTTPClient *http.Client
}

// Row is one timed download.
type Row struct {
	Protocol string  `json:"protocol"`
	Name     string  `json:"name"`
	Pass     string  `json:"pass"` // cold | warm | warm2…
	Bytes    int64   `json:"bytes"`
	Seconds  float64 `json:"seconds"`
	MBps     float64 `json:"mb_per_s"`
	Status   int     `json:"status"`
	Err      string  `json:"error,omitempty"`
}

// Report is the full bench summary.
type Report struct {
	Addr    string `json:"addr"`
	Results []Row  `json:"results"`
}

type caseSpec struct {
	protocol string
	name     string
	// path is relative to Addr (may start with /).
	path string
	// prep optionally mutates the request (e.g. OCI bearer).
	prep func(ctx context.Context, client *http.Client, addr string, req *http.Request) error
}

// Run executes cold (+ optional warm) downloads for the selected protocols.
func Run(ctx context.Context, opts Options) (Report, error) {
	addr := strings.TrimRight(strings.TrimSpace(opts.Addr), "/")
	if addr == "" {
		addr = "http://127.0.0.1:7732"
	}
	protos := opts.Protocols
	if len(protos) == 0 {
		protos = append([]string(nil), DefaultProtocols...)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	// WarmRounds: 0 = cold only; N = one cold + N warm passes.
	warm := opts.WarmRounds
	if warm < 0 {
		warm = 1
	}

	cases := selectCases(protos)
	rep := Report{Addr: addr}
	for _, c := range cases {
		passes := 1 + warm
		for i := 0; i < passes; i++ {
			pass := "cold"
			if i == 1 && warm == 1 {
				pass = "warm"
			} else if i > 0 {
				pass = fmt.Sprintf("warm%d", i)
			}
			row := runCase(ctx, client, addr, c, pass)
			rep.Results = append(rep.Results, row)
		}
	}
	return rep, nil
}

func selectCases(protos []string) []caseSpec {
	all := map[string]caseSpec{
		"go": {
			protocol: "go",
			name:     "golang.org/x/text@v0.15.0.zip",
			path:     "/go/golang.org/x/text/@v/v0.15.0.zip",
		},
		"npm": {
			protocol: "npm",
			name:     "left-pad-1.3.0.tgz",
			path:     "/npm/left-pad/-/left-pad-1.3.0.tgz",
		},
		"pypi": {
			protocol: "pypi",
			name:     "certifi-2024.2.2-py3-none-any.whl",
			path:     "/pypi/packages/py3/c/certifi/certifi-2024.2.2-py3-none-any.whl",
		},
		"helm": {
			protocol: "helm",
			name:     "index.yaml",
			path:     "/helm/index.yaml",
		},
		"tarball": {
			protocol: "tarball",
			name:     "cobra-v1.8.1.tar.gz",
			path:     "/tarball/github.com/spf13/cobra/archive/refs/tags/v1.8.1.tar.gz",
		},
		"apt": {
			protocol: "apt",
			name:     "noble/main Packages.gz",
			path:     "/apt/dists/noble/main/binary-amd64/Packages.gz",
		},
		"oci": {
			protocol: "oci",
			name:     "library/hello-world:latest manifest",
			path:     "/v2/library/hello-world/manifests/latest",
			prep:     ociAuthPrep("library/hello-world"),
		},
		"git": {
			protocol: "git",
			name:     "spf13/cobra.git info/refs",
			path:     "/git/github.com/spf13/cobra.git/info/refs?service=git-upload-pack",
			prep: func(_ context.Context, _ *http.Client, _ string, req *http.Request) error {
				req.Header.Set("User-Agent", "git/2.40.0")
				req.Header.Set("Git-Protocol", "version=2")
				return nil
			},
		},
	}
	out := make([]caseSpec, 0, len(protos))
	for _, p := range protos {
		p = strings.ToLower(strings.TrimSpace(p))
		if c, ok := all[p]; ok {
			out = append(out, c)
		}
	}
	return out
}

func runCase(ctx context.Context, client *http.Client, addr string, c caseSpec, pass string) Row {
	row := Row{Protocol: c.protocol, Name: c.name, Pass: pass}
	url := addr + c.path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	if c.prep != nil {
		if err := c.prep(ctx, client, addr, req); err != nil {
			row.Err = err.Error()
			return row
		}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		row.Err = err.Error()
		row.Seconds = time.Since(start).Seconds()
		return row
	}
	defer resp.Body.Close()
	row.Status = resp.StatusCode
	n, copyErr := io.Copy(io.Discard, resp.Body)
	row.Seconds = time.Since(start).Seconds()
	row.Bytes = n
	if copyErr != nil {
		row.Err = copyErr.Error()
		return row
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		row.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return row
	}
	if row.Seconds > 0 {
		row.MBps = (float64(row.Bytes) / (1024 * 1024)) / row.Seconds
	}
	return row
}

func ociAuthPrep(repo string) func(context.Context, *http.Client, string, *http.Request) error {
	return func(ctx context.Context, client *http.Client, addr string, req *http.Request) error {
		tokURL := fmt.Sprintf("%s/token?service=specula&scope=repository:%s:pull", addr, repo)
		treq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(treq)
		if err != nil {
			return fmt.Errorf("oci token: %w", err)
		}
		defer resp.Body.Close()
		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return fmt.Errorf("oci token decode: %w", err)
		}
		tok := body.Token
		if tok == "" {
			tok = body.AccessToken
		}
		if tok == "" {
			return fmt.Errorf("oci token empty (HTTP %d)", resp.StatusCode)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
			"application/vnd.oci.image.index.v1+json",
		}, ", "))
		return nil
	}
}

// FormatTable renders a human-readable throughput table.
func FormatTable(rep Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "specula bench  addr=%s\n", rep.Addr)
	fmt.Fprintf(&b, "%-8s %-6s %10s %10s %10s  %s\n",
		"PROTO", "PASS", "BYTES", "SECONDS", "MB/s", "ARTIFACT")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 88))

	rows := append([]Row(nil), rep.Results...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Protocol != rows[j].Protocol {
			return rows[i].Protocol < rows[j].Protocol
		}
		return rows[i].Pass < rows[j].Pass
	})
	for _, r := range rows {
		if r.Err != "" {
			fmt.Fprintf(&b, "%-8s %-6s %10s %10s %10s  %s  ERR %s\n",
				r.Protocol, r.Pass, formatBytes(r.Bytes), formatSec(r.Seconds), "—", r.Name, r.Err)
			continue
		}
		fmt.Fprintf(&b, "%-8s %-6s %10s %10s %10s  %s\n",
			r.Protocol, r.Pass, formatBytes(r.Bytes), formatSec(r.Seconds), formatMBps(r.MBps), r.Name)
	}
	return b.String()
}

func formatBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatSec(s float64) string {
	if s < 0.001 {
		return fmt.Sprintf("%.3fms", s*1000)
	}
	if s < 1 {
		return fmt.Sprintf("%.0fms", s*1000)
	}
	return fmt.Sprintf("%.2fs", s)
}

func formatMBps(v float64) string {
	if v <= 0 {
		return "—"
	}
	if v < 0.01 {
		return fmt.Sprintf("%.1f KiB/s", v*1024)
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
