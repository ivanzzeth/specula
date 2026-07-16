// Command specula is the Specula daemon entrypoint. It loads configuration,
// constructs the selected CAS BlobStore + MetadataStore, builds the streaming
// verification chain (checksum + TOFU) and CacheManager, then serves the OCI
// data plane on its port and the control-plane health/metrics endpoints.
//
// v0.2 scope: all eight data-plane protocol handlers serve for real — OCI,
// Go module (GOPROXY), pypi, npm, apt, helm, tarball and git. Protocol-native
// signed anchors are wired where they exist (apt GPG keyring, Helm .prov GPG,
// git signed refs); pypi/npm/tarball land on TOFU in this batch.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ivanzzeth/specula/internal/admin"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/config"
	apthandler "github.com/ivanzzeth/specula/internal/handler/apt"
	githandler "github.com/ivanzzeth/specula/internal/handler/git"
	"github.com/ivanzzeth/specula/internal/handler/gomod"
	helmhandler "github.com/ivanzzeth/specula/internal/handler/helm"
	"github.com/ivanzzeth/specula/internal/handler/npm"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/handler/pypi"
	tarballhandler "github.com/ivanzzeth/specula/internal/handler/tarball"
	"github.com/ivanzzeth/specula/internal/stats"
	blobstore "github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/local"
	metastore "github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/postgres"
	"github.com/ivanzzeth/specula/internal/store/s3"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
	webui "github.com/ivanzzeth/specula/web"
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

	// ── Protocol-native signed verifiers ────────────────────────────────────
	// Each is built from config paths and self-gates by ref.Protocol (like the Go
	// sumdb verifier), so a single shared chain carries all of them without one
	// protocol's verifier acting on another's artifacts. A missing/unreadable
	// keyring downgrades that protocol's ceiling to tofu and warns — never crashes.
	gpgV := buildGPGVerifier(cfg, log)           // apt  → signed (GPG keyring)
	helmProvV := buildHelmProvVerifier(cfg, log) // helm → signed (.prov GPG)
	gitSignedV := buildGitSignedVerifier(cfg, log)

	// ── Verification chain: checksum (transport integrity) + TOFU pin ────────
	// The chain is global; every verifier self-gates (no-op StatusPass for any ref
	// outside its protocol) so a single shared chain can carry them all without
	// acting on protocols they do not own (DESIGN-REVIEW §1 H5).
	verifiers := []verify.Verifier{
		verify.NewChecksumVerifier(),
		verify.NewTofuVerifier(newMetaTofuStore(metaStore)),
	}
	if goSumDB := buildGoSumDBVerifier(cfg, metaStore, log); goSumDB != nil {
		verifiers = append(verifiers, goSumDB)
	}
	if gpgV != nil {
		verifiers = append(verifiers, gpgV)
	}
	if helmProvV != nil {
		verifiers = append(verifiers, helmProvV)
	}
	if gitSignedV != nil {
		verifiers = append(verifiers, gitSignedV)
	}
	chain := verify.NewChain(verifiers...)

	// ── Cache manager: two-tier CAS + verify-on-write quarantine ─────────────
	cm := cache.New(blobs, metaStore, chain)

	// ── Stats collector (metadata-backed, powers /metrics + Admin API) ───────
	collector := stats.NewCollectorWithStore(metaStore)
	// Background refresh loop: periodically re-aggregates per-protocol usage into
	// Prometheus gauges. Stops when ctx is cancelled (SIGINT/SIGTERM).
	go collector.Run(ctx)

	// ── Data plane: all eight protocol handlers ──────────────────────────────
	dataMux := http.NewServeMux()
	mountOCI(dataMux, cfg, cm, metaStore, log)
	mountGoModule(dataMux, cfg, cm, metaStore, log)
	mountPyPI(dataMux, cfg, cm, metaStore, log)
	mountNPM(dataMux, cfg, cm, metaStore, log)
	mountAPT(dataMux, cfg, cm, metaStore, log, gpgV)
	mountHelm(dataMux, cfg, cm, metaStore, log, helmProvV)
	mountTarball(dataMux, cfg, cm, metaStore, log)
	mountGit(dataMux, cfg, metaStore, log, gitSignedV)
	// Liveness on the data plane too, so a bare data-plane LB can probe it.
	dataMux.HandleFunc("/healthz", healthz)

	// ── Control plane: health + readiness + metrics + Admin API ──────────────
	ctrlMux := http.NewServeMux()
	ctrlMux.HandleFunc("/healthz", healthz)
	ctrlMux.HandleFunc("/readyz", readyz(ctx, blobs, metaStore))
	ctrlMux.Handle("/metrics", promhttp.Handler())

	// Admin API (ARCHITECTURE §11): the concrete metadata store also implements
	// the control-plane UserStore (shared users table), so a single value backs
	// both the cache metadata and account management.
	userStore, ok := metaStore.(auth.UserStore)
	if !ok {
		return fmt.Errorf("meta store %T does not implement auth.UserStore", metaStore)
	}
	tokens := auth.NewHS256Verifier(resolveJWTSecret(cfg, log))
	authSvc := auth.NewService(userStore, auth.NewBcryptHasher(), tokens, cfg.Auth.CookieSecure)
	adminSrv := admin.New(admin.Deps{
		Stats:  collector,
		Meta:   metaStore,
		Users:  userStore,
		Auth:   authSvc,
		Tokens: tokens,
		Config: cfg,
		Blobs:  blobs,
		Secure: cfg.Auth.CookieSecure,
		Logger: log.With("component", "admin"),
	})
	adminSrv.RegisterRoutes(ctrlMux)
	log.Info("specula: mounted Admin API", "base", "/api/v1")

	// Embedded WebUI SPA (ARCHITECTURE §11): the "/" catch-all serves the Vite
	// build output; hashed assets get an immutable long cache, index.html is
	// no-cache for SPA route fallback. devMode surfaces dev-only UI when
	// APP_ENV==dev. Registered last so the more-specific /api, /healthz, /readyz,
	// /metrics patterns win under ServeMux longest-prefix matching.
	devMode := os.Getenv("APP_ENV") == "dev"
	ctrlMux.Handle("/", webui.Handler(devMode))
	log.Info("specula: mounted embedded WebUI", "path", "/", "dev_mode", devMode)

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
		return s3.NewS3Driver(ctx, s3.S3Config{
			Bucket:          sc.Bucket,
			Endpoint:        sc.Endpoint,
			Region:          sc.Region,
			AccessKeyID:     sc.AccessKeyID,
			SecretAccessKey: sc.SecretAccessKey,
			UsePathStyle:    sc.UsePathStyle,
		})
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

