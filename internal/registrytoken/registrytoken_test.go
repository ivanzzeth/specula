// Package registrytoken_test holds black-box tests for the registrytoken
// package: mint/verify round-trip, scope enforcement, /token handler flows,
// and the /v2/ Bearer challenge middleware.
package registrytoken_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/registrytoken"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newTestSvc creates a Service backed by a freshly-generated RSA key in a temp
// directory. ttl=0 → DefaultTTL.
func newTestSvc(t *testing.T) *registrytoken.Service {
	t.Helper()
	key, err := registrytoken.EnsureKeyPair(t.TempDir() + "/reg.pem")
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	return registrytoken.NewService(key, "specula-test", "specula", 0)
}

// ── EnsureKeyPair ─────────────────────────────────────────────────────────────

func TestEnsureKeyPairGenerateAndReload(t *testing.T) {
	path := t.TempDir() + "/reg.pem"

	k1, err := registrytoken.EnsureKeyPair(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call must return the same key (not regenerate).
	k2, err := registrytoken.EnsureKeyPair(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if k1.N.Cmp(k2.N) != 0 {
		t.Error("second load returned a different key")
	}
}

func TestEnsureKeyPairRejectsEmptyPath(t *testing.T) {
	if _, err := registrytoken.EnsureKeyPair(""); err == nil {
		t.Error("expected error for empty path; got nil")
	}
}

// ── Mint / Verify round-trip ──────────────────────────────────────────────────

func TestMintVerifyRoundtrip(t *testing.T) {
	svc := newTestSvc(t)
	access := []registrytoken.Access{
		{Type: "repository", Name: "myorg/app", Actions: []string{"pull", "push"}},
	}

	tok, err := svc.Mint("user:42", access)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims, err := svc.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user:42" {
		t.Errorf("Subject = %q; want %q", claims.Subject, "user:42")
	}
	if !claims.Allows("myorg/app", "pull") {
		t.Error("should allow pull on myorg/app")
	}
	if !claims.Allows("myorg/app", "push") {
		t.Error("should allow push on myorg/app")
	}
	if claims.Allows("myorg/app", "delete") {
		t.Error("should NOT allow delete (not in minted access)")
	}
	if claims.Allows("myorg/other", "pull") {
		t.Error("should NOT allow pull on a different repo")
	}
}

func TestMintVerifyEmptyAccess(t *testing.T) {
	svc := newTestSvc(t)
	tok, err := svc.Mint("", nil) // anonymous, no scopes
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := svc.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "" {
		t.Errorf("anonymous subject should be empty; got %q", claims.Subject)
	}
	if len(claims.Access) != 0 {
		t.Errorf("expected no access grants; got %v", claims.Access)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	svc := newTestSvc(t)
	tok, _ := svc.Mint("user:1", nil)

	// Flip the last byte of the signature segment.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("unexpected token format")
	}
	sigBytes, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sigBytes[len(sigBytes)-1] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(sigBytes)
	bad := strings.Join(parts, ".")

	if _, err := svc.Verify(bad); err == nil {
		t.Error("expected error on tampered token; got nil")
	}
}

func TestVerifyRejectsTokenFromOtherKey(t *testing.T) {
	svc1 := newTestSvc(t)
	svc2 := newTestSvc(t) // different key

	tok, _ := svc1.Mint("user:1", nil)
	if _, err := svc2.Verify(tok); err == nil {
		t.Error("expected error verifying token signed by a different key; got nil")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	svc := newTestSvc(t)
	cases := []string{
		"",
		"not.a.jwt",
		"a.b",
		"a.b.c.d",
	}
	for _, tc := range cases {
		if _, err := svc.Verify(tc); err == nil {
			t.Errorf("Verify(%q): expected error; got nil", tc)
		}
	}
}

func TestVerifyRejectsWrongIssuer(t *testing.T) {
	key, _ := registrytoken.EnsureKeyPair(t.TempDir() + "/k.pem")
	svc1 := registrytoken.NewService(key, "issuer-A", "svc", 0)
	svc2 := registrytoken.NewService(key, "issuer-B", "svc", 0)

	tok, _ := svc1.Mint("user:1", nil)
	if _, err := svc2.Verify(tok); err == nil {
		t.Error("expected error for wrong issuer; got nil")
	}
}

// ── AccessClaims.Allows ───────────────────────────────────────────────────────

func TestAllows(t *testing.T) {
	svc := newTestSvc(t)
	tok, _ := svc.Mint("user:1", []registrytoken.Access{
		{Type: "repository", Name: "org/a", Actions: []string{"pull"}},
		{Type: "repository", Name: "org/b", Actions: []string{"pull", "push"}},
	})
	claims, _ := svc.Verify(tok)

	table := []struct {
		repo, action string
		want         bool
	}{
		{"org/a", "pull", true},
		{"org/a", "push", false},
		{"org/b", "pull", true},
		{"org/b", "push", true},
		{"org/b", "delete", false},
		{"org/c", "pull", false},
	}
	for _, tt := range table {
		got := claims.Allows(tt.repo, tt.action)
		if got != tt.want {
			t.Errorf("Allows(%q, %q) = %v; want %v", tt.repo, tt.action, got, tt.want)
		}
	}
}

// ── ParseScopes ───────────────────────────────────────────────────────────────

func TestParseScopes(t *testing.T) {
	cases := []struct {
		input []string
		want  []registrytoken.Scope
	}{
		{
			input: []string{"repository:myorg/app:pull,push"},
			want: []registrytoken.Scope{
				{Type: "repository", Name: "myorg/app", Actions: []string{"pull", "push"}},
			},
		},
		{
			input: []string{"repository:myorg/app:pull", "repository:myorg/other:push"},
			want: []registrytoken.Scope{
				{Type: "repository", Name: "myorg/app", Actions: []string{"pull"}},
				{Type: "repository", Name: "myorg/other", Actions: []string{"push"}},
			},
		},
		{
			input: nil,
			want:  []registrytoken.Scope{},
		},
		{
			input: []string{"", "  ", "bad", "a:b"},
			want:  []registrytoken.Scope{},
		},
	}
	for _, tt := range cases {
		got := registrytoken.ParseScopes(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("ParseScopes(%v): len=%d; want %d", tt.input, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i].Type != tt.want[i].Type || got[i].Name != tt.want[i].Name {
				t.Errorf("scope[%d]: got {%s %s}; want {%s %s}", i,
					got[i].Type, got[i].Name, tt.want[i].Type, tt.want[i].Name)
			}
		}
	}
}

// ── ParseResourceScope ────────────────────────────────────────────────────────

func TestParseResourceScope(t *testing.T) {
	cases := []struct {
		method, path  string
		wantRepo      string
		wantAction    string
		wantIsRepoReq bool
	}{
		{"GET", "/v2/", "", "", false},
		{"GET", "/v2", "", "", false},
		{"GET", "/v2/myorg/app/manifests/latest", "myorg/app", "pull", true},
		{"HEAD", "/v2/myorg/app/manifests/sha256:abc", "myorg/app", "pull", true},
		{"PUT", "/v2/myorg/app/manifests/v1.0", "myorg/app", "push", true},
		{"GET", "/v2/myorg/app/blobs/sha256:abc", "myorg/app", "pull", true},
		{"DELETE", "/v2/myorg/app/manifests/v1.0", "myorg/app", "delete", true},
		{"POST", "/v2/myorg/app/blobs/uploads/", "myorg/app", "push", true},
		{"GET", "/v2/myorg/app/tags/list", "myorg/app", "pull", true},
		{"GET", "/unknown", "", "", false},
	}
	for _, tt := range cases {
		repo, action, isRepo := registrytoken.ParseResourceScope(tt.method, tt.path)
		if repo != tt.wantRepo || action != tt.wantAction || isRepo != tt.wantIsRepoReq {
			t.Errorf("ParseResourceScope(%q, %q) = (%q, %q, %v); want (%q, %q, %v)",
				tt.method, tt.path, repo, action, isRepo,
				tt.wantRepo, tt.wantAction, tt.wantIsRepoReq)
		}
	}
}

// ── stubs ─────────────────────────────────────────────────────────────────────

// stubScopeAuthz grants all requested actions to authenticated callers; for
// anonymous callers it only grants pull on repos in publicRepos.
type stubScopeAuthz struct {
	publicRepos map[string]bool
}

func (s *stubScopeAuthz) GrantedActions(_ context.Context, p registrytoken.Principal, repoName string, requested []string) []string {
	if p.Anonymous {
		if !s.publicRepos[repoName] {
			return nil
		}
		var out []string
		for _, a := range requested {
			if a == registrytoken.ActionPull {
				out = append(out, a)
			}
		}
		return out
	}
	return requested
}

// stubPasswordVerifier maps email→password for tests; returns "user:1" on match.
type stubPasswordVerifier struct {
	creds map[string]string // email → password
}

func (v *stubPasswordVerifier) VerifyPassword(_ context.Context, email, password string) (string, bool) {
	if pw, ok := v.creds[email]; ok && pw == password {
		return "user:1", true
	}
	return "", false
}

// ok returns the HTTP status code from a response recorder.
func ok200(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusOK }

// basicHeader returns a Basic auth header value for email:password.
func basicHeader(email, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+password))
}

