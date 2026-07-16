package admin

import (
	"context"
	"log/slog"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
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

// UserUpdater is an optional extension to auth.UserStore used by the PATCH
// /api/v1/admin/users/{id} handler to update display name and password hash.
// If the injected UserStore also satisfies this interface the handler applies
// name/password patches; otherwise those fields return 501 while role changes
// via auth.UserStore.UpdateUserRole still succeed.
//
// NOTE: This interface is not yet part of auth.UserStore — it is defined here
// because UpdateUserFields is currently absent from both auth.UserStore and the
// concrete store implementations (SQLiteStore, PostgresStore). Until it is
// added to those packages, callers that need name/password patching must
// provide a UserStore that also implements UserUpdater (e.g. test fakes or a
// wrapper). This is a known missing-dep — tracked as TODO.
type UserUpdater interface {
	// UpdateUserFields sets zero or more mutable user fields identified by id.
	// Nil pointer means "leave unchanged". passwordHash, when non-nil, must
	// already be a bcrypt hash (the caller is responsible for hashing first).
	// Returns auth.ErrUserNotFound when no row matches id.
	UpdateUserFields(ctx context.Context, id int64, name, passwordHash *string) error
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
