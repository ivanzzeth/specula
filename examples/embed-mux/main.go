// Example: embed Specula protocol handlers into an existing Go HTTP server.
//
//	go run ./examples/embed-mux
//	curl -sI http://127.0.0.1:7732/gomod/golang.org/x/mod/@v/list
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ivanzzeth/specula/pkg/embed"
	"github.com/ivanzzeth/specula/pkg/specula"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dir := filepath.Join(os.TempDir(), "specula-embed-example")

	ups := map[string][]upstream.Upstream{
		"gomod": {{Name: "goproxy.cn", BaseURL: "https://goproxy.cn", Priority: 1}},
		"oci":   {{Name: "dockerhub", BaseURL: "https://registry-1.docker.io", Priority: 1}},
	}

	s, err := specula.New(ctx, specula.Options{
		DataDir:   dir,
		Upstreams: ups,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	embed.Mount(mux, s, embed.Options{
		Protocols: []string{"gomod", "oci"},
		Upstreams: ups,
	})

	srv := &http.Server{Addr: ":7732", Handler: mux}
	go func() {
		log.Printf("embed-mux listening on %s (gomod + oci)", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
