// Package auth provides the control-plane user model and the minimal auth
// primitives used by the Specula control plane: bcrypt password hashing and
// a hand-rolled HS256 JWT signer/verifier (stdlib only, no third-party JWT
// libraries; rejects alg=none and any asymmetric or unknown alg family to
// prevent alg-confusion attacks). Ported/trimmed from ai-sandbox auth/
// (drops org/acl multi-tenancy).
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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

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

// ---- HS256 JWT signer / verifier ---------------------------------------------

// jwtHeader holds only the fields we care about from the JOSE header.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtClaims maps all User identity fields plus standard JWT registered claims.
// TokenGen is embedded so the middleware can detect revocation without a DB
// round-trip per request (the gen is compared to the live store value).
type jwtClaims struct {
	UserID     int64  `json:"uid"`
	Email      string `json:"email"`
	SystemRole string `json:"role"`
	TokenGen   int64  `json:"gen,omitempty"`
	Iss        string `json:"iss"`
	Aud        string `json:"aud,omitempty"`
	Exp        int64  `json:"exp"`
	Iat        int64  `json:"iat"`
}

// b64 is the JWT-standard unpadded base64url codec used for all three segments.
var b64 = base64.RawURLEncoding

// hs256Verifier implements TokenVerifier with a hand-rolled HS256 JWT.
type hs256Verifier struct{ secret []byte }

// NewHS256Verifier constructs an HS256 TokenVerifier from a signing secret.
// An empty secret is silently accepted here (to match the foundation contract
// signature) but will produce tokens that cannot be validated by any verifier
// constructed with a non-empty secret. Production callers should validate the
// secret is non-empty before passing it here.
func NewHS256Verifier(secret []byte) TokenVerifier { return &hs256Verifier{secret: secret} }

// Compile-time assertion.
var _ TokenVerifier = (*hs256Verifier)(nil)

// Sign issues a signed HS256 JWT. The user's TokenGen snapshot is embedded in
// the claims so that Middleware can detect logout-all revocation by comparing
// against the live store value.
func (v *hs256Verifier) Sign(user User) (string, error) {
	now := time.Now()
	hdr, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	c := jwtClaims{
		UserID:     user.ID,
		Email:      user.Email,
		SystemRole: user.SystemRole,
		TokenGen:   user.TokenGen,
		Iss:        tokenIssuer,
		Aud:        tokenAudience,
		Exp:        now.Add(DefaultTokenTTL).Unix(),
		Iat:        now.Unix(),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	unsigned := b64.EncodeToString(hdr) + "." + b64.EncodeToString(payload)
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + b64.EncodeToString(mac.Sum(nil)), nil
}

// Verify validates a compact JWS token (header.payload.signature). It
// explicitly rejects alg=none and any asymmetric or unknown alg to prevent
// alg-confusion attacks. Signature is verified before claims are inspected.
// If valid, returns the embedded User (with TokenGen snapshot).
func (v *hs256Verifier) Verify(token string) (User, error) {
	return v.verifyAt(token, time.Now())
}

// verifyAt is the internal implementation; accepts an explicit "now" so tests
// can inject a synthetic clock without wrapping time.Now globally.
func (v *hs256Verifier) verifyAt(token string, now time.Time) (User, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return User{}, ErrMalformed
	}

	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return User{}, ErrMalformed
	}
	var h jwtHeader
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return User{}, ErrMalformed
	}
	// Accept ONLY HS256. Explicitly reject alg=none (unsigned token), RS*/ES*/PS*
	// (asymmetric confusion), hs256 (case-sensitive), and anything else unknown.
	if h.Alg != "HS256" {
		return User{}, ErrAlg
	}

	payloadJSON, err := b64.DecodeString(parts[1])
	if err != nil {
		return User{}, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return User{}, ErrMalformed
	}

	// Verify HMAC-SHA256 signature before touching any claims (fail fast,
	// no claim data exposed on signature failure).
	unsigned := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(unsigned))
	want := mac.Sum(nil)
	if !hmac.Equal(sig, want) {
		return User{}, ErrSignature
	}

	var c jwtClaims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return User{}, ErrMalformed
	}
	if c.Exp != 0 && now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return User{}, ErrExpired
	}
	if c.Iss != tokenIssuer {
		return User{}, ErrIssuer
	}
	// Audience: missing = backward-compatible legacy token (no rejection);
	// present but wrong = reject (prevents token-type confusion).
	if c.Aud != "" && c.Aud != tokenAudience {
		return User{}, ErrAudience
	}

	return User{
		ID:         c.UserID,
		Email:      c.Email,
		SystemRole: c.SystemRole,
		TokenGen:   c.TokenGen,
	}, nil
}
