package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ivanzzeth/specula/internal/integrate"
)

// runDoctor implements: specula doctor [flags]
//
// Node-side preflight for OCI/CRI/k3s footguns (colon config_path, residual
// server=, wrong certs.d root, Specula unreachable) before kubeadm/crictl
// hang on public origins.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "http://127.0.0.1:7732", "Specula data-plane base URL to probe")
	skipProbe := fs.Bool("skip-probe", false, "skip HTTP check of Addr/v2/")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
  specula doctor [flags]
  specula integrate doctor [flags]

Catch OCI/CRI wiring footguns before production:
  • containerd 2.2 colon-separated CRI config_path (hosts.toml ignored)
  • effective config dump still colon after file fix (forgot restart)
  • residual server= public fallback in hosts.toml
  • k3s writing only /etc/containerd/certs.d (live path is agent tree)
  • missing registry.k8s.io / docker.io hosts.toml
  • Specula /v2/ unreachable

Exit 1 when any RISK or error is reported.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	rep, err := integrate.Doctor(integrate.DoctorOptions{
		Addr:      *addr,
		SkipProbe: *skipProbe,
	})
	if err != nil {
		return err
	}
	out := integrate.PrintReport(rep)
	out = strings.Replace(out, "specula integrate", "specula doctor", 1)
	fmt.Print(out)
	if integrate.ReportHasBlockingFindings(rep) {
		fmt.Fprintf(os.Stderr, "\ndoctor: fix RISKs above, then re-run (often: sudo specula integrate --protocols oci && systemctl restart containerd)\n")
		return fmt.Errorf("doctor found blocking risks")
	}
	return nil
}
