package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/bench"
)

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "http://127.0.0.1:7732", "Specula data-plane base URL")
	protocols := fs.String("protocols", strings.Join(bench.DefaultProtocols, ","),
		"comma-separated: go,npm,pypi,oci,helm,tarball,apt,git")
	warm := fs.Int("warm-rounds", 1, "warm passes after the cold fetch (0 = cold only)")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-download timeout")
	jsonOut := fs.Bool("json", false, "emit JSON instead of a table")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula bench [flags]

Download a representative artifact per protocol through Specula and report
bytes / wall time / MB/s (cold then warm). This measures end-to-end pull
throughput as seen by an HTTP client — not Prometheus TTFB.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	var protos []string
	for _, p := range strings.Split(*protocols, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			protos = append(protos, p)
		}
	}

	ctx := context.Background()
	rep, err := bench.Run(ctx, bench.Options{
		Addr:       *addr,
		Protocols:  protos,
		WarmRounds: *warm,
		Timeout:    *timeout,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Print(bench.FormatTable(rep))
	failed := 0
	for _, r := range rep.Results {
		if r.Err != "" {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d downloads failed", failed, len(rep.Results))
	}
	return nil
}