// goProtocolKey is the config.Protocols map key for the Go module proxy. Note
// the config keys this block "go" while the on-the-wire ArtifactRef.Protocol and
// store rows use "gomod" (see gomod.Protocol / verify.protocolGo).
const goProtocolKey = "go"

// goMountPrefix is the data-plane mount path for the GOPROXY handler. Users set
// GOPROXY=http://<host>/go so the go command appends /{module}/@v/... beneath it,
// and derives sumdb requests at /go/sumdb/{name}/... (routed internally).
const goMountPrefix = "/go"

// mountGoModule wires the GOPROXY data-plane handler at /go/ using the "go"
// protocol config for upstreams, mutable TTL and the /sumdb/ passthrough. The
// handler self-strips the /go prefix (WithPathPrefix) so it can be mounted with
// a bare ServeMux pattern. When the "go" protocol is absent the handler still
// mounts (serving already-cached content) but has no upstream fallback.
func mountGoModule(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger) {
	pc, ok := cfg.Protocols[goProtocolKey]
	opts := []gomod.Option{
		gomod.WithMeta(metaStore),
		gomod.WithPathPrefix(goMountPrefix),
		gomod.WithLogger(log.With("protocol", gomod.Protocol)),
	}

	sumdbEnabled := false
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, gomod.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		opts = append(opts, gomod.WithMutableTTL(mutableTTL(pc, cfg)))

		// /sumdb/ passthrough: transparently proxy the go command's own signed
		// tree-head + inclusion/consistency verification to the configured sumdb.
		// Shares the GONOSUMDB private-module matcher with the chain verifier so a
		// private module name is never forwarded to the public sumdb (H5).
		if pc.SumDB != nil {
			sh := gomod.NewSumDBHandler(pc.SumDB.URL,
				gomod.WithSumDBPrivateMatcher(verify.NewPrivateMatcher(pc.SumDB.PrivatePatterns)),
				gomod.WithSumDBLogger(log.With("protocol", gomod.Protocol, "component", "sumdb")),
			)
			opts = append(opts, gomod.WithSumDB(sh))
			sumdbEnabled = true
		}
	}

	mux.Handle(goMountPrefix+"/", gomod.NewHandler(cm, opts...))
	log.Info("specula: mounted Go module proxy",
		"path", goMountPrefix+"/", "configured", ok, "sumdb_passthrough", sumdbEnabled)
}

