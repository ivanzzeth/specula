// Package auth provides the control-plane user model and the minimal auth
// primitives used by the Specula control plane: bcrypt password hashing and
// an HS256 JWT signer/verifier backed by github.com/golang-jwt/jwt/v5.
//
// Algorithm safety: every call to Verify first inspects the JOSE header
// manually (before the library's signature work begins) and rejects any alg
// that is not exactly "HS256". The parser is also configured with
// WithValidMethods([]string{"HS256"}) as a second layer of defence, ensuring
// alg=none, RS*, ES*, PS*, and lowercase variants are all rejected before any
// cryptographic operation runs.
//
// # token_gen revocation
//
// The TokenGen field in User allows server-side logout-all: bumping a user's
// token_gen in the database invalidates all previously issued tokens. The
// embedded gen snapshot is verified against the live store value in Middleware.
//
// # First-user-admin
//
// When CountUsers()==0 at registration time the new account receives
// system_role="admin" so the first operator gains immediate access to the
// control plane without out-of-band seeding.
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// ---- constants ----------------------------------------------------------------

// TokenCookieName is the httpOnly SameSite=Lax cookie name for the session JWT.
const TokenCookieName = "specula_session"

// tokenIssuer and tokenAudience are embedded in every signed JWT and checked
// during verification to prevent issuer/audience confusion.
const (
	tokenIssuer   = "specula-auth"
	tokenAudience = "specula-session"
)

// DefaultTokenTTL is the lifetime of a freshly-issued session JWT (30 days).
const DefaultTokenTTL = 30 * 24 * time.Hour

// clockSkew is the tolerance added to exp validation to absorb minor server
// clock differences between nodes.
const clockSkew = 60 * time.Second

// ---- sentinel errors ----------------------------------------------------------

var (
	// ErrMalformed is returned when the token structure cannot be parsed.
	ErrMalformed = errors.New("auth: malformed token")
	// ErrAlg is returned when the token declares an alg other than HS256
	// (alg=none / RS* / ES* / PS* / hs256 lower-case are all rejected).
	ErrAlg = errors.New("auth: unexpected signing alg")
	// ErrSignature is returned when the HMAC-SHA256 signature does not match.
	ErrSignature = errors.New("auth: signature invalid")
	// ErrExpired is returned when the token's exp claim is in the past
	// (beyond clockSkew tolerance).
	ErrExpired = errors.New("auth: token expired")
	// ErrIssuer is returned when the iss claim does not match tokenIssuer.
	ErrIssuer = errors.New("auth: unexpected issuer")
	// ErrAudience is returned when the aud claim is present but does not match
	// tokenAudience. A missing aud is accepted for backward compatibility.
	ErrAudience = errors.New("auth: unexpected audience")
)

// ---- core types (foundation contract) ----------------------------------------

// User is the control-plane account row. The first registered user
// (CountUsers()==0 at register time) becomes system_role="admin".
type User struct {
	ID           int64     // stable DB identifier
	Email        string    // login identity (normalised to lower-case)
	Name         string    // optional display name (never used for auth)
	PasswordHash string    // bcrypt hash; never logged or returned to clients
	SystemRole   string    // "admin" | "user"
	TokenGen     int64     // generation counter; embedded in JWT, bump to revoke all sessions
	CreatedAt    time.Time // account creation time (zero until persisted)
}

// PasswordHasher hashes and verifies passwords.
type PasswordHasher interface {
	// Hash returns a bcrypt hash of password.
	Hash(password string) (string, error)
	// Compare checks password against a stored bcrypt hash; returns non-nil on mismatch.
	Compare(hash, password string) error
}

// TokenVerifier signs and verifies session JWTs (HS256, httpOnly cookie).
type TokenVerifier interface {
	// Sign issues a signed token embedding the user's token_gen snapshot.
	Sign(user User) (string, error)
	// Verify validates a token and returns the embedded user claims.
	// Callers must additionally check TokenGen against the live store value
	// to detect revocation (see Middleware).
	Verify(token string) (User, error)
}

// ---- bcrypt password hasher ---------------------------------------------------

// bcryptHasher implements PasswordHasher using bcrypt.DefaultCost.
type bcryptHasher struct{}

// NewBcryptHasher constructs a bcrypt-backed PasswordHasher.
func NewBcryptHasher() PasswordHasher { return &bcryptHasher{} }

// Compile-time assertion.
var _ PasswordHasher = (*bcryptHasher)(nil)

