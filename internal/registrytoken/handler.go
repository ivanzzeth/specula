package registrytoken

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/apikey"
)

// Registry token actions (Docker Distribution scope actions).
const (
	ActionPull   = "pull"
	ActionPush   = "push"
	ActionDelete = "delete"
)

// Principal is the caller resolved from /token Basic credentials. For an API
// key the org is pinned (Subject="apikey:<id>", OrgID set); for a password user
// the caller may belong to many orgs, so OrgID is empty and Email carries the
// identity the ScopeAuthorizer uses for per-org membership lookups. Anonymous is
// the credential-less caller (only ever granted pull on public repos).
type Principal struct {
	Subject   string // acl subject string ("user:<id>" | "apikey:<id>"); "" when Anonymous
	OrgID     string // pinned org for an API key; "" for a session/password user
	Email     string // login email for a password user; "" for an API key
	Anonymous bool
	// KeyScopes is set for API-key principals (normalised pull/push). Empty for
	// password users — the ScopeAuthorizer must not apply key-scope filtering then.
	KeyScopes []string
}

// Authenticator resolves /token Basic credentials into a Principal. The username
// is the login email; the secret is either an API key (spck_ prefix) or a
// password. ok=false means the credentials are invalid → 401.
type Authenticator interface {
	Authenticate(ctx context.Context, email, secret string) (Principal, bool)
}

// ScopeAuthorizer decides, per requested scope, which actions a principal may
// perform on a repository. Implementations bind repoName ("<org>/<repo>") to its
// owning org, load the repo's visibility, and consult acl.CanAccess — returning
// the subset of requested actions that are allowed. An anonymous principal must
// only ever receive pull on a public repo. Returning an empty slice denies the
// whole scope (the token is still minted, just without that grant).
type ScopeAuthorizer interface {
	GrantedActions(ctx context.Context, p Principal, repoName string, requested []string) []string
}

// TokenResponse is the /token JSON body (Docker Distribution token spec).
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"` // alias docker also reads
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// TokenHandler is the GET /token endpoint of the Bearer flow. It authenticates
// Basic credentials (or treats the caller as anonymous), authorizes each
// requested repository scope, and mints an RS256 token carrying the granted
// access.
type TokenHandler struct {
	svc   *Service
	authn Authenticator
	authz ScopeAuthorizer
}

// NewTokenHandler builds the /token handler. authn/authz are required; a nil
// authz denies every scope (fail-closed).
func NewTokenHandler(svc *Service, authn Authenticator, authz ScopeAuthorizer) *TokenHandler {
	return &TokenHandler{svc: svc, authn: authn, authz: authz}
}

// Compile-time assertion.
var _ http.Handler = (*TokenHandler)(nil)

// ServeHTTP implements GET /token?service=<svc>&scope=repository:<name>:<actions>[&scope=…].
//
// Flow (REGISTRY-DESIGN §3):
//  1. Resolve the principal from Basic auth (or anonymous when absent).
//  2. Parse the requested scopes.
//  3. For each scope, ask the ScopeAuthorizer for the granted actions.
//  4. Mint an RS256 token with the granted access and return it.
func (h *TokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	// Parse both the URL query (GET flow) and an x-www-form-urlencoded body
	// (OAuth2 POST flow). After this, r.Form carries the merged query+body params
	// and r.PostForm carries body-only params. ParseForm reads the body, which is
	// fine: the token endpoint never needs the raw body afterward.
	_ = r.ParseForm()

	// 1. Authenticate. Two client dialects are supported (REGISTRY-DESIGN §3):
	//   - GET  + HTTP Basic (email:secret)         — classic docker token flow.
	//   - POST + grant_type=password (username/    — OAuth2 token flow used by
	//     password form fields)                       containerd / the docker 29
	//                                                  image store.
	// Anonymous when neither is present. Invalid credentials → 401.
	principal := Principal{Anonymous: true}
	if email, secret, ok := r.BasicAuth(); ok {
		p, authOK := h.authenticate(ctx, email, secret)
		if !authOK {
			challengeUnauthorized(w, h.svc, "")
			return
		}
		principal = p
	} else if email, secret, ok := oauthPasswordCreds(r); ok {
		p, authOK := h.authenticate(ctx, email, secret)
		if !authOK {
			challengeUnauthorized(w, h.svc, "")
			return
		}
		principal = p
	}

	// 2. Parse requested scopes from the merged query+body params. OAuth2 clients
	//    send one or more space-delimited "scope" fields; the GET flow sends
	//    repeated ?scope= query params. scopeValues flattens both shapes.
	requested := ParseScopes(scopeValues(r.Form["scope"]))

	// 3. Authorize each scope; accumulate the granted access entries.
	var granted []Access
	for _, sc := range requested {
		actions := sc.Actions
		if h.authz != nil {
			actions = h.authz.GrantedActions(ctx, principal, sc.Name, sc.Actions)
		} else {
			actions = nil // no authorizer → deny (fail-closed)
		}
		if len(actions) == 0 {
			continue
		}
		granted = append(granted, Access{Type: "repository", Name: sc.Name, Actions: actions})
	}

	// 4. Mint. Even with no granted scopes a token is returned (docker login
	//    success is proven by 200 here; the data-plane request then decides).
	token, err := h.svc.Mint(principal.Subject, granted)
	if err != nil {
		http.Error(w, "token minting failed", http.StatusInternalServerError)
		return
	}
	writeTokenJSON(w, token, int(h.svc.ttl/time.Second))
}

