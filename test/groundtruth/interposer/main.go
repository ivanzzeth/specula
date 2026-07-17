// Command interposer runs a recording proxy between Specula and a real upstream
// mirror, so that groundtruth-gate.sh can answer the one question Specula's own
// counters structurally cannot answer honestly: did this request ACTUALLY
// contact an upstream?
//
// All the arbitration logic — recording, counting, failure injection — lives in
// the proxy package next door, where the coverage gate holds it to the normal
// threshold. This file is flag parsing and listener wiring only, mirroring how
// cmd/specula keeps main thin.
//
// # Usage
//
//	interposer -listen 127.0.0.1:9001 -control 127.0.0.1:9002 \
//	           -upstream https://goproxy.cn -name gomod
//
// Data port (-listen): everything is forwarded to -upstream, verbatim, and
// recorded. Control port (-control): a separate listener, so control endpoints
// can never collide with a proxied path.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ivanzzeth/specula/test/groundtruth/interposer/proxy"
)

func main() {
	var (
		listen     = flag.String("listen", "127.0.0.1:0", "data plane listen address (what Specula talks to)")
		control    = flag.String("control", "127.0.0.1:0", "control listen address (counts, mode, reset)")
		upstreamF  = flag.String("upstream", "", "real upstream base URL, e.g. https://goproxy.cn (required)")
		name       = flag.String("name", "interposer", "label for logs and the ports file")
		logPath    = flag.String("log", "", "append every record as JSONL to this file (optional)")
		portsPath  = flag.String("ports", "", "write the chosen ports here as JSON (optional)")
		delay      = flag.Duration("delay", 0, "artificial delay before forwarding; widens the single-flight window deterministically")
		failStatus = flag.Int("fail-status", http.StatusServiceUnavailable, "status returned in mode=fail")
	)
	flag.Parse()

	if *upstreamF == "" {
		log.Fatal("interposer: -upstream is required")
	}
	u, err := url.Parse(strings.TrimRight(*upstreamF, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Fatalf("interposer: bad -upstream %q: %v", *upstreamF, err)
	}

	ip := proxy.New(*name, u)
	ip.Delay = *delay
	ip.FailStatus = *failStatus

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("interposer: open -log: %v", err)
		}
		defer f.Close()
		ip.LogFile = f
	}

	dataLn, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("interposer: listen data: %v", err)
	}
	ctrlLn, err := net.Listen("tcp", *control)
	if err != nil {
		log.Fatalf("interposer: listen control: %v", err)
	}

	if *portsPath != "" {
		if err := proxy.WritePorts(*portsPath, *name,
			dataLn.Addr().String(), ctrlLn.Addr().String(), u.String(), os.Getpid()); err != nil {
			log.Fatalf("interposer: write -ports: %v", err)
		}
	}

	fmt.Printf("interposer[%s] data=http://%s control=http://%s upstream=%s pid=%d\n",
		*name, dataLn.Addr(), ctrlLn.Addr(), u, os.Getpid())

	go func() {
		if err := http.Serve(ctrlLn, ip.ControlMux()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("interposer: control server: %v", err)
		}
	}()

	srv := &http.Server{
		Handler:           http.HandlerFunc(ip.ServeProxy),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		_ = srv.Close()
	}()
	if err := srv.Serve(dataLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("interposer: data server: %v", err)
	}
}
