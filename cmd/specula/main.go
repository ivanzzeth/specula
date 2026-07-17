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
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ivanzzeth/specula/internal/admin"
	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/grant"
	apthandler "github.com/ivanzzeth/specula/internal/handler/apt"
	githandler "github.com/ivanzzeth/specula/internal/handler/git"
	"github.com/ivanzzeth/specula/internal/handler/gomod"
	helmhandler "github.com/ivanzzeth/specula/internal/handler/helm"
	"github.com/ivanzzeth/specula/internal/handler/npm"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/handler/pypi"
	"github.com/ivanzzeth/specula/internal/handler/registry"
	tarballhandler "github.com/ivanzzeth/specula/internal/handler/tarball"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/registryauthz"
	"github.com/ivanzzeth/specula/internal/registrytoken"
	"github.com/ivanzzeth/specula/internal/repo"
	"github.com/ivanzzeth/specula/internal/settings"
	"github.com/ivanzzeth/specula/internal/stats"
	"github.com/ivanzzeth/specula/internal/store/aptpins"
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

	// ── Multi-tenant kernel (R1): org / apikey / grant stores ────────────────
	// Constructed on the same database as the metadata store. For sqlite the
	// 0002_multitenant migration already ran inside NewSQLiteStore; for postgres
	// the embedded migrations are applied here against a stdlib handle. The org
	// store also drives the first-user bootstrap (default org + owner) via the
	// auth service below.
	orgStore, keyStore, grantStore, repoStore, mtDB, closeMT, err := buildMultiTenantStores(cfg, metaStore)
	if err != nil {
		return fmt.Errorf("build multi-tenant stores: %w", err)
	}
	defer closeMT()
	log.Info("specula: multi-tenant kernel ready", "meta_driver", cfg.Storage.Meta.Driver)

	// ── Runtime settings: encrypted config store + resolver ──────────────────
	// The config file/env is the bootstrap default; the encrypted store
	// (system_config, same database) holds runtime overrides, resolution order
	// override>bootstrap. This is what lets an auto-generated secret survive a
	// restart AND be shared by every HA replica, and what makes org.max_per_user
	// changeable at runtime.
	settings.WarnRemovedEnv(log)
	cfgStore, err := buildConfigStore(cfg, mtDB, log)
	if err != nil {
		return fmt.Errorf("build encrypted settings store: %w", err)
	}
	regKeyPath := resolveRegistryKeyPath(cfg)
	resolver := buildSettingsResolver(cfg, cfgStore, readRegistryKeyPEM(regKeyPath))

	// ── Writable hosted registry (R2): RS256 token service + authz glue ──────
	// The registry token service signs/verifies the Docker v2 Bearer tokens; the
	// authz glue binds acl.CanAccess + org membership + hosted repo ownership to
	// the /token, /v2/ write, and hosted-first pull seams. A key-load failure
	// downgrades /v2/ to pull-through-only (registryEnabled=false) rather than
	// crashing, so the cache keeps serving reads.
	regTokenSvc, registryEnabled := buildRegistryTokenService(ctx, resolver, regKeyPath, log)
	regAuthz := registryauthz.New(orgStore, repoStore)

	// ── Protocol-native signed verifiers ────────────────────────────────────
	// Each is built from config paths and self-gates by ref.Protocol (like the Go
	// sumdb verifier), so a single shared chain carries all of them without one
	// protocol's verifier acting on another's artifacts. A missing/unreadable
	// keyring downgrades that protocol's ceiling to tofu and warns — never crashes.
	gpgV := buildGPGVerifier(cfg, mtDB, log)     // apt  → signed (GPG keyring + persisted pins)
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

	// ── Cross-source consensus (TierConsensus, DESIGN-REVIEW §1.2) ────────────
	// Each configured protocol gets its OWN protocol-scoped consensus verifier so
	// one protocol's mirror set never acts on another's artifacts (the shared
	// chain would otherwise let it). The digest fetcher is metadata-only (HEAD /
	// index page, never the blob) and returns sha256 — so consensus is wired for
	// the ecosystems whose metadata publishes a sha256 (oci, pypi). npm (sha512
	// integrity) and tarball (no advertised digest) cannot be cross-checked
	// metadata-only, so their consensus request is logged and left at tofu rather
	// than fail-closing every real fetch.
	mirrorFetcher := verify.NewHTTPMirrorDigestFetcher(0)
	for _, protocol := range []string{"oci", "pypi", "npm", "tarball"} {
		cv, err := buildConsensusVerifier(protocol, cfg, mirrorFetcher, log)
		if err != nil {
			// Unsatisfiable consensus config: refuse to start rather than serve
			// 502s forever from a tier that can never pass (BUG C).
			return fmt.Errorf("build consensus verifier: %w", err)
		}
		if cv != nil {
			verifiers = append(verifiers, cv)
		}
	}

	// ── cosign keyed signed anchor for OCI (TierSigned, DESIGN-REVIEW §1.1) ────
	// Registered only when the oci protocol configures cosign public keys. The
	// verifier self-gates to resolved oci images; discovery uses go-container-
	// registry (the sha256-<hex>.sig companion tag), transparency log disabled.
	if cosignV := buildCosignVerifier(cfg, log); cosignV != nil {
		verifiers = append(verifiers, cosignV)
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
	//
	// upstreams is the shared per-protocol upstream Runtime registry. Each
	// protocol handler's upstream client is bound to its own Runtime, which both
	// records what actually happened on the wire (latency, serve counts,
	// auto-block state, last error) and applies the operator's runtime overrides
	// (enable/disable, fallback reorder). The same registry is handed to the
	// Admin API below, so /api/v1/admin/upstreams reports live state rather than
	// a config echo — and an operator's change there takes effect on the very
	// next fetch.
	upstreams := upstream.NewRegistry()

	dataMux := http.NewServeMux()
	mountOCI(dataMux, cfg, cm, metaStore, log, upstreams, regTokenSvc, regAuthz, repoStore, blobs, registryEnabled)
	mountGoModule(dataMux, cfg, cm, metaStore, log, upstreams)
	mountPyPI(dataMux, cfg, cm, metaStore, log, upstreams)
	mountNPM(dataMux, cfg, cm, metaStore, log, upstreams)
	mountAPT(dataMux, cfg, cm, metaStore, log, upstreams, gpgV)
	mountHelm(dataMux, cfg, cm, metaStore, log, upstreams, helmProvV)
	mountTarball(dataMux, cfg, cm, metaStore, log, upstreams)
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
	jwtSecret, err := ensureJWTSecret(ctx, resolver, log)
	if err != nil {
		return fmt.Errorf("resolve session signing secret: %w", err)
	}
	tokens := auth.NewHS256Verifier(jwtSecret)
	// Passing orgStore enables the first-user-admin bootstrap to also seed the
	// default org and make the first user its owner (auth.Service.Register).
	authSvc := auth.NewService(userStore, auth.NewBcryptHasher(), tokens, cfg.Auth.CookieSecure, orgStore)
	adminSrv := admin.New(admin.Deps{
		Stats:      collector,
		Meta:       metaStore,
		Users:      userStore,
		Auth:       authSvc,
		Tokens:     tokens,
		Config:     cfg,
		Blobs:      blobs,
		Secure:     cfg.Auth.CookieSecure,
		Logger:     log.With("component", "admin"),
		OrgStore:   orgStore,
		KeyStore:   keyStore,
		GrantStore: grantStore,
		RepoStore:  repoStore,
		TagStore:   repoStore,
		Upstreams:  upstreams,
		Settings:   resolver,
	})
	adminSrv.RegisterRoutes(ctrlMux)
	log.Info("specula: mounted Admin API", "base", "/api/v1")

	// ── Registry /token endpoint (Docker v2 Bearer flow) ─────────────────────
	// Authenticates Basic credentials (email:apikey via keyStore, or
	// email:password via the auth service) and mints RS256 tokens scoped by the
	// same authz glue used at /v2/. Mounted on BOTH planes: the control plane
	// (per the two-plane split) and the data plane, so a docker/crane client
	// that only reaches the data-plane host can follow the same-origin realm.
	if registryEnabled {
		authn := &registrytoken.BasicAuthenticator{
			Keys:      keyStore,
			Passwords: &registryauthz.PasswordAuth{Svc: authSvc},
		}
		tokenHandler := registrytoken.NewTokenHandler(regTokenSvc, authn, regAuthz)
		ctrlMux.Handle("/token", tokenHandler)
		dataMux.Handle("/token", tokenHandler)
		log.Info("specula: mounted registry token endpoint", "path", "/token", "planes", "control+data")
	}

	// Machine-facing prefixes must never fall through to the SPA catch-all below.
	// Both of these were real, observed failures:
	//
	//   /api/**  — the admin routes are individual patterns on this same mux, so a
	//     typo'd endpoint answered an API client with 200 + index.html.
	//   /v2/**   — the registry lives on the DATA plane. Served from here the SPA
	//     answered GET /v2/ with 200, which `docker login` reads as "registry
	//     reachable, no auth required": it printed "Login Succeeded" against the
	//     control-plane port with an entirely bogus password, and only the later
	//     push failed, confusingly.
	//
	// ServeMux longest-pattern-wins keeps every real route ahead of these guards.
	jsonNotFound := func(msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":`+strconv.Quote(msg)+"}\n")
		}
	}
	ctrlMux.HandleFunc("/api/", jsonNotFound("no such API endpoint"))
	ctrlMux.HandleFunc("/v2/", jsonNotFound(
		"the OCI registry is served on the data plane, not the control plane"))

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

