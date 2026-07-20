// Example: programmatic SDK Get of a Go module zip via Specula.
//
//	go run ./examples/sdk-get-module
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/specula"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

func main() {
	ctx := context.Background()
	dir := filepath.Join(os.TempDir(), "specula-sdk-example")
	_ = os.RemoveAll(dir)

	s, err := specula.New(ctx, specula.Options{
		DataDir: dir,
		Upstreams: map[string][]upstream.Upstream{
			"gomod": {{
				Name:     "goproxy.cn",
				BaseURL:  "https://goproxy.cn",
				Priority: 1,
			}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	ref := artifact.ArtifactRef{
		Protocol: "gomod",
		Name:     "golang.org/x/mod",
		Version:  "v0.20.0.info", // GOPROXY file component
	}

	entry, err := s.Get(ctx, ref)
	if err != nil {
		log.Fatalf("Get: %v", err)
	}
	fmt.Printf("cached %s/%s@%s tier=%s digest=%s size=%d\n",
		entry.Protocol, entry.Ref.Name, entry.Ref.Version,
		entry.Tier, entry.Digest, entry.Size)

	rc, err := s.Open(ctx, entry)
	if err != nil {
		log.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	n, _ := io.Copy(os.Stdout, rc)
	fmt.Fprintf(os.Stderr, "\n-- read %d verified bytes (tier=%s) --\n", n, entry.Tier)
}