// buildGoSumDBVerifier constructs the Go checksum-database verifier from the "go"
// protocol's sumdb block, or returns nil when the go protocol has no sumdb
// config. Anti-rollback high-water tree size is persisted via the metadata store.
func buildGoSumDBVerifier(cfg *config.Config, metaStore metastore.MetadataStore, log *slog.Logger) *verify.SumDBVerifier {
	pc, ok := cfg.Protocols[goProtocolKey]
	if !ok || pc.SumDB == nil {
		return nil
	}
	sc := pc.SumDB
	log.Info("specula: Go sumdb verification enabled",
		"url", sc.URL, "policy", policyOrDefault(sc.Policy), "private_patterns", len(sc.PrivatePatterns))
	return verify.NewSumDBVerifier(verify.SumDBConfig{
		URL:             sc.URL,
		VerifierKey:     sc.VerifierKey,
		Policy:          verify.Policy(sc.Policy),
		PrivatePatterns: sc.PrivatePatterns,
		TreeSize:        newMetaTreeSizeStore(metaStore),
	})
}

// policyOrDefault renders the effective sumdb policy for logging ("" → enforce).
func policyOrDefault(p string) string {
	if p == "" {
		return string(verify.PolicyEnforce)
	}
	return p
}

// mountPrefix is the data-plane path prefix each non-OCI protocol is mounted on.
// The handler self-strips it (WithPathPrefix) so it can route against a bare
// ServeMux pattern (mux.Handle(prefix+"/", …)).
const (
	pypiPrefix    = "/pypi"
	npmPrefix     = "/npm"
	aptPrefix     = "/apt"
	helmPrefix    = "/helm"
	tarballPrefix = "/tarball"
	gitPrefix     = "/git"
)

// mountPyPI wires the PyPI handler at /pypi/ using the "pypi" protocol config for
// upstreams, mutable TTL and the optional dependency-confusion private guard.
// pypi tops out at TOFU in this batch (no protocol-native signed anchor).
func mountPyPI(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger) {
	pc, ok := cfg.Protocols["pypi"]
	l := log.With("protocol", pypi.Protocol)
	opts := []pypi.Option{
		pypi.WithMeta(metaStore),
		pypi.WithPathPrefix(pypiPrefix),
		pypi.WithLogger(l),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, pypi.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		opts = append(opts, pypi.WithMutableTTL(mutableTTL(pc, cfg)))
		if dc := pc.Verification.DependencyConfusion; dc != nil && dc.PrivateUpstream != "" {
			opts = append(opts,
				pypi.WithPrivateNames(dc.PrivateNames),
				pypi.WithPrivateUpstream(privateUpstream(dc.PrivateUpstream)),
				pypi.WithFailClosed(dc.OnPrivateDown != "serve_stale"),
			)
			l.Info("specula: pypi dependency-confusion guard enabled",
				"private_names", len(dc.PrivateNames), "on_private_down", onPrivateDownOrDefault(dc.OnPrivateDown))
		}
	}
	mux.Handle(pypiPrefix+"/", pypi.NewHandler(cm, opts...))
	log.Info("specula: mounted PyPI handler", "path", pypiPrefix+"/", "configured", ok)
}

// mountNPM wires the npm handler at /npm/ using the "npm" protocol config for
// upstreams, mutable TTL and the optional dependency-confusion private guard.
// npm tops out at TOFU in this batch (scoped names are confusion-resistant).
func mountNPM(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger) {
	pc, ok := cfg.Protocols["npm"]
	l := log.With("protocol", npm.Protocol)
	opts := []npm.Option{
		npm.WithMeta(metaStore),
		npm.WithPathPrefix(npmPrefix),
		npm.WithLogger(l),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, npm.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		opts = append(opts, npm.WithMutableTTL(mutableTTL(pc, cfg)))
		if dc := pc.Verification.DependencyConfusion; dc != nil && dc.PrivateUpstream != "" {
			opts = append(opts,
				npm.WithPrivateScopes(dc.PrivateScopes),
				npm.WithPrivateUnscoped(dc.PrivateUnscoped),
				npm.WithPrivateUpstream(privateUpstream(dc.PrivateUpstream)),
				npm.WithFailClosed(dc.OnPrivateDown != "serve_stale"),
			)
			l.Info("specula: npm dependency-confusion guard enabled",
				"private_scopes", len(dc.PrivateScopes), "private_unscoped", len(dc.PrivateUnscoped),
				"on_private_down", onPrivateDownOrDefault(dc.OnPrivateDown))
		}
	}
	mux.Handle(npmPrefix+"/", npm.NewHandler(cm, opts...))
	log.Info("specula: mounted npm handler", "path", npmPrefix+"/", "configured", ok)
}

