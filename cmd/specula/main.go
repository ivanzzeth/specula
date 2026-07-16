// Command specula is the Specula daemon entrypoint. It loads configuration,
// constructs the selected CAS BlobStore + MetadataStore, builds the streaming
// verification chain (checksum + TOFU) and CacheManager, then serves the OCI
// data plane on its port and the control-plane health/metrics endpoints.
//
// v0.1 scope: only the OCI data-plane handler actually serves. The other seven
// protocol ports (pypi, npm, go, apt, helm, tarball, git) are wired as TODO
// stubs on the data plane and return 501.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/stats"
	blobstore "github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/local"
	metastore "github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/postgres"
	"github.com/ivanzzeth/specula/internal/store/s3"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

const banner = `
   ____                       _
  / ___| _ __   ___  ___ _   _| | __ _
  \___ \| '_ \ / _ \/ __| | | | |/ _` + "`" + ` |
   ___) | |_) |  __/ (__| |_| | | (_| |
  |____/| .__/ \___|\___|\__,_|_|\__,_|
        |_|   honest tiered-trust artifact cache
`

// shutdownTimeout bounds graceful HTTP server drain on SIGINT/SIGTERM.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("specula: fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, configPath, err := parseAndLoad()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	fmt.Fprint(os.Stdout, banner)
	log.Info("specula: starting", "config", configPath,
		"data_plane", cfg.Server.DataPlaneAddr, "control_plane", cfg.Server.ControlPlaneAddr)

	// Root context cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── Storage: CAS blob store + metadata store ────────────────────────────
	blobs, err := buildBlobStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build blob store: %w", err)
	}
	metaStore, closeMeta, err := buildMetaStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build metadata store: %w", err)
	}
	defer closeMeta()
	log.Info("specula: storage ready",
		"blob_driver", cfg.Storage.Blob.Driver, "meta_driver", cfg.Storage.Meta.Driver)

	// ── Verification chain: checksum (transport integrity) + TOFU pin ────────
	chain := verify.NewChain(
		verify.NewChecksumVerifier(),
		verify.NewTofuVerifier(newMetaTofuStore(metaStore)),
	)

	// ── Cache manager: two-tier CAS + verify-on-write quarantine ─────────────
	cm := cache.New(blobs, metaStore, chain)

	// ── Stats collector (metadata-backed, powers /metrics) ───────────────────
	collector := stats.NewCollectorWithStore(metaStore)
	_ = collector // exported via Prometheus default registry; refreshed by handlers/GC

	// ── Data plane: OCI handler + protocol stubs ─────────────────────────────
	dataMux := http.NewServeMux()
	mountOCI(dataMux, cfg, cm, metaStore, log)
	mountProtocolStubs(dataMux, log)
	// Liveness on the data plane too, so a bare data-plane LB can probe it.
	dataMux.HandleFunc("/healthz", healthz)

	// ── Control plane: health + readiness + metrics ──────────────────────────
	ctrlMux := http.NewServeMux()
	ctrlMux.HandleFunc("/healthz", healthz)
	ctrlMux.HandleFunc("/readyz", readyz(ctx, blobs, metaStore))
	ctrlMux.Handle("/metrics", promhttp.Handler())
	// TODO(control-plane): mount embedded WebUI + Admin API + auth (ARCHITECTURE §11).

	dataSrv := &http.Server{Addr: cfg.Server.DataPlaneAddr, Handler: dataMux, ReadHeaderTimeout: 15 * time.Second}
	ctrlSrv := &http.Server{Addr: cfg.Server.ControlPlaneAddr, Handler: ctrlMux, ReadHeaderTimeout: 15 * time.Second}

	return serve(ctx, log, dataSrv, ctrlSrv)
}

// parseAndLoad parses the --config flag and loads+validates the config.
func parseAndLoad() (*config.Config, string, error) {
	configPath := flag.String("config", "specula.yaml", "path to the Specula config file")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, *configPath, fmt.Errorf("load config %q: %w", *configPath, err)
	}
	return cfg, *configPath, nil
}

// buildBlobStore constructs the CAS blob store from config (local | s3).
func buildBlobStore(ctx context.Context, cfg *config.Config) (blobstore.BlobStore, error) {
	switch cfg.Storage.Blob.Driver {
	case "local":
		return local.NewLocalDiskDriver(cfg.Storage.Blob.Local.Root), nil
	case "s3":
		sc := cfg.Storage.Blob.S3
		// NOTE: only bucket wiring is supported by the current S3Driver
		// constructor; endpoint/region/static-credentials wiring is a TODO
		// that needs a richer constructor in internal/store/s3.
		return s3.NewS3Driver(ctx, sc.Bucket)
	default:
		return nil, fmt.Errorf("unknown blob driver %q (want \"local\" or \"s3\")", cfg.Storage.Blob.Driver)
	}
}