// buildMultiTenantStores constructs the R1 org / apikey / grant stores on the
// same database backing the metadata store. The returned close func is always
// non-nil.
//
//   - sqlite   — reuses the *sql.DB already opened + migrated by NewSQLiteStore
//     (the 0002_multitenant migration is embedded and auto-applied there), so no
//     second connection or migration pass is needed; closeMT is a no-op.
//   - postgres — opens a stdlib *sql.DB against the same DSN and applies the
//     embedded goose migrations before constructing the stores; closeMT closes
//     that handle. The postgres constructors (NewSQLStorePostgres) rebind the
//     ported "?"-placeholder SQL to "$N" for the pgx stdlib driver.
//
// The returned *sql.DB is the same handle the multi-tenant stores use; the
// encrypted settings store (internal/configstore) is built on it too, so runtime
// settings live in the same database and are therefore shared by every replica.
func buildMultiTenantStores(cfg *config.Config, metaStore metastore.MetadataStore) (org.Store, apikey.Store, grant.Store, *repo.SQLStore, *sql.DB, func(), error) {
	switch cfg.Storage.Meta.Driver {
	case "sqlite":
		st, ok := metaStore.(*sqlite.SQLiteStore)
		if !ok {
			return nil, nil, nil, nil, nil, func() {}, fmt.Errorf("sqlite meta store has unexpected type %T", metaStore)
		}
		db := st.DB()
		return org.NewSQLStore(db), apikey.NewSQLStore(db), grant.NewSQLStore(db), repo.NewSQLStore(db), db, func() {}, nil
	case "postgres":
		db, err := postgres.OpenSQLDB(cfg.Storage.Meta.DSN)
		if err != nil {
			return nil, nil, nil, nil, nil, func() {}, err
		}
		if err := postgres.Migrate(db); err != nil {
			_ = db.Close()
			return nil, nil, nil, nil, nil, func() {}, err
		}
		return org.NewSQLStorePostgres(db), apikey.NewSQLStorePostgres(db), grant.NewSQLStorePostgres(db), repo.NewSQLStorePostgres(db), db, func() { _ = db.Close() }, nil
	default:
		return nil, nil, nil, nil, nil, func() {}, fmt.Errorf("unknown meta driver %q (want \"sqlite\" or \"postgres\")", cfg.Storage.Meta.Driver)
	}
}