// mountAPT wires the apt handler at /apt/ using the "apt" protocol config. The
// GPG chain verifier (apt's signed anchor) is passed in when configured; the same
// instance is already registered in the shared verify chain so verify-on-write and
// the handler share one stateful anchor. Without it, apt tops out at tofu.
func mountAPT(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, gpgV *verify.GPGVerifier) {
	pc, ok := cfg.Protocols["apt"]
	opts := []apthandler.Option{
		apthandler.WithMeta(metaStore),
		apthandler.WithPathPrefix(aptPrefix),
		apthandler.WithLogger(log.With("protocol", apthandler.Protocol)),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, apthandler.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		opts = append(opts, apthandler.WithMutableTTL(mutableTTL(pc, cfg)))
	}
	if gpgV != nil {
		opts = append(opts, apthandler.WithGPGVerifier(gpgV))
	}
	mux.Handle(aptPrefix+"/", apthandler.NewHandler(cm, opts...))
	log.Info("specula: mounted APT handler",
		"path", aptPrefix+"/", "configured", ok, "signed", gpgV != nil)
}

// mountHelm wires the classic-HTTP Helm handler at /helm/ using the "helm"
// protocol config. The .prov GPG verifier (helm's signed anchor) is passed in
// when configured; the same instance is registered in the shared chain. Without
// it, helm tops out at tofu.
func mountHelm(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, provV *verify.HelmProvVerifier) {
	pc, ok := cfg.Protocols["helm"]
	opts := []helmhandler.Option{
		helmhandler.WithMeta(metaStore),
		helmhandler.WithPathPrefix(helmPrefix),
		helmhandler.WithLogger(log.With("protocol", helmhandler.Protocol)),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, helmhandler.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		opts = append(opts, helmhandler.WithMutableTTL(mutableTTL(pc, cfg)))
	}
	if provV != nil {
		opts = append(opts, helmhandler.WithProvenanceVerifier(provV))
	}
	mux.Handle(helmPrefix+"/", helmhandler.NewHandler(cm, opts...))
	log.Info("specula: mounted Helm handler",
		"path", helmPrefix+"/", "configured", ok, "signed", provV != nil)
}

// mountTarball wires the generic URL-keyed tarball handler at /tarball/ using the
// "tarball" protocol config. The allowed-host allowlist is derived from the
// configured upstream base URLs (a request host outside the list is rejected).
// tarball tops out at TOFU (immutable digest pin) in this batch.
func mountTarball(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger) {
	pc, ok := cfg.Protocols["tarball"]
	opts := []tarballhandler.Option{
		tarballhandler.WithMeta(metaStore),
		tarballhandler.WithPathPrefix(tarballPrefix),
		tarballhandler.WithLogger(log.With("protocol", tarballhandler.Protocol)),
	}
	hosts := []string{}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, tarballhandler.WithUpstream(upstream.NewClient(), toUpstreams(pc.Upstreams)))
		}
		hosts = upstreamHosts(pc.Upstreams)
		opts = append(opts, tarballhandler.WithAllowedHosts(hosts))
	}
	mux.Handle(tarballPrefix+"/", tarballhandler.NewHandler(cm, opts...))
	log.Info("specula: mounted tarball handler",
		"path", tarballPrefix+"/", "configured", ok, "allowed_hosts", len(hosts))
}