// ── /token handler: API-key auth ──────────────────────────────────────────────

func TestTokenHandlerAPIKeyAuth(t *testing.T) {
	svc := newTestSvc(t)
	keys := apikey.NewMemStore()
	_, rawKey, err := keys.Create("org1", "ci-key")
	if err != nil {
		t.Fatalf("Create key: %v", err)
	}

	authz := &stubScopeAuthz{publicRepos: map[string]bool{}}
	authn := &registrytoken.BasicAuthenticator{Keys: keys}
	h := registrytoken.NewTokenHandler(svc, authn, authz)

	req := httptest.NewRequest(http.MethodGet, "/token?service=specula&scope=repository:org1/app:pull,push", nil)
	req.Header.Set("Authorization", basicHeader("any@example.com", rawKey))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !ok200(rr) {
		t.Fatalf("expected 200; got %d — body: %s", rr.Code, rr.Body.String())
	}

	var resp registrytoken.TokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	claims, err := svc.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify issued token: %v", err)
	}
	if !claims.Allows("org1/app", "pull") {
		t.Error("issued token should allow pull on org1/app")
	}
	if !claims.Allows("org1/app", "push") {
		t.Error("issued token should allow push on org1/app")
	}
}

// TestTokenHandlerBadCredentials verifies that invalid credentials yield 401.
func TestTokenHandlerBadCredentials(t *testing.T) {
	svc := newTestSvc(t)
	authn := &registrytoken.BasicAuthenticator{
		Keys:      apikey.NewMemStore(),
		Passwords: &stubPasswordVerifier{creds: map[string]string{}},
	}
	h := registrytoken.NewTokenHandler(svc, authn, nil)

	req := httptest.NewRequest(http.MethodGet, "/token?service=specula", nil)
	req.Header.Set("Authorization", basicHeader("user@example.com", "wrongpassword"))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401; got %d", rr.Code)
	}
}