// mountOCI wires the /v2/ data plane. The OCI pull-through handler always mounts
// (serving the upstream cache for non-hosted names). When the writable registry
// is enabled it is additionally:
//   - given the hosted-first pull seam (HostedResolver + HostedReadAuthz) so a
//     pushed org-owned repo is served from CAS and visibility-checked; and
//   - wrapped by the registry WRITE handler (push endpoints) which in turn is
//     wrapped by the registry-token Bearer challenge middleware.
//
// The final chain at /v2/ is: Challenge → registry(write) → oci(read).
func mountOCI(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry, tokenSvc *registrytoken.Service, authz *registryauthz.Authz, repoStore *repo.SQLStore, blobs blobstore.BlobStore, registryEnabled bool) {
	pc, ok := cfg.Protocols["oci"]
	opts := []oci.Option{
		oci.WithMeta(metaStore),
		oci.WithLogger(log.With("protocol", "oci")),
	}
	if ok {
		opts = append(opts,
			oci.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("oci")), upstreamsFor("oci", pc.Upstreams)),
			oci.WithMutableTTL(mutableTTL(pc, cfg)),
		)
	}

	if !registryEnabled {
		// Pull-through-only: no hosted-first seam, no write path, no auth gate.
		mux.Handle("/v2/", metrics.Middleware("oci", oci.NewHandler(cm, opts...)))
		log.Info("specula: mounted OCI handler (pull-through only)", "path", "/v2/", "configured", ok)
		return
	}

	// Hosted-first pull: resolve org-owned repos and enforce per-repo visibility.
	opts = append(opts,
		oci.WithHostedResolver(authz),
		oci.WithHostedReadAuthz(authz),
		oci.WithOwnedNamespaceResolver(authz),
	)
	ociRead := oci.NewHandler(cm, opts...)

	// Write path over the shared CAS; non-write requests fall through to the
	// pull-through read handler above.
	writeHandler := registry.NewHandler(cm, repoStore, repoStore, authz,
		registry.WithBlobStore(blobs),
		registry.WithMeta(metaStore),
		registry.WithReadHandler(ociRead),
		registry.WithLogger(log.With("protocol", "oci", "component", "registry")),
	)

	// Bearer challenge gate. The realm is computed per-request from the Host so a
	// single binary advertises a correct same-origin /token URL.
	challenge := tokenSvc.ChallengeFunc(registryRealm)
	mux.Handle("/v2/", metrics.Middleware("oci", challenge(writeHandler)))
	log.Info("specula: mounted writable registry", "path", "/v2/", "configured", ok, "auth", "registry-token")
}