// mountGit wires the git-clone acceleration handler at /git/ using the "git"
// protocol config. Unlike the CAS-backed handlers git has no CacheManager: its
// byte cache is the on-disk bare mirror. The host allowlist and mirror settings
// come from the git-specific config block; the signed-refs verifier lifts a ref
// to the signed tier when an allowed-signers file is configured.
func mountGit(mux *http.ServeMux, cfg *config.Config, metaStore metastore.MetadataStore, log *slog.Logger, signedV *verify.GitSignedVerifier) {
	pc, ok := cfg.Protocols["git"]
	opts := []githandler.Option{
		githandler.WithMeta(metaStore),
		githandler.WithPathPrefix(gitPrefix),
		githandler.WithLogger(log.With("protocol", githandler.Protocol)),
	}
	if ok {
		opts = append(opts, githandler.WithMutableTTL(mutableTTL(pc, cfg)))
		if gc := pc.Git; gc != nil {
			opts = append(opts,
				githandler.WithMirrorDir(gc.MirrorDir),
				githandler.WithAllowedUpstreams(gc.AllowedUpstreams),
				githandler.WithPublicOnly(gc.PublicOnly),
				githandler.WithFailClosed(gc.FailClosed),
			)
			if d, err := time.ParseDuration(gc.SyncStaleAfter); err == nil && gc.SyncStaleAfter != "" {
				opts = append(opts, githandler.WithSyncStaleAfter(d))
			}
		}
	}
	if signedV != nil {
		opts = append(opts, githandler.WithSignedRefsVerifier(signedV))
	}
	mux.Handle(gitPrefix+"/", githandler.NewHandler(opts...))
	log.Info("specula: mounted git handler",
		"path", gitPrefix+"/", "configured", ok, "signed", signedV != nil)
}

// buildGPGVerifier constructs the apt InRelease→Packages→.deb GPG chain verifier
// from the "apt" protocol's verification.gpg.keyring path. Returns nil (and warns)
// when the keyring is unset or unreadable so apt downgrades to tofu without crashing.
func buildGPGVerifier(cfg *config.Config, log *slog.Logger) *verify.GPGVerifier {
	pc, ok := cfg.Protocols["apt"]
	if !ok || pc.Verification.GPG == nil || strings.TrimSpace(pc.Verification.GPG.Keyring) == "" {
		log.Warn("specula: apt GPG keyring not configured — apt tops out at tofu tier")
		return nil
	}
	keyring := pc.Verification.GPG.Keyring
	v, err := verify.NewGPGVerifier(keyring)
	if err != nil {
		log.Warn("specula: apt GPG verifier disabled (keyring load failed) — downgrading apt to tofu",
			"keyring", keyring, "err", err)
		return nil
	}
	log.Info("specula: apt GPG signed verification enabled",
		"keyring", keyring, "policy", policyOrWarn(pc.Verification.GPG.Policy))
	return v
}

// buildHelmProvVerifier constructs the Helm .prov detached-GPG verifier from the
// "helm" protocol's verification.provenance.keyring path. Returns nil (and warns)
// when unset or unreadable so helm downgrades to tofu without crashing.
func buildHelmProvVerifier(cfg *config.Config, log *slog.Logger) *verify.HelmProvVerifier {
	pc, ok := cfg.Protocols["helm"]
	if !ok || pc.Verification.Provenance == nil || strings.TrimSpace(pc.Verification.Provenance.Keyring) == "" {
		log.Warn("specula: helm provenance keyring not configured — helm tops out at tofu tier")
		return nil
	}
	keyring := pc.Verification.Provenance.Keyring
	v, err := verify.NewHelmProvVerifier(keyring)
	if err != nil {
		log.Warn("specula: helm provenance verifier disabled (keyring load failed) — downgrading helm to tofu",
			"keyring", keyring, "err", err)
		return nil
	}
	log.Info("specula: helm provenance signed verification enabled",
		"keyring", keyring, "policy", policyOrWarn(pc.Verification.Provenance.Policy))
	return v
}

// buildGitSignedVerifier constructs the git signed tag/commit verifier from the
// "git" protocol's verification.signed_refs.allowed_signers path. Returns nil (and
// warns) when unset or unreadable so git stays at tofu without crashing.
func buildGitSignedVerifier(cfg *config.Config, log *slog.Logger) *verify.GitSignedVerifier {
	pc, ok := cfg.Protocols["git"]
	if !ok || pc.Verification.SignedRefs == nil || strings.TrimSpace(pc.Verification.SignedRefs.AllowedSigners) == "" {
		log.Warn("specula: git allowed-signers not configured — git tops out at tofu tier")
		return nil
	}
	signers := pc.Verification.SignedRefs.AllowedSigners
	v, err := verify.NewGitSignedVerifier(signers)
	if err != nil {
		log.Warn("specula: git signed-refs verifier disabled (allowed-signers load failed) — downgrading git to tofu",
			"allowed_signers", signers, "err", err)
		return nil
	}
	log.Info("specula: git signed-refs verification enabled",
		"allowed_signers", signers, "policy", policyOrWarn(pc.Verification.SignedRefs.Policy))
	return v
}