// TestTokenHandlerPasswordAuth verifies that email:password basic auth works.
func TestTokenHandlerPasswordAuth(t *testing.T) {
	svc := newTestSvc(t)
	pw := &stubPasswordVerifier{creds: map[string]string{"alice@example.com": "s3cr3t"}}
	authz := &stubScopeAuthz{publicRepos: map[string]bool{}}
	authn := &registrytoken.BasicAuthenticator{
		Keys:      apikey.NewMemStore(),
		Passwords: pw,
	}
	h := registrytoken.NewTokenHandler(svc, authn, authz)

	req := httptest.NewRequest(http.MethodGet, "/token?service=specula&scope=repository:myorg/app:pull", nil)
	req.Header.Set("Authorization", basicHeader("alice@example.com", "s3cr3t"))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !ok200(rr) {
		t.Fatalf("expected 200; got %d", rr.Code)
	}
	var resp registrytoken.TokenResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	claims, err := svc.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user:1" {
		t.Errorf("Subject = %q; want %q", claims.Subject, "user:1")
	}
}

// TestTokenHandlerOAuth2PasswordGrant verifies the OAuth2 form-encoded token
// flow used by containerd / the docker image store: POST /token with
// grant_type=password and username/password/scope form fields (no Basic header,
// no query params). This is the flow real `docker push` uses on modern docker.
func TestTokenHandlerOAuth2PasswordGrant(t *testing.T) {
	svc := newTestSvc(t)
	pw := &stubPasswordVerifier{creds: map[string]string{"alice@example.com": "s3cr3t"}}
	authz := &stubScopeAuthz{publicRepos: map[string]bool{}}
	authn := &registrytoken.BasicAuthenticator{Keys: apikey.NewMemStore(), Passwords: pw}
	h := registrytoken.NewTokenHandler(svc, authn, authz)

	// containerd sends two scope fields, one of them comma-joining the actions —
	// exercise both the repeated-field and combined-actions shapes.
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("service", "specula")
	form.Set("client_id", "containerd-client")
	form.Set("username", "alice@example.com")
	form.Set("password", "s3cr3t")
	form["scope"] = []string{"repository:myorg/app:pull", "repository:myorg/app:pull,push"}

	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !ok200(rr) {
		t.Fatalf("OAuth2 password grant: expected 200; got %d — body: %s", rr.Code, rr.Body.String())
	}
	var resp registrytoken.TokenResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	claims, err := svc.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user:1" {
		t.Errorf("Subject = %q; want %q", claims.Subject, "user:1")
	}
	if !claims.Allows("myorg/app", "pull") || !claims.Allows("myorg/app", "push") {
		t.Errorf("OAuth2-granted token must allow pull+push on myorg/app; access=%+v", claims.Access)
	}
}