// registryRealm computes the absolute /token URL to advertise in the
// WWW-Authenticate challenge from the incoming request. It honours an
// X-Forwarded-Proto set by a TLS-terminating proxy, defaulting to http.
func registryRealm(r *http.Request) string {
	scheme := "http"
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/token"
}

// buildRegistryTokenService resolves the durable RS256 registry signing keypair
// through the settings layer (encrypted store first, on-disk PEM migrated in,
// otherwise freshly generated) and constructs the token Service. On any key
// error it warns and returns enabled=false so /v2/ stays pull-through-only and
// the cache keeps serving reads.
func buildRegistryTokenService(ctx context.Context, resolver *settings.Resolver, keyPath string, log *slog.Logger) (*registrytoken.Service, bool) {
	key, err := ensureRegistryKey(ctx, resolver, keyPath, log)
	if err != nil {
		log.Warn("specula: registry token key unavailable — /v2/ stays pull-through only (no push/private repos)",
			"key_path", keyPath, "err", err)
		return nil, false
	}
	log.Info("specula: writable registry enabled", "issuer", registryIssuer, "service", registryService)
	return registrytoken.NewService(key, registryIssuer, registryService, 0), true
}

// registryIssuer / registryService are the iss/aud identities of registry
// tokens (the service name must match the docker client's ?service= param).
const (
	registryIssuer  = "specula"
	registryService = "specula"
)

// resolveRegistryKeyPath returns the configured registry token key path, else a
// durable default next to the local blob store, else a temp-dir path (with the
// caveat that a temp key is not stable across restarts).
func resolveRegistryKeyPath(cfg *config.Config) string {
	if cfg.Auth.RegistryTokenKeyPath != "" {
		return cfg.Auth.RegistryTokenKeyPath
	}
	if cfg.Storage.Blob.Driver == "local" && cfg.Storage.Blob.Local.Root != "" {
		return filepath.Join(filepath.Dir(strings.TrimRight(cfg.Storage.Blob.Local.Root, "/")), "specula-registry-token.pem")
	}
	return filepath.Join(os.TempDir(), "specula-registry-token.pem")
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
func mountGoModule(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry) {
	pc, ok := cfg.Protocols[goProtocolKey]
	opts := []gomod.Option{
		gomod.WithMeta(metaStore),
		gomod.WithPathPrefix(goMountPrefix),
		gomod.WithLogger(log.With("protocol", gomod.Protocol)),
	}

	sumdbEnabled := false
	if ok {
		if len(pc.Upstreams) > 0 {
			// The metrics protocol label MUST be gomod.Protocol ("gomod"), not
			// goProtocolKey ("go"). The config keys this block "go", but the
			// on-the-wire ArtifactRef.Protocol — which is what the upstream client
			// labels specula_upstream_latency_seconds and specula_upstream_blocked
			// with — is "gomod". Pre-initialising under "go" would mint a phantom
			// {protocol="go"} series that reads 0 for ever while any real block
			// appeared under {protocol="gomod"}: an operator watching the phantom
			// would see a permanently healthy upstream. Caught by the real-traffic
			// acceptance run, which showed {protocol="go"} alongside gomod's
			// hit/miss series.
			opts = append(opts, gomod.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime(goProtocolKey)), upstreamsFor(gomod.Protocol, pc.Upstreams)))
		}
		opts = append(opts, gomod.WithMutableTTL(mutableTTL(pc, cfg)))

		// /sumdb/ passthrough: transparently proxy the go command's own signed
		// tree-head + inclusion/consistency verification to the configured sumdb.
		// Shares the GONOSUMDB private-module matcher with the chain verifier so a
		// private module name is never forwarded to the public sumdb (H5).
		if pc.SumDB != nil {
			// The passthrough serves exactly the database named by the configured
			// verifier key — the same key the chain verifier pins — so the two
			// cannot disagree about WHICH log is authoritative.
			sh := gomod.NewSumDBHandler(pc.SumDB.URL,
				gomod.WithSumDBName(verify.SumDBNameFromKey(pc.SumDB.VerifierKey)),
				gomod.WithSumDBPrivateMatcher(verify.NewPrivateMatcher(pc.SumDB.PrivatePatterns)),
				gomod.WithSumDBLogger(log.With("protocol", gomod.Protocol, "component", "sumdb")),
			)
			opts = append(opts, gomod.WithSumDB(sh))
			sumdbEnabled = true
		}
	}

	mux.Handle(goMountPrefix+"/", metrics.Middleware("gomod", gomod.NewHandler(cm, opts...)))
	log.Info("specula: mounted Go module proxy",
		"path", goMountPrefix+"/", "configured", ok, "sumdb_passthrough", sumdbEnabled)
}