// policyOrWarn renders an effective enforce/warn policy for logging ("" → warn).
// gpg/provenance/signed_refs blocks default to warn (degrade rather than fail).
func policyOrWarn(p string) string {
	if p == "" {
		return "warn"
	}
	return p
}

// onPrivateDownOrDefault renders the effective dependency-confusion fail behaviour
// for logging ("" → fail_closed).
func onPrivateDownOrDefault(v string) string {
	if v == "" {
		return "fail_closed"
	}
	return v
}

// privateUpstream builds a synthetic upstream.Upstream for a dependency-confusion
// private index/registry base URL. Marked Official since it is the authoritative
// source for the private names it owns.
func privateUpstream(baseURL string) upstream.Upstream {
	return upstream.Upstream{Name: "private", BaseURL: baseURL, Priority: 0, Official: true}
}

// upstreamHosts extracts the distinct hostnames from a list of upstream base URLs,
// used to build the tarball handler's fetch host allowlist. Unparseable entries
// are skipped.
func upstreamHosts(ups []config.UpstreamConfig) []string {
	seen := make(map[string]struct{}, len(ups))
	out := make([]string, 0, len(ups))
	for _, u := range ups {
		parsed, err := url.Parse(u.BaseURL)
		if err != nil || parsed.Host == "" {
			continue
		}
		host := parsed.Hostname()
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
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

// resolveJWTSecret returns the configured HS256 session-signing secret. When
// auth.jwt_secret is empty it generates a random 32-byte ephemeral secret and
// warns loudly: sessions will not survive a restart and cannot be shared across
// HA replicas until a stable secret is configured (ARCHITECTURE §11 ensureSecret).
func resolveJWTSecret(cfg *config.Config, log *slog.Logger) []byte {
	if cfg.Auth.JWTSecret != "" {
		return []byte(cfg.Auth.JWTSecret)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is fatal-adjacent; fall back to a fixed marker so
		// the process still starts but every session is clearly non-persistent.
		log.Error("specula: crypto/rand failed generating JWT secret", "err", err)
		return []byte("specula-insecure-ephemeral-fallback-secret")
	}
	log.Warn("specula: auth.jwt_secret is empty — generated an EPHEMERAL secret; " +
		"sessions will be invalidated on restart and are not valid across replicas. " +
		"Set auth.jwt_secret (or SPECULA_AUTH__JWT_SECRET) for production.")
	return []byte(hex.EncodeToString(buf))
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

// metaTreeSizeStore adapts a MetadataStore into the verify.TreeSizeStore used for
// Go sumdb anti-rollback: it persists the monotonic high-water signed tree size
// per sumdb name in the mutable tier under a "sumdb-treesize:" key namespace with
// a never-revalidate TTL. The size is stored as a decimal string in the Digest
// field (the mutable pointer's opaque value slot).
type metaTreeSizeStore struct {
	meta metastore.MetadataStore
}

func newMetaTreeSizeStore(m metastore.MetadataStore) verify.TreeSizeStore {
	return &metaTreeSizeStore{meta: m}
}

func (s *metaTreeSizeStore) GetTreeSize(ctx context.Context, name string) (int64, error) {
	e, err := s.meta.GetMutable(ctx, treeSizeKey(name))
	if err != nil {
		return 0, err
	}
	if e == nil || e.Digest == "" {
		return 0, nil
	}
	n, parseErr := strconv.ParseInt(e.Digest, 10, 64)
	if parseErr != nil {
		// Corrupt persisted value: treat as "no record" rather than failing the
		// whole verification. The next successful head write repairs it.
		return 0, nil
	}
	return n, nil
}

func (s *metaTreeSizeStore) SetTreeSize(ctx context.Context, name string, size int64) error {
	return s.meta.PutMutable(ctx, artifact.MutableEntry{
		Key:        treeSizeKey(name),
		Protocol:   "sumdb",
		Digest:     strconv.FormatInt(size, 10),
		TTLSeconds: config.TTLNeverRevalidate,
		FetchedAt:  time.Now().UTC(),
	})
}

func treeSizeKey(name string) string { return "sumdb-treesize:" + name }