// TestTokenHandlerOAuth2BadCredentials verifies the OAuth2 password grant with
// invalid credentials yields 401 (not an anonymous token).
func TestTokenHandlerOAuth2BadCredentials(t *testing.T) {
	svc := newTestSvc(t)
	pw := &stubPasswordVerifier{creds: map[string]string{}}
	authn := &registrytoken.BasicAuthenticator{Keys: apikey.NewMemStore(), Passwords: pw}
	h := registrytoken.NewTokenHandler(svc, authn, &stubScopeAuthz{})

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", "nobody@example.com")
	form.Set("password", "wrong")
	form.Set("scope", "repository:myorg/app:pull")
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("OAuth2 bad credentials: expected 401; got %d", rr.Code)
	}
}

// ── /token handler: anonymous public pull vs private repo ────────────────────

func TestTokenHandlerAnonymousPublicPull(t *testing.T) {
	svc := newTestSvc(t)
	authz := &stubScopeAuthz{publicRepos: map[string]bool{"public-org/images": true}}
	h := registrytoken.NewTokenHandler(svc, &registrytoken.BasicAuthenticator{}, authz)

	// Anonymous (no Authorization header) requesting pull on a public repo.
	req := httptest.NewRequest(http.MethodGet, "/token?service=specula&scope=repository:public-org/images:pull", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !ok200(rr) {
		t.Fatalf("expected 200 for anonymous public pull; got %d", rr.Code)
	}
	var resp registrytoken.TokenResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	claims, err := svc.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Anonymous token must allow pull on the public repo.
	if !claims.Allows("public-org/images", "pull") {
		t.Error("anonymous token should allow pull on a public repo")
	}
	// Anonymous token must NOT allow push, ever.
	if claims.Allows("public-org/images", "push") {
		t.Error("anonymous token must NOT allow push on any repo")
	}
}

func TestTokenHandlerAnonymousPrivateRepo(t *testing.T) {
	svc := newTestSvc(t)
	// No repos are public — all are private.
	authz := &stubScopeAuthz{publicRepos: map[string]bool{}}
	h := registrytoken.NewTokenHandler(svc, &registrytoken.BasicAuthenticator{}, authz)

	req := httptest.NewRequest(http.MethodGet, "/token?service=specula&scope=repository:myorg/private:pull", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Token endpoint returns 200 even for anonymous with zero grants (docker
	// login expects 200; the data-plane challenge then 401/403s).
	if !ok200(rr) {
		t.Fatalf("expected 200 from /token; got %d", rr.Code)
	}
	var resp registrytoken.TokenResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	claims, err := svc.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// No access should be granted for the private repo.
	if claims.Allows("myorg/private", "pull") {
		t.Error("anonymous token must NOT allow pull on a private repo")
	}
}

// ── Challenge middleware ───────────────────────────────────────────────────────

// sentinelHandler responds 200 with "OK" so test cases can confirm pass-through.
var sentinelHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
})

func TestChallengeNoToken(t *testing.T) {
	svc := newTestSvc(t)
	h := svc.Challenge("https://specula/token")(sentinelHandler)

	req := httptest.NewRequest(http.MethodGet, "/v2/myorg/app/manifests/latest", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: expected 401; got %d", rr.Code)
	}
	wwa := rr.Header().Get("WWW-Authenticate")
	if !strings.Contains(wwa, "Bearer") {
		t.Errorf("WWW-Authenticate missing Bearer: %q", wwa)
	}
	if !strings.Contains(wwa, "realm=") {
		t.Errorf("WWW-Authenticate missing realm: %q", wwa)
	}
}

func TestChallengeValidTokenWithMatchingScope(t *testing.T) {
	svc := newTestSvc(t)
	tok, _ := svc.Mint("user:1", []registrytoken.Access{
		{Type: "repository", Name: "myorg/app", Actions: []string{"pull"}},
	})
	h := svc.Challenge("https://specula/token")(sentinelHandler)

	req := httptest.NewRequest(http.MethodGet, "/v2/myorg/app/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("valid scoped token: expected 200; got %d — body: %s", rr.Code, rr.Body.String())
	}
}

func TestChallengeValidTokenMissingScope(t *testing.T) {
	svc := newTestSvc(t)
	// Token only has pull, but the request is a PUT (push).
	tok, _ := svc.Mint("user:1", []registrytoken.Access{
		{Type: "repository", Name: "myorg/app", Actions: []string{"pull"}},
	})
	h := svc.Challenge("https://specula/token")(sentinelHandler)

	req := httptest.NewRequest(http.MethodPut, "/v2/myorg/app/manifests/v1.0", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// A valid token that lacks the required scope gets a 401 Bearer challenge
	// (carrying the required scope), not a 403: the Docker CLI escalates scope on
	// 401 but treats 403 as a hard denial. The challenge must advertise the scope
	// so the client can fetch a correctly scoped token and retry.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("insufficient scope: expected 401 challenge; got %d", rr.Code)
	}
	if wa := rr.Header().Get("WWW-Authenticate"); !strings.Contains(wa, `scope="repository:myorg/app:push"`) {
		t.Errorf("insufficient-scope challenge must advertise the required scope; got %q", wa)
	}
}

func TestChallengeInvalidToken(t *testing.T) {
	svc := newTestSvc(t)
	h := svc.Challenge("https://specula/token")(sentinelHandler)

	req := httptest.NewRequest(http.MethodGet, "/v2/myorg/app/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer this.is.not.valid")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: expected 401; got %d", rr.Code)
	}
}