// buildMetaStore constructs the metadata store from config (sqlite | postgres).
// The returned close func is always non-nil.
func buildMetaStore(ctx context.Context, cfg *config.Config) (metastore.MetadataStore, func(), error) {
	switch cfg.Storage.Meta.Driver {
	case "sqlite":
		st, err := sqlite.NewSQLiteStore(cfg.Storage.Meta.DSN)
		if err != nil {
			return nil, func() {}, err
		}
		return st, func() { _ = st.Close() }, nil
	case "postgres":
		st, err := postgres.NewPostgresStore(ctx, cfg.Storage.Meta.DSN)
		if err != nil {
			return nil, func() {}, err
		}
		return st, st.Close, nil
	default:
		return nil, func() {}, fmt.Errorf("unknown meta driver %q (want \"sqlite\" or \"postgres\")", cfg.Storage.Meta.Driver)
	}
}

// mountOCI wires the OCI data-plane handler at /v2/ using the "oci" protocol
// config for upstreams and mutable TTL.
func mountOCI(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger) {
	pc, ok := cfg.Protocols["oci"]
	opts := []oci.Option{
		oci.WithMeta(metaStore),
		oci.WithLogger(log.With("protocol", "oci")),
	}
	if ok {
		opts = append(opts,
			oci.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)),
			oci.WithMutableTTL(mutableTTL(pc, cfg)),
		)
	}
	mux.Handle("/v2/", oci.NewHandler(cm, opts...))
	log.Info("specula: mounted OCI handler", "path", "/v2/", "configured", ok)
}

// mountProtocolStubs registers the remaining seven data-plane protocols as
// 501 stubs. v0.1 only serves OCI; these are placeholders (ARCHITECTURE §8).
func mountProtocolStubs(mux *http.ServeMux, log *slog.Logger) {
	stubs := map[string]string{
		"/pypi/":    "pypi",
		"/npm/":     "npm",
		"/go/":      "go",
		"/apt/":     "apt",
		"/helm/":    "helm",
		"/tarball/": "tarball",
		"/git/":     "git",
	}
	for path, name := range stubs {
		proto := name
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, fmt.Sprintf("protocol %q not yet implemented", proto), http.StatusNotImplemented)
		})
	}
	log.Info("specula: mounted protocol stubs (501)", "count", len(stubs))
}

// toUpstreams converts config upstream entries into the upstream package type.
func toUpstreams(in []config.UpstreamConfig) []upstream.Upstream {
	out := make([]upstream.Upstream, 0, len(in))
	for _, u := range in {
		out = append(out, upstream.Upstream{
			Name:     u.Name,
			BaseURL:  u.BaseURL,
			Priority: u.Priority,
			Official: u.Official,
		})
	}
	return out
}

// mutableTTL resolves the effective mutable TTL for a protocol, falling back to
// the global default when the protocol does not set its own.
func mutableTTL(pc config.ProtocolConfig, cfg *config.Config) int64 {
	if pc.MutableTTLSeconds != 0 {
		return pc.MutableTTLSeconds
	}
	return cfg.Cache.DefaultMutableTTLSeconds
}

// healthz is a liveness probe: the process is up and serving.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyz reports readiness by checking that both storage backends respond.
func readyz(_ context.Context, blobs blobstore.BlobStore, metaStore metastore.MetadataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := blobs.UsageBytes(ctx); err != nil {
			http.Error(w, "blob store not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if _, err := metaStore.CacheSizeByProtocol(ctx); err != nil {
			http.Error(w, "metadata store not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}

// serve runs both HTTP servers and shuts them down gracefully on ctx cancel.
func serve(ctx context.Context, log *slog.Logger, servers ...*http.Server) error {
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		s := srv
		go func() {
			log.Info("specula: listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("server %s: %w", s.Addr, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("specula: shutdown signal received, draining")
	case err := <-errCh:
		log.Error("specula: server error, shutting down", "err", err)
		shutdownAll(log, servers)
		return err
	}

	shutdownAll(log, servers)
	return nil
}

// shutdownAll gracefully drains every server with a bounded timeout.
func shutdownAll(log *slog.Logger, servers []*http.Server) {
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("specula: forced close", "addr", srv.Addr, "err", err)
			_ = srv.Close()
		}
	}
}

// metaTofuStore adapts a MetadataStore into the narrow verify.TofuStore
// (GetPin/SetPin) using the mutable tier with a "tofu:" key namespace and a
// never-revalidate TTL so pins are permanent.
type metaTofuStore struct {
	meta metastore.MetadataStore
}

func newMetaTofuStore(m metastore.MetadataStore) verify.TofuStore {
	return &metaTofuStore{meta: m}
}

func (s *metaTofuStore) GetPin(ctx context.Context, key string) (string, error) {
	e, err := s.meta.GetMutable(ctx, tofuKey(key))
	if err != nil {
		return "", err
	}
	if e == nil {
		return "", nil
	}
	return e.Digest, nil
}

func (s *metaTofuStore) SetPin(ctx context.Context, key, digest string) error {
	return s.meta.PutMutable(ctx, artifact.MutableEntry{
		Key:        tofuKey(key),
		Protocol:   "tofu",
		Digest:     digest,
		TTLSeconds: config.TTLNeverRevalidate,
		FetchedAt:  time.Now(),
	})
}

func tofuKey(key string) string { return "tofu:" + key }