// buildGoSumDBVerifier constructs the Go checksum-database verifier from the "go"
// protocol's sumdb block, or returns nil (with a startup warning) when the go
// protocol has no sumdb config. Without the warning, go is the only protocol
// that degrades to tofu silently — every other anchor builder (GPG, Helm .prov,
// git signed-refs) logs a Warn when its anchor is unconfigured. Anti-rollback
// high-water tree size is persisted via the metadata store.
func buildGoSumDBVerifier(cfg *config.Config, metaStore metastore.MetadataStore, log *slog.Logger) *verify.SumDBVerifier {
	pc, ok := cfg.Protocols[goProtocolKey]
	if !ok || pc.SumDB == nil {
		log.Warn("specula: go sumdb not configured — go tops out at tofu tier")
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
		// nil → built-in tolerance for CDN edge lag; 0 → strict (BUG D).
		RollbackToleranceEntries: sc.RollbackToleranceEntries,
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
func mountPyPI(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry) {
	pc, ok := cfg.Protocols["pypi"]
	l := log.With("protocol", pypi.Protocol)
	opts := []pypi.Option{
		pypi.WithMeta(metaStore),
		pypi.WithPathPrefix(pypiPrefix),
		pypi.WithLogger(l),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, pypi.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("pypi")), upstreamsFor("pypi", pc.Upstreams)))
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
	mux.Handle(pypiPrefix+"/", metrics.Middleware("pypi", pypi.NewHandler(cm, opts...)))
	log.Info("specula: mounted PyPI handler", "path", pypiPrefix+"/", "configured", ok)
}

// mountNPM wires the npm handler at /npm/ using the "npm" protocol config for
// upstreams, mutable TTL and the optional dependency-confusion private guard.
// npm tops out at TOFU in this batch (scoped names are confusion-resistant).
func mountNPM(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry) {
	pc, ok := cfg.Protocols["npm"]
	l := log.With("protocol", npm.Protocol)
	opts := []npm.Option{
		npm.WithMeta(metaStore),
		npm.WithPathPrefix(npmPrefix),
		npm.WithLogger(l),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, npm.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("npm")), upstreamsFor("npm", pc.Upstreams)))
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
	mux.Handle(npmPrefix+"/", metrics.Middleware("npm", npm.NewHandler(cm, opts...)))
	log.Info("specula: mounted npm handler", "path", npmPrefix+"/", "configured", ok)
}

// mountAPT wires the apt handler at /apt/ using the "apt" protocol config. The
// GPG chain verifier (apt's signed anchor) is passed in when configured; the same
// instance is already registered in the shared verify chain so verify-on-write and
// the handler share one stateful anchor. Without it, apt tops out at tofu.
func mountAPT(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry, gpgV *verify.GPGVerifier) {
	pc, ok := cfg.Protocols["apt"]
	opts := []apthandler.Option{
		apthandler.WithMeta(metaStore),
		apthandler.WithPathPrefix(aptPrefix),
		apthandler.WithLogger(log.With("protocol", apthandler.Protocol)),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, apthandler.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("apt")), upstreamsFor("apt", pc.Upstreams)))
		}
		opts = append(opts, apthandler.WithMutableTTL(mutableTTL(pc, cfg)))
	}
	if gpgV != nil {
		opts = append(opts, apthandler.WithGPGVerifier(gpgV))
	}
	mux.Handle(aptPrefix+"/", metrics.Middleware("apt", apthandler.NewHandler(cm, opts...)))
	log.Info("specula: mounted APT handler",
		"path", aptPrefix+"/", "configured", ok, "signed", gpgV != nil)
}

// mountHelm wires the classic-HTTP Helm handler at /helm/ using the "helm"
// protocol config. The .prov GPG verifier (helm's signed anchor) is passed in
// when configured; the same instance is registered in the shared chain. Without
// it, helm tops out at tofu.
func mountHelm(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry, provV *verify.HelmProvVerifier) {
	pc, ok := cfg.Protocols["helm"]
	opts := []helmhandler.Option{
		helmhandler.WithMeta(metaStore),
		helmhandler.WithPathPrefix(helmPrefix),
		helmhandler.WithLogger(log.With("protocol", helmhandler.Protocol)),
	}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, helmhandler.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("helm")), upstreamsFor("helm", pc.Upstreams)))
		}
		opts = append(opts, helmhandler.WithMutableTTL(mutableTTL(pc, cfg)))
	}
	if provV != nil {
		opts = append(opts, helmhandler.WithProvenanceVerifier(provV))
	}
	mux.Handle(helmPrefix+"/", metrics.Middleware("helm", helmhandler.NewHandler(cm, opts...)))
	log.Info("specula: mounted Helm handler",
		"path", helmPrefix+"/", "configured", ok, "signed", provV != nil)
}

