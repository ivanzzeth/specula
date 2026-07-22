package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

// runBootstrapMirror implements:
//
//	specula bootstrap-mirror write --endpoint … --certs-dir … --registries … [--hold]
func runBootstrapMirror(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, `Usage:
  specula bootstrap-mirror write [flags]

Write containerd certs.d/<registry>/hosts.toml drop-ins that redirect pulls
through a bootstrap Specula NodePort. Designed for distroless: no shell needed.

Flags:
`)
		fs := flag.NewFlagSet("bootstrap-mirror write", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		_ = fs.String("endpoint", "http://127.0.0.1:30732", "mirror URL the node dials")
		_ = fs.String("certs-dir", "/etc/containerd/certs.d", "containerd certs.d root")
		_ = fs.String("registries", "docker.io,registry.k8s.io", "comma-separated registries")
		_ = fs.Bool("skip-verify", true, "set skip_verify on the mirror host entry")
		_ = fs.Bool("hold", false, "sleep forever after writing (DaemonSet)")
		fs.PrintDefaults()
		return nil
	}
	if args[0] != "write" {
		return fmt.Errorf("unknown subcommand %q (want write)", args[0])
	}

	fs := flag.NewFlagSet("bootstrap-mirror write", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "http://127.0.0.1:30732", "mirror URL the node dials")
	certsDir := fs.String("certs-dir", "/etc/containerd/certs.d", "containerd certs.d root")
	registries := fs.String("registries", "docker.io,registry.k8s.io", "comma-separated registries")
	skipVerify := fs.Bool("skip-verify", true, "set skip_verify on the mirror host entry")
	hold := fs.Bool("hold", false, "sleep forever after writing (DaemonSet)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	var regs []string
	for _, r := range strings.Split(*registries, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			regs = append(regs, r)
		}
	}
	if err := bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
		CertsDir:   *certsDir,
		Endpoint:   *endpoint,
		Registries: regs,
		SkipVerify: *skipVerify,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "bootstrap-mirror: wrote %d registry host(s) under %s → %s\n",
		len(regs), *certsDir, *endpoint)
	if !*hold {
		return nil
	}
	fmt.Fprintln(os.Stdout, "bootstrap-mirror: holding (SIGINT/SIGTERM to exit)")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

// runBootstrapPrefetch implements:
//
//	specula bootstrap-prefetch --addr http://… --images a:tag,b:tag
func runBootstrapPrefetch(args []string) error {
	fs := flag.NewFlagSet("bootstrap-prefetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "http://127.0.0.1:7732", "Specula data-plane base URL")
	images := fs.String("images", "", "comma-separated image refs to warm")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  specula bootstrap-prefetch --addr http://specula:7732 --images docker.io/library/hello-world:latest,...

Warm OCI manifests through a bootstrap Specula (token + manifest GET) so HA
dependency metadata is cached before kubelet pulls.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list []string
	for _, img := range strings.Split(*images, ",") {
		img = strings.TrimSpace(img)
		if img != "" {
			list = append(list, img)
		}
	}
	if len(list) == 0 {
		fs.Usage()
		return fmt.Errorf("--images is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	results, err := bootstrap.WarmImages(ctx, bootstrap.PrefetchOptions{
		Addr:   *addr,
		Images: list,
	})
	if err != nil {
		return err
	}
	var failed int
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "warm %s (path=%s) -> ERR: %v\n", r.Ref, r.Path, r.Err)
			continue
		}
		fmt.Fprintf(os.Stdout, "warm %s (path=%s) -> %d\n", r.Ref, r.Path, r.StatusCode)
	}
	if failed > 0 {
		return fmt.Errorf("bootstrap-prefetch: %d/%d image(s) failed", failed, len(results))
	}
	fmt.Fprintln(os.Stdout, "bootstrap-prefetch: done")
	return nil
}