func (h *bcryptHasher) Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (h *bcryptHasher) Compare(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// ---- HS256 JWT signer / verifier (backed by golang-jwt/jwt/v5) ---------------

// jwtHeader holds only the fields we care about from the JOSE header.
// Retained so that the manual alg pre-check (Step 1 of verifyAt) can inspect
// the token before the library begins any cryptographic work. Tests in package
// auth also use b64 directly, so both must remain package-visible.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// sessionClaims is the combined JWT payload for a Specula session token.
// Custom fields preserve the wire names from the predecessor so existing tokens
// issued before this change remain verifiable (same json tags). Embedding
// jwt.RegisteredClaims provides iss/aud/exp/iat in their RFC 7519 positions.
type sessionClaims struct {
	UserID     int64  `json:"uid"`
	Email      string `json:"email"`
	SystemRole string `json:"role"`
	TokenGen   int64  `json:"gen,omitempty"`
	jwt.RegisteredClaims
}

// b64 is the JWT-standard unpadded base64url codec.
// Kept as a package-level variable so tests in package auth can assemble
// hand-crafted tokens (e.g. for alg-confusion rejection tests) without
// importing encoding/base64 directly.
var b64 = base64.RawURLEncoding

// hs256Verifier implements TokenVerifier with HMAC-SHA256 via golang-jwt/v5.
type hs256Verifier struct{ secret []byte }

// NewHS256Verifier constructs an HS256 TokenVerifier from a signing secret.
// An empty secret is accepted here (to match the interface contract) but tokens
// signed with an empty secret cannot be verified by a verifier with a non-empty
// secret. Production callers should validate the secret is non-empty before use.
func NewHS256Verifier(secret []byte) TokenVerifier { return &hs256Verifier{secret: secret} }

// Compile-time assertion.
var _ TokenVerifier = (*hs256Verifier)(nil)

// Sign issues a signed HS256 JWT. The user's TokenGen snapshot is embedded so
// Middleware can detect logout-all revocation by comparing against the live store.
func (v *hs256Verifier) Sign(user User) (string, error) {
	now := time.Now()
	claims := sessionClaims{
		UserID:     user.ID,
		Email:      user.Email,
		SystemRole: user.SystemRole,
		TokenGen:   user.TokenGen,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(DefaultTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(v.secret)
}

// Verify validates a compact JWS token (header.payload.signature). It
// explicitly rejects alg=none and any asymmetric or unknown alg to prevent
// alg-confusion attacks. The signature is verified by the library before claims
// are inspected. If valid, returns the embedded User (with TokenGen snapshot).
func (v *hs256Verifier) Verify(token string) (User, error) {
	return v.verifyAt(token, time.Now())
}

// verifyAt is the internal implementation; accepts an explicit "now" so tests
// that construct tokens with synthetic expiry values can inject the clock.
func (v *hs256Verifier) verifyAt(token string, now time.Time) (User, error) {
	// ── Step 1: manual alg pre-check ────────────────────────────────────────
	// Inspect the JOSE header before the library touches the token. This is the
	// primary alg-confusion defence: reject anything that is not exactly "HS256"
	// (case-sensitive, per RFC 7518 §3) before any cryptographic operation runs.
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return User{}, ErrMalformed
	}
	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return User{}, ErrMalformed
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return User{}, ErrMalformed
	}
	// Accept ONLY "HS256". alg=none, RS*/ES*/PS*, and lowercase "hs256" are
	// all rejected here before the library even sees the token.
	if hdr.Alg != "HS256" {
		return User{}, ErrAlg
	}

	// ── Step 2: library signature + expiry verification ─────────────────────
	// WithValidMethods provides a second alg layer inside the library.
	// WithLeeway adds clockSkew tolerance to the exp check.
	// WithTimeFunc injects the caller's "now" so tests can control the clock.
	// Issuer and audience are NOT validated here — done manually in Step 3 so
	// we return our own typed sentinel errors rather than the library's.
	var claims sessionClaims
	_, parseErr := jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(func() time.Time { return now }),
	).ParseWithClaims(token, &claims, func(_ *jwt.Token) (interface{}, error) {
		return v.secret, nil
	})
	if parseErr != nil {
		return User{}, mapHS256ParseError(parseErr)
	}

	// ── Step 3: application-level claim checks ───────────────────────────────
	if claims.Issuer != tokenIssuer {
		return User{}, ErrIssuer
	}
	// Audience: missing = accepted (backward compat for tokens without aud);
	// present but not matching = rejected (prevents token-type confusion).
	if len(claims.Audience) > 0 && !containsString(claims.Audience, tokenAudience) {
		return User{}, ErrAudience
	}

	return User{
		ID:         claims.UserID,
		Email:      claims.Email,
		SystemRole: claims.SystemRole,
		TokenGen:   claims.TokenGen,
	}, nil
}

// mapHS256ParseError converts a golang-jwt parsing error to one of our typed
// sentinel errors. Because the alg pre-check already passed, any
// ErrTokenSignatureInvalid here is a genuine HMAC key mismatch, not an
// alg-confusion issue — so it maps to ErrSignature rather than ErrAlg.
func mapHS256ParseError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return ErrMalformed
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return ErrSignature
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrExpired
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		// alg=none causes ErrTokenUnverifiable ("signing method unavailable")
		// in ParseUnverified; shouldn't reach here after Step 1, but map to
		// ErrAlg as a safety net.
		return ErrAlg
	default:
		return ErrMalformed
	}
}

// containsString reports whether haystack contains needle (case-sensitive).
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