// mountTarball wires the generic URL-keyed tarball handler at /tarball/ using the
// "tarball" protocol config. The allowed-host allowlist is derived from the
// configured upstream base URLs (a request host outside the list is rejected).
// tarball tops out at TOFU (immutable digest pin) in this batch.
func mountTarball(mux *http.ServeMux, cfg *config.Config, cm cache.CacheManager, metaStore metastore.MetadataStore, log *slog.Logger, ups *upstream.Registry) {
	pc, ok := cfg.Protocols["tarball"]
	opts := []tarballhandler.Option{
		tarballhandler.WithMeta(metaStore),
		tarballhandler.WithPathPrefix(tarballPrefix),
		tarballhandler.WithLogger(log.With("protocol", tarballhandler.Protocol)),
	}
	hosts := []string{}
	if ok {
		if len(pc.Upstreams) > 0 {
			opts = append(opts, tarballhandler.WithUpstream(upstream.NewClientWithRuntime(ups.Runtime("tarball")), upstreamsFor("tarball", pc.Upstreams)))
		}
		hosts = upstreamHosts(pc.Upstreams)
		opts = append(opts, tarballhandler.WithAllowedHosts(hosts))
	}
	mux.Handle(tarballPrefix+"/", metrics.Middleware("tarball", tarballhandler.NewHandler(cm, opts...)))
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
	mux.Handle(gitPrefix+"/", metrics.Middleware("git", githandler.NewHandler(opts...)))
	log.Info("specula: mounted git handler",
		"path", gitPrefix+"/", "configured", ok, "signed", signedV != nil)
}

// buildGPGVerifier constructs the apt InRelease→Packages→.deb GPG chain verifier
// from the "apt" protocol's verification.gpg.keyring path. Returns nil (and warns)
// when the keyring is unset or unreadable so apt downgrades to tofu without crashing.
//
// db is the shared metadata database handle (the same one the multi-tenant and
// settings stores use). The chain's pinned hashes are persisted there rather than
// held in this process's heap: they are required chain state, so per PRD §G3 the
// replica that serves `apt-get update` must be able to hand them to whichever
// replica serves the `.deb`, and they must survive a restart. Without this, apt —
// the PRD's end-to-end gold standard — 502s in exactly the HA topology §G3
// specifies.
//
// A pin store is REQUIRED, not optional: if it cannot be built, this returns nil
// so apt visibly falls back to tofu, rather than silently reverting to a
// heap-local chain that works on one replica and fails on the next.
func buildGPGVerifier(cfg *config.Config, db *sql.DB, log *slog.Logger) *verify.GPGVerifier {
	pc, ok := cfg.Protocols["apt"]
	if !ok || pc.Verification.GPG == nil || strings.TrimSpace(pc.Verification.GPG.Keyring) == "" {
		log.Warn("specula: apt GPG keyring not configured — apt tops out at tofu tier")
		return nil
	}

	pins, err := buildAptPinStore(cfg, db)
	if err != nil {
		log.Warn("specula: apt GPG verifier disabled (no pin store) — downgrading apt to tofu",
			"err", err)
		return nil
	}

	keyring := pc.Verification.GPG.Keyring
	v, err := verify.NewGPGVerifier(keyring, verify.WithAptPinStore(pins))
	if err != nil {
		log.Warn("specula: apt GPG verifier disabled (keyring load failed) — downgrading apt to tofu",
			"keyring", keyring, "err", err)
		return nil
	}
	log.Info("specula: apt GPG signed verification enabled",
		"keyring", keyring, "policy", policyOrWarn(pc.Verification.GPG.Policy),
		"pins", cfg.Storage.Meta.Driver)
	return v
}

