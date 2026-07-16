package registrytoken

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token type / algorithm sentinels and defaults.
const (
	// tokenTyp is the JOSE header "typ".
	tokenTyp = "JWT"
	// tokenAlg is the ONLY accepted signing algorithm. alg=none and every
	// symmetric/other-asymmetric family are rejected on verify (alg-confusion
	// defence, matching internal/auth's HS256 verifier philosophy).
	tokenAlg = "RS256"
	// DefaultTTL is the lifetime of a freshly minted registry token. Short by
	// design: a token authorizes a specific push/pull burst, not a session.
	DefaultTTL = 5 * time.Minute
	// clockSkew absorbs minor inter-node clock drift on exp/nbf checks.
	clockSkew = 60 * time.Second
)

// b64 is the JWT-standard unpadded base64url codec for all three segments.
// Used internally by verifyAt for the manual alg pre-check.
var b64 = base64.RawURLEncoding

// Verify sentinel errors.
var (
	ErrMalformed = errors.New("registrytoken: malformed token")
	ErrAlg       = errors.New("registrytoken: unexpected signing alg")
	ErrSignature = errors.New("registrytoken: signature invalid")
	ErrExpired   = errors.New("registrytoken: token expired")
	ErrNotYet    = errors.New("registrytoken: token not yet valid")
	ErrIssuer    = errors.New("registrytoken: unexpected issuer")
	ErrAudience  = errors.New("registrytoken: unexpected audience")
)

// Access is one Docker Distribution resource-scope entry. For the registry it
// is always Type=="repository", Name=="<org>/<repo>", Actions⊆{pull,push,delete}.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// jwtHeader holds only the fields we care about from the JOSE header.
// Used by verifyAt for the manual alg pre-check before the library runs.
type jwtHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
}

// registryClaims is the registry token payload (Distribution token spec shape).
// Access carries the Docker Distribution resource-scope list; jwt.RegisteredClaims
// provides the standard iss/sub/aud/exp/nbf/iat/jti fields in their RFC 7519
// positions with the correct wire names.
type registryClaims struct {
	Access []Access `json:"access"`
	jwt.RegisteredClaims
}

// AccessClaims is the verified, decoded view returned by Verify: who the token
// is for and what repository actions it authorizes.
type AccessClaims struct {
	Subject   string    // acl subject string the token was minted for ("" = anonymous)
	Access    []Access  // authorized repository scopes
	ExpiresAt time.Time // exp
}

// Allows reports whether the claims authorize action on repository repoName.
func (c *AccessClaims) Allows(repoName, action string) bool {
	for _, a := range c.Access {
		if a.Type != "repository" || a.Name != repoName {
			continue
		}
		for _, act := range a.Actions {
			if act == action {
				return true
			}
		}
	}
	return false
}

// Service mints and verifies RS256 registry tokens.
type Service struct {
	priv    *rsa.PrivateKey
	issuer  string        // token "iss" (this registry's identity)
	service string        // token "aud" (the service name in the WWW-Authenticate challenge)
	ttl     time.Duration // token lifetime
}

// NewService constructs a token Service. issuer is the registry's own identity
// (iss claim); service is the audience name that must match the docker client's
// ?service= parameter; ttl<=0 falls back to DefaultTTL.
func NewService(priv *rsa.PrivateKey, issuer, service string, ttl time.Duration) *Service {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Service{priv: priv, issuer: issuer, service: service, ttl: ttl}
}

// Service returns the configured service (audience) name.
func (s *Service) Service() string { return s.service }

// PublicKey returns the RSA public key token consumers can verify with.
func (s *Service) PublicKey() *rsa.PublicKey { return &s.priv.PublicKey }

// Mint issues a signed RS256 token for subject authorizing the given access
// scopes. subject "" mints an anonymous token (only ever carrying pull access
// to public repositories, per the authorizer). access may be empty (a valid
// token with no grants — docker accepts it and the request then 401/403s at
// the data plane).
func (s *Service) Mint(subject string, access []Access) (string, error) {
	now := time.Now()
	claims := registryClaims{
		Access: access,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{s.service},
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
			NotBefore: jwt.NewNumericDate(now.Add(-clockSkew)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        randomJTI(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.priv)
}

// Verify validates a compact RS256 JWS and returns its access claims. It
// rejects any alg other than RS256, a bad signature, expired/not-yet-valid
// windows (± clockSkew), a wrong issuer, and a present-but-wrong audience.
func (s *Service) Verify(token string) (*AccessClaims, error) {
	return s.verifyAt(token, time.Now())
}

func (s *Service) verifyAt(token string, now time.Time) (*AccessClaims, error) {
	// ── Step 1: manual alg pre-check ────────────────────────────────────────
	// Inspect the JOSE header before the library begins any cryptographic work.
	// This is the primary alg-confusion defence: reject anything that is not
	// exactly "RS256" (case-sensitive, per RFC 7518) up front.
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrMalformed
	}
	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, ErrMalformed
	}
	if hdr.Alg != tokenAlg {
		return nil, ErrAlg
	}

	// ── Step 2: library signature + expiry/nbf verification ─────────────────
	// WithValidMethods adds a second alg layer inside the library.
	// WithLeeway applies clockSkew tolerance to both exp and nbf.
	// WithTimeFunc injects the caller's "now" for clock-controlled testing.
	// Issuer and audience are NOT validated by the parser — done manually in
	// Step 3 so we return our own typed sentinel errors.
	var claims registryClaims
	_, parseErr := jwt.NewParser(
		jwt.WithValidMethods([]string{tokenAlg}),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(func() time.Time { return now }),
	).ParseWithClaims(token, &claims, func(_ *jwt.Token) (interface{}, error) {
		return &s.priv.PublicKey, nil
	})
	if parseErr != nil {
		return nil, mapRS256ParseError(parseErr)
	}

	// ── Step 3: application-level claim checks ───────────────────────────────
	if s.issuer != "" && claims.Issuer != s.issuer {
		return nil, ErrIssuer
	}
	// Audience: if s.service is set and the token carries a non-empty audience
	// that does not include our service name, reject. A missing/empty audience
	// claim is accepted (legacy tokens without aud).
	if s.service != "" && len(claims.Audience) > 0 {
		if !containsString([]string(claims.Audience), s.service) {
			return nil, ErrAudience
		}
	}

	var expiresAt time.Time
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	}

	return &AccessClaims{
		Subject:   claims.Subject,
		Access:    claims.Access,
		ExpiresAt: expiresAt,
	}, nil
}

// mapRS256ParseError converts a golang-jwt parsing error to one of our typed
// sentinel errors. Because the alg pre-check has already passed, any
// ErrTokenSignatureInvalid here reflects a genuine RSA key mismatch.
func mapRS256ParseError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return ErrMalformed
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return ErrSignature
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ErrNotYet
	case errors.Is(err, jwt.ErrTokenUnverifiable):
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

// randomJTI returns a 16-byte hex token id for replay/audit distinctness.
func randomJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
