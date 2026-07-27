package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/cacheimport"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/verify"
)

// runCache dispatches `specula cache <subcommand>`.
func runCache(args []string) error {
	if len(args) == 0 {
		cacheUsage()
		return errors.New("missing subcommand")
	}
	switch args[0] {
	case "import":
		return runCacheImport(args[1:])
	case "-h", "--help", "help":
		cacheUsage()
		return nil
	default:
		cacheUsage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func cacheUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  specula cache import --from <oci-layout> --as <image-ref> [--config specula.yaml]

Seed the cache from an OCI image layout produced on a machine that CAN reach the
upstream, so later pulls hit the cache and contact no upstream at all. Also the
way to fill an air-gapped install.

Produce the layout with a tool that preserves the registry's digests:

  crane pull --format=oci docker.io/library/redis:7-alpine redis.tar
  # or
  skopeo copy docker://docker.io/library/redis:7-alpine oci-archive:redis.tar

Then, anywhere Specula's stores are reachable. The Pod itself is usually simplest —
it already has the binary, the config and the credentials — and the archive goes in
over stdin, because kubectl cp shells out to tar inside the container and a
distroless image has none:

  kubectl exec -i -n specula-boot <pod> -- /specula cache import \
      --config /etc/specula/specula.yaml \
      --from - --as docker.io/library/redis:7-alpine < redis.tar

A legacy 'docker save' archive is refused: it re-packs layers, so its digests are
not the ones clients ask for.
`)
}

func runCacheImport(args []string) error {
	fs := flag.NewFlagSet("cache import", flag.ContinueOnError)
	var (
		cfgPath = fs.String("config", "", "path to the Specula config (default: SPECULA_CONFIG or specula.yaml)")
		from    = fs.String("from", "", "OCI layout directory, OCI archive (tar), or - for stdin")
		spool   = fs.String("spool-dir", "", "where to expand an archive (default: OS temp; use the data volume for large images)")
		as      = fs.String("as", "", "reference clients will pull, e.g. docker.io/library/redis:7-alpine")
		ttl     = fs.Int64("tag-ttl-seconds", 0, "TTL for the tag pointer (0 = long-lived default)")
		dryRun  = fs.Bool("dry-run", false, "report what would be imported without writing")
		quiet   = fs.Bool("quiet", false, "only print the summary line")
	)
	fs.Usage = cacheUsage
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *as == "" {
		cacheUsage()
		return errors.New("--from and --as are both required")
	}

	path := *cfgPath
	if path == "" {
		path = os.Getenv("SPECULA_CONFIG")
	}
	if path == "" {
		path = "specula.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load config %q: %w", path, err)
	}

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx := context.Background()

	// The same stores the daemon uses, from the same config — so an import writes
	// exactly where a pull-through fetch would have.
	blobs, err := buildBlobStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	metaStore, closeMeta, err := buildMetaStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("metadata store: %w", err)
	}
	defer closeMeta()

	// Verify-on-write is the point of going through the cache manager rather than
	// writing objects directly: a layout with tampered bytes is rejected here
	// instead of being discovered by a client.
	cm := cache.New(blobs, metaStore, verify.NewChain())

	spoolDir := *spool
	if spoolDir == "" {
		// An expanded layout is about the size of the image, and the container's
		// ephemeral layer is not where a 500 MB one belongs: filling it gets the Pod
		// evicted, which reads as a crash rather than a full disk. The quarantine
		// directory is already the configured place for large in-flight content.
		spoolDir = cfg.EffectiveQuarantineDir()
	}

	res, err := cacheimport.Run(ctx, cm, metaStore, cacheimport.Options{
		Source:     *from,
		Target:     *as,
		TTLSeconds: *ttl,
		DryRun:     *dryRun,
		SpoolDir:   spoolDir,
		Stdin:      os.Stdin,
		Logger:     log,
	})
	if err != nil {
		// The legacy-format refusal carries its own instructions; do not bury them.
		if errors.Is(err, cacheimport.ErrLegacyDockerSave) {
			return err
		}
		return fmt.Errorf("import %q: %w", *from, err)
	}

	verb := "imported"
	if *dryRun {
		verb = "would import"
	}
	tagPart := ""
	if res.Tag != "" {
		tagPart = ":" + res.Tag
	}
	fmt.Printf("cache import: %s %s%s → %d manifest(s), %d blob(s), %s (%d already present)\n",
		verb, res.Name, tagPart, res.Manifests, res.Blobs, humanBytes(res.Bytes), res.AlreadyPresent)
	fmt.Printf("  manifest digest: %s\n", res.ManifestDigest)
	if !*dryRun {
		fmt.Printf("  pulls of %s now hit the cache with no upstream request\n", strings.TrimSpace(*as))
	}
	return nil
}

// humanBytes formats a byte count for the summary line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", v/unit)
}
