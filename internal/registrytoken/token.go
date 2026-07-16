package registrytoken

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
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

// jwtHeader is the JOSE header we emit / accept.
type jwtHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
}

// jwtClaims is the registry token payload (Distribution token spec shape).
type jwtClaims struct {
	Iss    string   `json:"iss"`
	Sub    string   `json:"sub"`
	Aud    string   `json:"aud"`
	Exp    int64    `json:"exp"`
	Nbf    int64    `json:"nbf"`
	Iat    int64    `json:"iat"`
	Jti    string   `json:"jti"`
	Access []Access `json:"access"`
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
// token with no grants — docker accepts it and the request then 401/403s at the
// data plane).
func (s *Service) Mint(subject string, access []Access) (string, error) {
	now := time.Now()
	hdr, err := json.Marshal(jwtHeader{Typ: tokenTyp, Alg: tokenAlg})
	if err != nil {
		return "", err
	}
	claims := jwtClaims{
		Iss:    s.issuer,
		Sub:    subject,
		Aud:    s.service,
		Exp:    now.Add(s.ttl).Unix(),
		Nbf:    now.Add(-clockSkew).Unix(),
		Iat:    now.Unix(),
		Jti:    randomJTI(),
		Access: access,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64.EncodeToString(hdr) + "." + b64.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// Verify validates a compact RS256 JWS and returns its access claims. It
// rejects any alg other than RS256, a bad signature, expired/not-yet-valid
// windows (± clockSkew), a wrong issuer, and a present-but-wrong audience.
func (s *Service) Verify(token string) (*AccessClaims, error) {
	return s.verifyAt(token, time.Now())
}

func (s *Service) verifyAt(token string, now time.Time) (*AccessClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrMalformed
	}

	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h jwtHeader
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return nil, ErrMalformed
	}
	if h.Alg != tokenAlg {
		return nil, ErrAlg
	}

	payloadJSON, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}

	// Verify signature before inspecting any claim (fail fast).
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&s.priv.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return nil, ErrSignature
	}

	var c jwtClaims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return nil, ErrMalformed
	}
	if c.Exp != 0 && now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return nil, ErrExpired
	}
	if c.Nbf != 0 && now.Add(clockSkew).Before(time.Unix(c.Nbf, 0)) {
		return nil, ErrNotYet
	}
	if s.issuer != "" && c.Iss != s.issuer {
		return nil, ErrIssuer
	}
	if s.service != "" && c.Aud != "" && c.Aud != s.service {
		return nil, ErrAudience
	}

	return &AccessClaims{
		Subject:   c.Sub,
		Access:    c.Access,
		ExpiresAt: time.Unix(c.Exp, 0),
	}, nil
}

// randomJTI returns a 16-byte hex token id for replay/audit distinctness.
func randomJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
