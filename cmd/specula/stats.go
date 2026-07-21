package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/admin"
	"github.com/ivanzzeth/specula/internal/clicreds"
	"github.com/ivanzzeth/specula/internal/metrics"
)

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "", "control-plane URL (default: credentials / SPECULA_ADDR / http://127.0.0.1:7733)")
	token := fs.String("token", "", "API key (default: credentials / SPECULA_TOKEN)")
	watch := fs.Duration("watch", 0, "if >0, refresh every duration (e.g. 2s)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	trafficOnly := fs.Bool("traffic-only", false, "skip auth; public GET /api/v1/traffic only")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula stats [flags]

Show LIVE cache occupancy + runtime throughput from a running Specula daemon
(GET /api/v1/stats with API key, or public /api/v1/traffic without one).

Authenticate once with:
  specula login --token spck_…

Credentials: ~/.config/specula/credentials.json
Env: SPECULA_TOKEN, SPECULA_CONTROL_PLANE / SPECULA_ADDR

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := clicreds.Resolve(*addr, *token, "http://127.0.0.1:7733")
	if err != nil {
		return err
	}

	printOnce := func() error {
		if *trafficOnly || creds.Token == "" {
			snap, err := fetchTraffic(creds.ControlPlane)
			if err != nil {
				return err
			}
			if *jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(snap)
			}
			if creds.Token == "" && !*trafficOnly {
				fmt.Fprintln(os.Stderr, "note: no API key — showing traffic only. Run: specula login --token spck_…")
			}
			fmt.Print(metrics.FormatTrafficTable(snap))
			return nil
		}

		inst, err := fetchInstanceStats(creds.ControlPlane, creds.Token)
		if err != nil {
			return err
		}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(inst)
		}
		fmt.Print(formatCacheTable(inst.Cache))
		fmt.Println()
		fmt.Print(metrics.FormatTrafficTable(inst.Traffic))
		return nil
	}

	if *watch <= 0 {
		return printOnce()
	}
	for {
		if err := printOnce(); err != nil {
			fmt.Fprintf(os.Stderr, "specula stats: %v\n", err)
		}
		fmt.Fprintln(os.Stdout)
		time.Sleep(*watch)
	}
}

func fetchTraffic(ctrl string) (metrics.TrafficSnapshot, error) {
	url := strings.TrimRight(ctrl, "/") + "/api/v1/traffic"
	resp, err := http.Get(url) //nolint:gosec // operator CLI against configured control plane
	if err != nil {
		return metrics.TrafficSnapshot{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return metrics.TrafficSnapshot{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return metrics.TrafficSnapshot{}, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	var snap metrics.TrafficSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return metrics.TrafficSnapshot{}, err
	}
	return snap, nil
}

func fetchInstanceStats(ctrl, token string) (admin.InstanceStatsResponse, error) {
	url := strings.TrimRight(ctrl, "/") + "/api/v1/stats"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return admin.InstanceStatsResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return admin.InstanceStatsResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return admin.InstanceStatsResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return admin.InstanceStatsResponse{}, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	var inst admin.InstanceStatsResponse
	if err := json.Unmarshal(body, &inst); err != nil {
		return admin.InstanceStatsResponse{}, err
	}
	return inst, nil
}

func formatCacheTable(c admin.StatsResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "specula cache  total=%s  objects=%d  disk_used=%s  disk_free=%s\n",
		formatStatBytes(c.TotalBytes), c.TotalObjects,
		formatStatBytes(c.BackendDiskUsed), formatStatBytes(c.BackendDiskFree))
	fmt.Fprintf(&b, "%-8s %12s %10s\n", "PROTO", "BYTES", "OBJECTS")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 34))
	for _, p := range c.PerProtocol {
		obj := "—"
		if p.Objects != nil {
			obj = fmt.Sprintf("%d", *p.Objects)
		}
		fmt.Fprintf(&b, "%-8s %12s %10s\n", p.Protocol, formatStatBytes(p.Bytes), obj)
	}
	return b.String()
}

func formatStatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	return formatTrafficBytesCLI(uint64(n))
}

func formatTrafficBytesCLI(n uint64) string {
	switch {
	case n >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	case n >= 1024*1024:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