func TestChallengeVersionProbePassesWithValidToken(t *testing.T) {
	svc := newTestSvc(t)
	// A valid token for any scope satisfies the /v2/ version probe.
	tok, _ := svc.Mint("user:1", nil)
	h := svc.Challenge("https://specula/token")(sentinelHandler)

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("/v2/ with valid token: expected 200; got %d", rr.Code)
	}
}

func TestClaimsFromContext(t *testing.T) {
	svc := newTestSvc(t)
	access := []registrytoken.Access{
		{Type: "repository", Name: "org/r", Actions: []string{"pull"}},
	}
	tok, _ := svc.Mint("user:99", access)

	var gotClaims *registrytoken.AccessClaims
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		c, ok := registrytoken.ClaimsFromContext(r.Context())
		if !ok || c == nil {
			return
		}
		gotClaims = c
	})
	h := svc.Challenge("https://specula/token")(inner)

	req := httptest.NewRequest(http.MethodGet, "/v2/org/r/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotClaims == nil {
		t.Fatal("ClaimsFromContext: expected claims in context; got nil")
	}
	if gotClaims.Subject != "user:99" {
		t.Errorf("context claims Subject = %q; want %q", gotClaims.Subject, "user:99")
	}
}

// ── scope enforcement: anonymous public pull→allowed, private→denied ──────────