// oauthPasswordCreds extracts credentials from an OAuth2 "password" grant token
// request (Docker Distribution OAuth2 flow, used by containerd / the docker image
// store): POST /token with form fields grant_type=password, username, password.
// ok=false for any other grant (e.g. refresh_token, which Specula does not issue)
// or when the fields are absent, leaving the caller anonymous.
func oauthPasswordCreds(r *http.Request) (email, secret string, ok bool) {
	if r.Method != http.MethodPost {
		return "", "", false
	}
	if r.PostForm.Get("grant_type") != "password" {
		return "", "", false
	}
	email = r.PostForm.Get("username")
	secret = r.PostForm.Get("password")
	if email == "" || secret == "" {
		return "", "", false
	}
	return email, secret, true
}

// scopeValues flattens raw scope parameters into individual scope tokens. A
// single OAuth2 "scope" field is space-delimited and may carry several scopes
// ("repository:a:pull repository:b:push"); the GET flow instead repeats the
// ?scope= query param. Both shapes reduce to a flat list of scope strings.
func scopeValues(raw []string) []string {
	var out []string
	for _, s := range raw {
		out = append(out, strings.Fields(s)...)
	}
	return out
}

// authenticate dispatches on the secret shape: an spck_-prefixed secret is an
// API key, anything else is treated as a password. The concrete Authenticator
// performs the actual verification.
func (h *TokenHandler) authenticate(ctx context.Context, email, secret string) (Principal, bool) {
	if h.authn == nil {
		return Principal{}, false
	}
	return h.authn.Authenticate(ctx, email, secret)
}

// ── /v2/ Bearer challenge middleware ─────────────────────────────────────────

// challengeCtxKey carries the verified AccessClaims into downstream handlers.
type challengeCtxKey struct{}

// ClaimsFromContext returns the AccessClaims the Challenge middleware attached,
// or (nil, false) for an anonymous/unauthenticated request that was allowed
// through (e.g. the /v2/ version probe or a public-repo pull).
func ClaimsFromContext(ctx context.Context) (*AccessClaims, bool) {
	c, ok := ctx.Value(challengeCtxKey{}).(*AccessClaims)
	return c, ok && c != nil
}

// Challenge is the /v2/ Bearer gate. realm is the absolute /token URL the client
// must call; the middleware's own service name comes from the Service.
//
// Behaviour:
//   - GET/HEAD /v2/ base probe with no/invalid token → 401 with a bare
//     WWW-Authenticate: Bearer realm,service challenge (drives docker login).
//   - A repository request with a valid Bearer whose claims cover the required
//     (repo, action) → passes through with claims in context.
//   - A repository request with no/invalid/insufficient token → 401 challenge
//     carrying the required scope so the client can fetch a scoped token. Public
//     anonymous pull is enabled by the /token endpoint issuing an anon token
//     with pull access for public repos, which then satisfies this check.
func (s *Service) Challenge(realm string) func(http.Handler) http.Handler {
	return s.ChallengeFunc(func(*http.Request) string { return realm })
}

