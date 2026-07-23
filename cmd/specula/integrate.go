package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ivanzzeth/specula/internal/integrate"
)

// runIntegrate implements: specula integrate [status] [flags]
//
// It additively wires local package clients to Specula without destroying
// existing mirrors/registries (prepend GOPROXY, keep npmrc keys, apt drop-in, …).
func runIntegrate(args []string) error {
	if len(args) > 0 && (args[0] == "status" || args[0] == "show") {
		rep, err := integrate.Status("")
		if err != nil {
			return err
		}
		fmt.Print(integrate.PrintReport(rep))
		return nil
	}

	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "http://127.0.0.1:7732", "Specula data-plane base URL")
	protocols := fs.String("protocols", strings.Join(integrate.DefaultProtocols, ","),
		"comma-separated protocols: go,npm,pypi,oci,helm,git,apt,cargo,conda,hf")
	dryRun := fs.Bool("dry-run", false, "print planned changes without writing")
	skipRoot := fs.Bool("skip-root", false, "skip apt /etc/docker actions that need root")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula integrate [flags]
  specula integrate status

Add Specula as a client-side mirror without destroying existing config:
  go     prepend Specula to GOPROXY (keep proxy.golang.org,direct, …)
  npm    set registry=…/npm/ (preserve other ~/.npmrc keys; backup old registry)
  pypi   set index-url; move previous index to extra-index-url
  oci    Docker/containerd: registry-mirrors + insecure-registries (http)
         (writes /etc/docker/daemon.json when root — sudo for live dockerd)
  helm   helm repo add specula … (owned name only)
  git    add url.<specula>/git/github.com/.insteadOf (keep other insteadOf)
  apt    write /etc/apt/sources.list.d/specula.list (never edit sources.list)

Examples:
  specula integrate --addr http://127.0.0.1:7732
  sudo specula integrate --protocols oci --addr http://127.0.0.1:7732
  specula integrate --protocols docker   # alias of oci

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

	rep, err := integrate.Run(integrate.Options{
		Addr:      *addr,
		Protocols: protos,
		DryRun:    *dryRun,
		SkipRoot:  *skipRoot,
	})
	if err != nil {
		return err
	}
	fmt.Print(integrate.PrintReport(rep))
	for _, r := range rep.Results {
		if r.Action == "error" {
			return fmt.Errorf("integrate: one or more protocols failed")
		}
	}
	if !*dryRun {
		fmt.Fprintf(os.Stderr, "\nstate: ~/.config/specula/integrate-state.json\nenv:   ~/.config/specula/env.sh\n")
	}
	return nil
}