// TestAnonymousPublicPullVsPrivate401 is the end-to-end scenario that exercises
// the full round-trip: /token (anon) → Bearer challenge on GET manifests.
func TestAnonymousPublicPullVsPrivate401(t *testing.T) {
	svc := newTestSvc(t)
	authz := &stubScopeAuthz{publicRepos: map[string]bool{"pub-org/images": true}}
	h := registrytoken.NewTokenHandler(svc, &registrytoken.BasicAuthenticator{}, authz)

	// 1. Fetch a token for the public repo (anonymous).
	req := httptest.NewRequest(http.MethodGet, "/token?service=specula&scope=repository:pub-org/images:pull", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !ok200(rr) {
		t.Fatalf("/token for public repo returned %d", rr.Code)
	}
	var resp registrytoken.TokenResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	pubToken := resp.Token

	// 2. Use that token against the Challenge middleware for a public repo pull.
	chall := svc.Challenge("https://specula/token")(sentinelHandler)
	pullReq := httptest.NewRequest(http.MethodGet, "/v2/pub-org/images/manifests/latest", nil)
	pullReq.Header.Set("Authorization", "Bearer "+pubToken)
	pullRR := httptest.NewRecorder()
	chall.ServeHTTP(pullRR, pullReq)
	if pullRR.Code != http.StatusOK {
		t.Errorf("public pull with valid anon token: expected 200; got %d", pullRR.Code)
	}

	// 3. Use the same token against a private repo → 401 with a Bearer challenge
	//    (token is valid but doesn't include the private-repo scope). This is a
	//    401 rather than a 403 so a Docker CLI client re-authenticates for the
	//    challenged scope instead of giving up; an anonymous caller then simply
	//    gets a token with no private grant and fails after the retry.
	privReq := httptest.NewRequest(http.MethodGet, "/v2/myorg/secret/manifests/latest", nil)
	privReq.Header.Set("Authorization", "Bearer "+pubToken)
	privRR := httptest.NewRecorder()
	chall.ServeHTTP(privRR, privReq)
	if privRR.Code != http.StatusUnauthorized {
		t.Errorf("private repo pull with anon-scoped token: expected 401 challenge; got %d", privRR.Code)
	}

	// 4. No token at all for a private repo → 401.
	noTokReq := httptest.NewRequest(http.MethodGet, "/v2/myorg/secret/manifests/latest", nil)
	noTokRR := httptest.NewRecorder()
	chall.ServeHTTP(noTokRR, noTokReq)
	if noTokRR.Code != http.StatusUnauthorized {
		t.Errorf("no token private repo: expected 401; got %d", noTokRR.Code)
	}
}
