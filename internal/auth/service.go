package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ---- service-level sentinel errors -------------------------------------------

var (
	// ErrUserNotFound is returned by UserStore when no matching record exists.
	ErrUserNotFound = errors.New("auth: user not found")
	// ErrEmailRequired is returned when Register is called with a blank email.
	ErrEmailRequired = errors.New("auth: email is required")
	// ErrPasswordTooShort is returned when the password is below MinPasswordLen.
	ErrPasswordTooShort = fmt.Errorf("auth: password must be at least %d characters", MinPasswordLen)
	// ErrInvalidCredentials is returned by Login on any authentication failure.
	// The same sentinel is used for "user not found" and "wrong password" to
	// prevent user-enumeration: callers cannot distinguish the two cases.
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	// ErrEmailTaken is returned by Register when the email is already in use.
	ErrEmailTaken = errors.New("auth: email already registered")
)

// ---- UserStore interface -------------------------------------------------------

// UserStore is the persistence interface required by Service. Implementations
// live in internal/store/{sqlite,postgres}; tests use the local fakeStore.
//
// Implementations must return ErrUserNotFound (or an error wrapping it) when
// a lookup by email or ID yields no row, so callers can distinguish "not found"
// from unexpected DB errors.
type UserStore interface {
	// CountUsers returns the total number of user rows.
	CountUsers(ctx context.Context) (int64, error)
	// GetUserByEmail returns the user with the given email, or ErrUserNotFound.
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	// CreateUser persists a new user and returns the stored copy with its
	// assigned ID populated. Returns ErrEmailTaken on a unique-constraint
	// violation (e.g. race between CountUsers check and insert).
	CreateUser(ctx context.Context, user User) (*User, error)
	// GetUserByID returns the user with the given ID, or ErrUserNotFound.
	GetUserByID(ctx context.Context, id int64) (*User, error)
	// BumpTokenGen atomically increments the user's token_gen and returns the
	// new value. All previously issued JWTs become invalid because the embedded
	// gen snapshot no longer matches.
	BumpTokenGen(ctx context.Context, id int64) (int64, error)

	// ---- admin management surface (used by internal/admin) --------------------

	// ListUsers returns a page of users ordered by ID ascending (limit/offset)
	// together with the total user count for pagination. A limit <= 0 returns
	// all rows. Password hashes are populated but callers must never expose them.
	ListUsers(ctx context.Context, limit, offset int) ([]User, int64, error)
	// UpdateUserRole sets the user's system_role ("admin" | "user"). Returns
	// ErrUserNotFound when no row matches id.
	UpdateUserRole(ctx context.Context, id int64, role string) error
	// DeleteUser removes the user row. Returns ErrUserNotFound when absent.
	DeleteUser(ctx context.Context, id int64) error
}

// ---- Service ------------------------------------------------------------------

// Service implements email register/login and session management for the
// Specula control plane.
type Service struct {
	store    UserStore
	hasher   PasswordHasher
	verifier TokenVerifier
	secure   bool // set Secure flag on session cookies (enable for HTTPS)
}

// NewService constructs an AuthService.
//
//   - store    — user persistence
//   - hasher   — password hashing (NewBcryptHasher())
//   - verifier — JWT signing/verification (NewHS256Verifier(secret))
//   - secure   — enable Secure flag on Set-Cookie (true in HTTPS deployments)
func NewService(store UserStore, hasher PasswordHasher, verifier TokenVerifier, secure bool) *Service {
	return &Service{store: store, hasher: hasher, verifier: verifier, secure: secure}
}

// Secure reports whether session cookies should carry the Secure attribute.
func (s *Service) Secure() bool { return s.secure }

// Register creates a new user account.
//
// First-user-admin: if CountUsers()==0 at the time of this call, the new
// account receives system_role="admin" so the very first operator gains
// immediate control-plane access without any out-of-band seeding step.
//
// The email is normalised to lower-case and trimmed before storage. Passwords
// shorter than MinPasswordLen are rejected before hashing.
func (s *Service) Register(ctx context.Context, email, password string) (*User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, ErrEmailRequired
	}
	if len(password) < MinPasswordLen {
		return nil, ErrPasswordTooShort
	}

	// Check for an existing account before hashing (cheap early-exit).
	if existing, err := s.store.GetUserByEmail(ctx, email); err == nil && existing != nil {
		return nil, ErrEmailTaken
	} else if err != nil && !errors.Is(err, ErrUserNotFound) {
		return nil, fmt.Errorf("auth: check email: %w", err)
	}

	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: count users: %w", err)
	}
	role := "user"
	if count == 0 {
		role = "admin" // first-user-admin bootstrap
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return nil, fmt.Errorf("auth: hash password: %w", err)
	}

	u, err := s.store.CreateUser(ctx, User{
		Email:        email,
		PasswordHash: hash,
		SystemRole:   role,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: create user: %w", err)
	}
	return u, nil
}

// Login authenticates email+password and, on success, returns a signed session
// JWT and the matched User.
//
// Timing equalization: when the email does not exist in the store,
// CheckPasswordDummy() is called to consume the same CPU time as a real bcrypt
// comparison, preventing user enumeration via response-latency side-channel.
// The returned error is always ErrInvalidCredentials regardless of whether the
// failure was "no such user" or "wrong password".
func (s *Service) Login(ctx context.Context, email, password string) (string, *User, error) {
	email = strings.TrimSpace(strings.ToLower(email))

	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		// User not found: absorb the bcrypt-shaped timing gap to prevent
		// email enumeration via response latency (see password.go).
		CheckPasswordDummy()
		return "", nil, ErrInvalidCredentials
	}

	if err := s.hasher.Compare(u.PasswordHash, password); err != nil {
		return "", nil, ErrInvalidCredentials
	}

	token, err := s.verifier.Sign(*u)
	if err != nil {
		return "", nil, fmt.Errorf("auth: sign token: %w", err)
	}
	return token, u, nil
}

// Logout bumps the user's token_gen, invalidating all previously issued JWTs.
// The next Middleware check for any existing session cookie will detect the
// gen mismatch and return 401. Callers should also clear the client-side
// cookie with ClearSessionCookie.
func (s *Service) Logout(ctx context.Context, userID int64) error {
	if _, err := s.store.BumpTokenGen(ctx, userID); err != nil {
		return fmt.Errorf("auth: bump token gen: %w", err)
	}
	return nil
}
