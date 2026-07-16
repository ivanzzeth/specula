package admin

import (
	"context"
	"log/slog"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/grant"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/stats"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// BlobUsageReporter is the narrow slice of blob.BlobStore the admin API needs:
// the backend's total used-byte footprint for the capacity dashboard. Kept as a
// local one-method interface so the admin package does not depend on the whole
// CAS driver surface (accept interfaces, return structs).
type BlobUsageReporter interface {
	// UsageBytes returns the backend's total used bytes (may be cached/approx).
	UsageBytes(ctx context.Context) (int64, error)
}

// Deps is the explicit dependency set for the admin Server. Every field is an
// interface (or a read-only config snapshot) so the server is trivially
// testable with fakes and so the WebUI/backend can evolve behind the contract.
//
// Tokens and Users are also what the auth middleware is built from; Auth is the
// higher-level service used by the register/login/logout handlers.
type Deps struct {
	// Stats aggregates per-protocol + total cache capacity (G7).
	Stats stats.Collector
	// Meta is the metadata store (verification events, cache entries).
	Meta meta.MetadataStore
	// Users is the control-plane account store (list/create/patch/delete).
	Users auth.UserStore
	// Auth is the register/login/logout + session service.
	Auth *auth.Service
	// Tokens verifies session JWTs for the auth middleware.
	Tokens auth.TokenVerifier
	// Config is the running configuration snapshot (redacted before exposure).
	Config *config.Config
	// Blobs reports backend disk usage for the capacity dashboard.
	Blobs BlobUsageReporter
	// Secure sets the Secure flag on session cookies (true behind HTTPS).
	Secure bool
	// Logger is the structured logger; slog.Default() is used when nil.
	Logger *slog.Logger

	// ── multi-tenant deps (R1) ───────────────────────────────────────────────

	// OrgStore is the multi-tenant org/member/invitation persistence layer.
	// Optional: when nil, org-related endpoints return 501.
	OrgStore org.Store
	// KeyStore is the API-key persistence and lookup layer.
	// Optional: when nil, key endpoints return 501.
	KeyStore apikey.Store
	// GrantStore is the cross-org resource-sharing grants layer.
	// Optional: when nil, grant-aware acl falls back to CanAccess.
	GrantStore grant.Store
}

// Server holds the admin API dependencies and serves the /api/v1 routes.
// Construct it with New and mount it with RegisterRoutes.
type Server struct {
	stats  stats.Collector
	meta   meta.MetadataStore
	users  auth.UserStore
	auth   *auth.Service
	tokens auth.TokenVerifier
	cfg    *config.Config
	blobs  BlobUsageReporter
	// hasher is used by handleCreateUser and handlePatchUser to bcrypt-hash
	// passwords before storing them. Not in Deps (internal concern; always
	// bcrypt at DefaultCost).
	hasher auth.PasswordHasher
	secure bool
	log    *slog.Logger

	// multi-tenant deps (R1)
	orgs   org.Store
	keys   apikey.Store
	grants grant.Store
}

// New constructs an admin Server from deps. The logger falls back to
// slog.Default() when Deps.Logger is nil. No dependency is dialed here; the
// server is inert until its routes are mounted and requests arrive.
func New(deps Deps) *Server {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		stats:  deps.Stats,
		meta:   deps.Meta,
		users:  deps.Users,
		auth:   deps.Auth,
		tokens: deps.Tokens,
		cfg:    deps.Config,
		blobs:  deps.Blobs,
		hasher: auth.NewBcryptHasher(),
		secure: deps.Secure,
		log:    log,
		orgs:   deps.OrgStore,
		keys:   deps.KeyStore,
		grants: deps.GrantStore,
	}
}

// toUserDTO converts an auth.User to the safe client projection (drops the
// password hash and token generation counter).
func toUserDTO(u auth.User) UserDTO {
	return UserDTO{
		ID:         u.ID,
		Email:      u.Email,
		Name:       u.Name,
		SystemRole: u.SystemRole,
		CreatedAt:  u.CreatedAt,
	}
}
