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
		if integrate.ReportHasBlockingFindings(rep) {
			return fmt.Errorf("status reports blocking risks — run: specula doctor")
		}
		return nil
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:])
	}

	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", integrate.DefaultAddr, "Specula data-plane base URL (default https://127.0.0.1:7732)")
	protocols := fs.String("protocols", strings.Join(integrate.DefaultProtocols, ","),
		"comma-separated protocols: go,npm,pypi,oci,helm,git,apt,cargo,conda,hf")
	dryRun := fs.Bool("dry-run", false, "print planned changes without writing")
	skipRoot := fs.Bool("skip-root", false, "skip apt /etc/docker actions that need root")
	skipSchemeProbe := fs.Bool("skip-scheme-probe", false, "do not auto-upgrade http://→https:// when the port speaks TLS only")
	configPath := fs.String("config", "", "path to specula.yaml (optional; enables multi-source helm/apt/conda wiring)")
	registryHost := fs.String("registry-host", "",
		"OCI hostname to wire to --addr (k3s: registries.yaml + certs.d; vanilla: certs.d)")
	caFile := fs.String("ca-file", "",
		"path to PEM CA cert on this node (e.g. /etc/specula/ca.crt); trust HTTPS Specula without skip_verify")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula integrate [flags]
  specula integrate status
  specula integrate doctor   # alias of: specula doctor

Add Specula as a client-side mirror without destroying existing config:
  go     prepend Specula to GOPROXY (keep proxy.golang.org,direct, …)
  npm    set registry=…/npm/ (preserve other ~/.npmrc keys; backup old registry)
  pypi   set sole index-url (never promote old index to extra-index-url)
  oci    Docker/containerd: registry-mirrors + insecure-registries (http)
         (+ --registry-host wires that hostname → --addr on k3s/vanilla)
  helm   helm repo add specula … (owned name only)
  git    add url.<specula>/git/github.com/.insteadOf (keep other insteadOf)
  apt    write /etc/apt/sources.list.d/specula.list (never edit sources.list)

Examples:
  specula integrate --addr https://127.0.0.1:7732
  sudo specula integrate --protocols oci --addr https://127.0.0.1:7732
  sudo specula integrate --protocols oci --addr https://specula.example.test:7732 --ca-file /etc/specula/ca.crt
  specula integrate --protocols docker   # alias of oci
  # plain http only when the data plane is truly cleartext:
  specula integrate --addr http://127.0.0.1:7732   # auto-upgrades to https if port is TLS-only

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
		Addr:            *addr,
		Protocols:       protos,
		DryRun:          *dryRun,
		SkipRoot:        *skipRoot,
		ConfigPath:      *configPath,
		RegistryHost:    *registryHost,
		CAFile:          *caFile,
		SkipSchemeProbe: *skipSchemeProbe,
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
		fmt.Fprintf(os.Stderr, "tip:   specula doctor   # catch CRI/k3s footguns before kubeadm hangs\n")
	}
	return nil
}