// buildAptPinStore binds the apt trust-chain pin store to the configured metadata
// database. The tables are created by the 0006/006 apt pins migration, which both
// drivers apply at open time.
func buildAptPinStore(cfg *config.Config, db *sql.DB) (verify.AptPinStore, error) {
	if db == nil {
		return nil, fmt.Errorf("apt pins: no metadata database handle")
	}
	switch cfg.Storage.Meta.Driver {
	case "sqlite":
		return aptpins.New(db, aptpins.SQLite), nil
	case "postgres":
		return aptpins.New(db, aptpins.Postgres), nil
	default:
		return nil, fmt.Errorf("apt pins: unknown meta driver %q", cfg.Storage.Meta.Driver)
	}
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

// consensusMetadataProtocols is the set of protocols for which a mirror's
// sha256 digest is obtainable metadata-only (HEAD / index page) and therefore
// directly comparable to the artifact's CAS digest. pypi publishes a PEP 503
// "#sha256=" per file; oci returns Docker-Content-Digest on a manifest/blob
// HEAD. npm (sha512 integrity) and generic tarballs (no advertised digest)
// cannot be cross-checked without downloading the blob, so consensus is not
// enabled for them (it would fail-close every real fetch).
var consensusMetadataProtocols = map[string]bool{"oci": true, "pypi": true}

// consensusEnabled reports whether a protocol's verification config asks for the
// consensus tier — either via the tiers list or a structured consensus block.
func consensusEnabled(vc config.VerificationConfig) bool {
	if vc.Consensus != nil {
		return true
	}
	for _, t := range vc.Tiers {
		if t == "consensus" {
			return true
		}
	}
	return false
}

// buildConsensusVerifier constructs a protocol-scoped cross-source consensus
// verifier for the named protocol from its config, or returns (nil, nil) when
// consensus is not enabled or not achievable metadata-only for that protocol.
// Mirrors come from the structured consensus block when present, else are derived
// from the protocol upstreams (non-official = independent mirrors; the official
// upstream becomes the authoritative origin witness). The returned verifier is
// scoped so it only acts on its own protocol within the shared chain.
//
// An UNSATISFIABLE quorum (quorum > mirrors) is a hard error, not a warning: the
// tier could never pass, so every fetch of that protocol would 502 forever. The
// derivation rule makes this easy to get wrong — marking the official origin
// `official: true` moves it OUT of the mirror set and into the witness slot, so
// two upstreams with quorum:2 leaves one voter. Startup used to log a cheerful
// "quorum:2 mirrors:1" and serve 502s. A security tier that silently cannot pass
// is worse than one that is off, so we refuse to start and name the numbers
// (BUG C).
func buildConsensusVerifier(protocol string, cfg *config.Config, fetcher verify.MirrorDigestFetcher, log *slog.Logger) (verify.Verifier, error) {
	pc, ok := cfg.Protocols[protocol]
	if !ok {
		return nil, nil
	}
	vc := pc.Verification
	if !consensusEnabled(vc) {
		return nil, nil
	}
	if !consensusMetadataProtocols[protocol] {
		log.Warn("specula: consensus requested but not achievable metadata-only — staying at tofu",
			"protocol", protocol,
			"reason", "mirror metadata advertises no sha256 (npm uses sha512 integrity; tarball advertises none)")
		return nil, nil
	}

	quorum := vc.Quorum
	var mirrors []verify.ConsensusMirror
	var origin verify.OriginCheck

	if vc.Consensus != nil {
		quorum = vc.Consensus.Quorum
		for _, m := range vc.Consensus.Mirrors {
			mirrors = append(mirrors, verify.ConsensusMirror{Name: m.Name, BaseURL: m.BaseURL})
		}
		if oc := vc.Consensus.OriginCheck; oc != nil {
			origin = verify.OriginCheck{URL: oc.URL, ViaProxy: oc.ViaProxy}
		}
	}
	if len(mirrors) == 0 {
		// Derive mirrors from the protocol upstreams: independent (non-official)
		// mirrors vote; the first official upstream is the origin witness.
		for _, u := range pc.Upstreams {
			if u.Official && origin.URL == "" {
				origin.URL = u.BaseURL
				continue
			}
			mirrors = append(mirrors, verify.ConsensusMirror{Name: u.Name, BaseURL: u.BaseURL})
		}
	}
	if quorum < 1 {
		quorum = 1
	}

	// Fail closed on an unsatisfiable quorum, naming both numbers and the
	// derivation that produced them.
	if len(mirrors) == 0 {
		return nil, fmt.Errorf(
			"protocols[%s].verification: consensus tier enabled with quorum %d but 0 independent mirrors — "+
				"the tier can never pass and every %s fetch would fail closed. Non-official upstreams become "+
				"consensus mirrors; an upstream marked `official: true` becomes the origin WITNESS, not a mirror. "+
				"Configure >= %d non-official upstreams (or an explicit verification.consensus.mirrors list), "+
				"or remove the consensus tier",
			protocol, quorum, protocol, quorum)
	}
	if quorum > len(mirrors) {
		return nil, fmt.Errorf(
			"protocols[%s].verification: consensus quorum %d exceeds the %d available independent mirror(s) — "+
				"the tier can never pass and every %s fetch would fail closed. Note an upstream marked "+
				"`official: true` becomes the origin WITNESS, not a mirror. Either lower quorum to <= %d, "+
				"or add %d more non-official upstream(s)/consensus mirror(s)",
			protocol, quorum, len(mirrors), protocol, len(mirrors), quorum-len(mirrors))
	}

	cv := verify.NewConsensusVerifier(verify.ConsensusConfig{
		Quorum:      quorum,
		Mirrors:     mirrors,
		OriginCheck: origin,
	}, fetcher)
	log.Info("specula: cross-source consensus enabled",
		"protocol", protocol, "quorum", quorum, "mirrors", len(mirrors), "origin_check", origin.URL != "")
	return newProtocolScopedVerifier(cv, protocol), nil
}

// buildCosignVerifier constructs the keyed cosign verifier for the oci protocol
// from configured public keys (structured cosign.keys, or the flat cosign_key
// back-compat field), or returns nil when no keys are set. The transparency log
// is always disabled (CN-offline keyed mode). Signature discovery is wired to
// go-containerregistry over the configured oci registries. A key load failure
// warns and downgrades oci to consensus/tofu rather than crashing.
func buildCosignVerifier(cfg *config.Config, log *slog.Logger) *verify.CosignVerifier {
	pc, ok := cfg.Protocols["oci"]
	if !ok {
		return nil
	}
	vc := pc.Verification
	var keys []string
	if vc.Cosign != nil {
		keys = vc.Cosign.Keys
	}
	if len(keys) == 0 && strings.TrimSpace(vc.CosignKey) != "" {
		keys = []string{vc.CosignKey}
	}
	if len(keys) == 0 {
		log.Warn("specula: oci cosign keys not configured — oci tops out at consensus/tofu tier")
		return nil
	}

	registries := make([]string, 0, len(pc.Upstreams))
	for _, u := range pc.Upstreams {
		registries = append(registries, u.BaseURL)
	}
	fetcher := verify.NewOCISignatureFetcher(registries)

	v, err := verify.NewCosignVerifier(verify.CosignConfig{Keys: keys, Tlog: false}, fetcher)
	if err != nil {
		log.Warn("specula: oci cosign verifier disabled (key load failed) — downgrading oci to consensus/tofu",
			"keys", len(keys), "err", err)
		return nil
	}
	log.Info("specula: oci cosign keyed signed verification enabled",
		"keys", len(keys), "registries", len(registries), "tlog", false)
	return v
}

// protocolScopedVerifier wraps a Verifier so it only acts on artifacts of a
// single protocol. For any other protocol it returns a no-op StatusPass at
// TierChecksum, exactly like the built-in per-protocol verifiers' self-gate, so
// a single shared chain can carry protocol-specific verifiers (e.g. the
// protocol-blind consensus verifier) without one acting on another's artifacts.
type protocolScopedVerifier struct {
	inner    verify.Verifier
	protocol string
}

func newProtocolScopedVerifier(inner verify.Verifier, protocol string) *protocolScopedVerifier {
	return &protocolScopedVerifier{inner: inner, protocol: protocol}
}

func (p *protocolScopedVerifier) Name() string        { return p.inner.Name() }
func (p *protocolScopedVerifier) Tier() artifact.Tier { return p.inner.Tier() }

func (p *protocolScopedVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if ref.Protocol != p.protocol {
		// StatusSkip, not StatusPass: this verifier did not run. Reporting a
		// pass here would manufacture presence in specula_verification_total —
		// consensus configured for pypi alone would be counted as having run
		// and passed on every apt, gomod and helm artifact Specula ever served,
		// in the very metric PRD §7 offers the operator as their independent
		// check on our trust claims. verify.go's contract: absence means
		// "did not run".
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("%s: skipped (protocol %q out of scope)", p.inner.Name(), ref.Protocol),
		}, nil
	}
	return p.inner.Verify(ctx, ref, art)
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

// upstreamsFor converts a protocol's configured upstreams AND declares each one
// to the metrics layer, so specula_upstream_blocked{protocol,upstream} reads a
// real 0 from the very first scrape of a fresh headless process.
//
// Without this the gauge would spring into existence only once an upstream had
// already failed, making "absent" and "healthy" indistinguishable at exactly the
// moment an operator is looking. Declaring the configured set is safe precisely
// because it is bounded and known: it comes from config, not from traffic.
func upstreamsFor(protocol string, in []config.UpstreamConfig) []upstream.Upstream {
	out := toUpstreams(in)
	for _, u := range out {
		metrics.PreInitUpstream(protocol, u.Name)
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

// resolveJWTSecret is gone: see ensureJWTSecret in settings.go. It generated a
// fresh EPHEMERAL secret on every boot when auth.jwt_secret was unset, which
// signed every user out on restart and made sessions non-portable across HA
// replicas. The secret is now generated once and persisted into the encrypted
// settings store, so both problems disappear; the loud warning survives for the
// case where no master key is configured and the old behaviour still applies.

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