// ChallengeFunc is Challenge with a per-request realm. realmFor computes the
// absolute /token URL to advertise in the WWW-Authenticate header from the
// incoming request (e.g. "http://"+r.Host+"/token"), so a single binary serves
// a correct same-origin realm regardless of the host/port the client used. All
// gating behaviour is identical to Challenge.
func (s *Service) ChallengeFunc(realmFor func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			realm := realmFor(r)
			repoName, action, isRepoReq := ParseResourceScope(r.Method, r.URL.Path)

			token := bearerToken(r)
			if token == "" {
				s.writeChallenge(w, realm, repoName, action, isRepoReq, http.StatusUnauthorized)
				return
			}
			claims, err := s.Verify(token)
			if err != nil {
				s.writeChallenge(w, realm, repoName, action, isRepoReq, http.StatusUnauthorized)
				return
			}
			// Base probe (/v2/ or /v2): a valid token is sufficient, no scope needed.
			if isRepoReq && !claims.Allows(repoName, action) {
				// Authenticated but the token lacks the required scope. Per the
				// Docker Distribution token-auth flow this is a 401 with a Bearer
				// challenge carrying the required scope — NOT a 403. The docker CLI
				// obtains a base (scopeless) token at login and only escalates to a
				// scoped token when it sees a 401 challenge for a specific
				// repo+action; it treats a 403 as a hard, non-retryable denial and
				// gives up. Returning 401 here lets the client fetch a correctly
				// scoped token (carrying its Basic credentials) and retry — which is
				// how `docker push`/`docker pull` acquire per-repo scope. A genuinely
				// forbidden caller simply receives a token with no matching grant and
				// fails after the retry. (go-containerregistry pre-scopes its token
				// request, so it never reaches this branch.)
				s.writeChallenge(w, realm, repoName, action, isRepoReq, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), challengeCtxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeChallenge emits a 401/403 with the WWW-Authenticate: Bearer header. When
// the request targets a repository, the required scope is included so the client
// requests a correctly-scoped token on its retry.
func (s *Service) writeChallenge(w http.ResponseWriter, realm, repoName, action string, isRepoReq bool, status int) {
	challengeWithScope(w, realm, s.service, repoName, action, isRepoReq, status)
}

// ── convenience Authenticator ────────────────────────────────────────────────

// PasswordVerifier verifies email:password credentials, returning the caller's
// acl user-subject string ("user:<id>") on success. Wire it from auth.Service.
type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, email, password string) (userSubject string, ok bool)
}

// BasicAuthenticator is a ready-made Authenticator that routes an spck_-prefixed
// secret to an apikey.Store and anything else to a PasswordVerifier. It is pure
// glue over the two reused primitives (REGISTRY-DESIGN §3); either dependency may
// be nil to disable that path.
type BasicAuthenticator struct {
	Keys      apikey.Store
	Passwords PasswordVerifier
}

// Compile-time assertion.
var _ Authenticator = (*BasicAuthenticator)(nil)

// Authenticate resolves credentials into a Principal.
func (a *BasicAuthenticator) Authenticate(ctx context.Context, email, secret string) (Principal, bool) {
	if strings.HasPrefix(secret, apikey.KeyPrefix) {
		if a.Keys == nil {
			return Principal{}, false
		}
		info, ok := a.Keys.LookupKey(secret)
		if !ok {
			return Principal{}, false
		}
		return Principal{
			Subject:   apikey.SubjectID(info.ID),
			OrgID:     info.OrgID,
			Email:     email,
			KeyScopes: info.Scopes,
		}, true
	}
	if a.Passwords == nil {
		return Principal{}, false
	}
	subject, ok := a.Passwords.VerifyPassword(ctx, email, secret)
	if !ok {
		return Principal{}, false
	}
	return Principal{Subject: subject, Email: email}, true
}

// ── helpers ──────────────────────────────────────────────────────────────────

// writeTokenJSON emits the /token success body.
func writeTokenJSON(w http.ResponseWriter, token string, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   expiresIn,
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
	})
}

// challengeUnauthorized writes a bare 401 Bearer challenge (no scope), used when
// /token receives invalid credentials.
func challengeUnauthorized(w http.ResponseWriter, svc *Service, realm string) {
	challengeWithScope(w, realm, svc.service, "", "", false, http.StatusUnauthorized)
}

// challengeWithScope writes the WWW-Authenticate: Bearer header and status.
func challengeWithScope(w http.ResponseWriter, realm, service, repoName, action string, isRepoReq bool, status int) {
	var b strings.Builder
	b.WriteString(`Bearer realm="`)
	b.WriteString(realm)
	b.WriteString(`",service="`)
	b.WriteString(service)
	b.WriteString(`"`)
	if isRepoReq && repoName != "" {
		b.WriteString(`,scope="repository:`)
		b.WriteString(repoName)
		b.WriteString(`:`)
		b.WriteString(action)
		b.WriteString(`"`)
	}
	w.Header().Set("WWW-Authenticate", b.String())
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
}

// bearerToken extracts the token from an "Authorization: Bearer <t>" header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
