package auth

// principal_test.go exercises PrincipalMiddleware (API-key path, JWT path,
// anonymous path, frozen org, store errors) and the three context accessors.
// All tests assert the fail-closed posture documented in REGISTRY-DESIGN §3
// and ARCHITECTURE §11.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/org"
)

// ── context accessor tests ────────────────────────────────────────────────────

// TestContextAccessors_EmptyContext verifies the three accessors all return the
// zero/false result on a plain context with no middleware applied. This confirms
// they are safe to call before PrincipalMiddleware runs.
func TestContextAccessors_EmptyContext(t *testing.T) {
	ctx := context.Background()

	if _, ok := SubjectFromContext(ctx); ok {
		t.Error("SubjectFromContext on empty context must return ok=false")
	}
	if _, ok := OrgFromContext(ctx); ok {
		t.Error("OrgFromContext on empty context must return ok=false")
	}
	if _, ok := ActiveOrgFromContext(ctx); ok {
		t.Error("ActiveOrgFromContext on empty context must return ok=false")
	}
}

// ── helper types for error injection ─────────────────────────────────────────

// errOrgResolver wraps a *org.MemStore and optionally overrides GetOrg and
// GetOrgMember to return injected errors, exercising the 503 / 403 error paths
// in PrincipalMiddleware.
type errOrgResolver struct {
	base         *org.MemStore
	getOrgErr    error // if non-nil, GetOrg returns this
	getMemberErr error // if non-nil, GetOrgMember returns this
}

func (r *errOrgResolver) GetOrg(ctx context.Context, id string) (*org.Org, error) {
	if r.getOrgErr != nil {
		return nil, r.getOrgErr
	}
	return r.base.GetOrg(ctx, id)
}

func (r *errOrgResolver) GetOrgMember(ctx context.Context, orgID, email string) (*org.Member, error) {
	if r.getMemberErr != nil {
		return nil, r.getMemberErr
	}
	return r.base.GetOrgMember(ctx, orgID, email)
}

func (r *errOrgResolver) ListOrgsForEmail(ctx context.Context, email string) ([]*org.Org, error) {
	return r.base.ListOrgsForEmail(ctx, email)
}

// ── builder helpers ───────────────────────────────────────────────────────────

// principalMW builds a PrincipalMiddleware with the given parts, wrapping an
// okHandler that records the acl.Subject and org ID it sees in context.
type mwResult struct {
	code      int
	subject   acl.Subject
	subjectOK bool
	orgID     string
	orgIDOK   bool
	activeOrg *org.Org
	activeOK  bool
	user      User
	userOK    bool
}

func runPrincipalMW(
	t *testing.T,
	req *http.Request,
	keys apikey.Store,
	orgs OrgResolver,
	verifier TokenVerifier,
	users UserStore,
) mwResult {
	t.Helper()

	var res mwResult
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.subject, res.subjectOK = SubjectFromContext(r.Context())
		res.orgID, res.orgIDOK = OrgFromContext(r.Context())
		res.activeOrg, res.activeOK = ActiveOrgFromContext(r.Context())
		res.user, res.userOK = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := PrincipalMiddleware(keys, orgs, verifier, users)
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)
	res.code = rec.Code
	return res
}

// ── anonymous path ────────────────────────────────────────────────────────────

func TestPrincipalMiddleware_Anonymous(t *testing.T) {
	svc, store := newTestService(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil) // no token

	res := runPrincipalMW(t, req, nil, nil, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("anonymous: want 200, got %d", res.code)
	}
	// An anonymous caller gets an empty Subject (public-read only).
	if res.subject.UserID != "" {
		t.Errorf("anonymous: Subject.UserID=%q, want empty", res.subject.UserID)
	}
	// No active org.
	if res.orgIDOK {
		t.Error("anonymous: OrgFromContext should return false")
	}
	if res.activeOK {
		t.Error("anonymous: ActiveOrgFromContext should return false")
	}
}

// ── JWT path ──────────────────────────────────────────────────────────────────

func TestPrincipalMiddleware_JWT_ValidNoOrg(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "jwt@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, u, err := svc.Login(ctx, "jwt@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	res := runPrincipalMW(t, req, nil, nil, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("valid JWT no org: want 200, got %d", res.code)
	}
	// Subject is set (user: subject string from user ID).
	if res.subject.UserID == "" {
		t.Error("JWT: Subject.UserID must not be empty")
	}
	// No X-Org-Id header → no active org (no default-org fallback).
	if res.orgIDOK {
		t.Error("JWT without X-Org-Id: OrgFromContext must return false (no phantom org)")
	}
	// auth.User is also injected.
	if !res.userOK || res.user.ID != u.ID {
		t.Errorf("JWT: UserFromContext mismatch: got %v %v", res.userOK, res.user.ID)
	}
}

func TestPrincipalMiddleware_JWT_InvalidToken_Returns401(t *testing.T) {
	svc, store := newTestService(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid.jwt.token")

	res := runPrincipalMW(t, req, nil, nil, svc.verifier, store)
	if res.code != http.StatusUnauthorized {
		t.Fatalf("invalid JWT: want 401, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_RevokedToken_Returns401(t *testing.T) {
	// ARCHITECTURE §11: bumping token_gen invalidates in-flight sessions.
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "rev@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, u, err := svc.Login(ctx, "rev@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// Bump token_gen to revoke all sessions.
	if err := svc.Logout(ctx, u.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	res := runPrincipalMW(t, req, nil, nil, svc.verifier, store)
	if res.code != http.StatusUnauthorized {
		t.Fatalf("revoked token: want 401, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_WithOrg_MemberAccess(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "orguser@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "orguser@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_test", Name: "Test Org", Slug: "test", Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(ctx, &org.Member{
		OrgID: "org_test", Email: "orguser@example.com", Role: org.RoleEditor,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_test")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("org member: want 200, got %d", res.code)
	}
	if !res.orgIDOK || res.orgID != "org_test" {
		t.Errorf("org member: OrgFromContext got (%q, %v), want (org_test, true)", res.orgID, res.orgIDOK)
	}
	if !res.activeOK {
		t.Error("org member: ActiveOrgFromContext must return true")
	}
	if res.activeOrg.Role != org.RoleEditor {
		t.Errorf("org member: Role=%q, want editor", res.activeOrg.Role)
	}
}

func TestPrincipalMiddleware_JWT_WithOrg_OrgNotFound_Returns404(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "x@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "x@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore() // org "missing" does not exist

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "missing")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusNotFound {
		t.Fatalf("org not found: want 404, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_WithOrg_StoreError_Returns503(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "x@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "x@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Inject an unexpected GetOrg error (not ErrNotFound).
	orgStore := &errOrgResolver{
		base:      org.NewMemStore(),
		getOrgErr: errors.New("db unavailable"),
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "any-org")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("org store error: want 503, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_MemberStoreError_Returns503(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "x@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "x@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	base := org.NewMemStore()
	_ = base.CreateOrg(ctx, &org.Org{ID: "org_x", Name: "X", Slug: "x", Status: org.StatusActive})
	orgStore := &errOrgResolver{
		base:         base,
		getMemberErr: errors.New("membership db unavailable"),
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_x")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("member store error: want 503, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_NonMemberAdmin_CrossOrgRead(t *testing.T) {
	// ARCHITECTURE §11: system admin gets implicit cross-org read (SystemAccess=true).
	svc, store := newTestService(t)
	ctx := context.Background()
	// First registered user = system admin.
	if _, err := svc.Register(ctx, "admin@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "admin@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_other", Name: "Other Org", Slug: "other", Status: org.StatusActive,
	})
	// Admin is NOT a member of "org_other".

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_other")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("system admin cross-org: want 200, got %d", res.code)
	}
	if !res.activeOK || !res.activeOrg.SystemAccess {
		t.Error("system admin cross-org: ActiveOrg must have SystemAccess=true")
	}
	if res.activeOrg.Role != org.RoleViewer {
		t.Errorf("system admin cross-org: Role=%q, want viewer", res.activeOrg.Role)
	}
}

func TestPrincipalMiddleware_JWT_NonMemberNonAdmin_Returns403(t *testing.T) {
	// A non-admin user naming an org they do not belong to must get 403.
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "admin@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register admin: %v", err)
	}
	// Second user = non-admin.
	if _, err := svc.Register(ctx, "plain@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register plain: %v", err)
	}
	tok, _, err := svc.Login(ctx, "plain@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_other", Name: "Other", Slug: "other", Status: org.StatusActive,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_other")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusForbidden {
		t.Fatalf("non-member non-admin: want 403, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_FrozenOrg_Returns403(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	// Register first user (becomes system admin); they bypass frozen checks,
	// so we need a separate plain (non-admin) user for this test.
	if _, err := svc.Register(ctx, "admin@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register admin: %v", err)
	}
	// Second user is a plain member with no system admin role.
	if _, err := svc.Register(ctx, "member@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register member: %v", err)
	}
	tok, _, err := svc.Login(ctx, "member@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_frozen", Name: "Frozen", Slug: "frozen", Status: org.StatusFrozen,
	})
	_ = orgStore.AddOrgMember(ctx, &org.Member{
		OrgID: "org_frozen", Email: "member@example.com", Role: org.RoleEditor,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_frozen")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusForbidden {
		t.Fatalf("frozen org (member): want 403, got %d", res.code)
	}
}

func TestPrincipalMiddleware_JWT_FrozenOrg_SysAdmin_PassesThrough(t *testing.T) {
	// System admins retain a break-glass channel into frozen orgs.
	svc, store := newTestService(t)
	ctx := context.Background()
	// First user = system admin.
	if _, err := svc.Register(ctx, "admin@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "admin@example.com", "password123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	orgStore := org.NewMemStore()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_frozen", Name: "Frozen", Slug: "frozen", Status: org.StatusFrozen,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Org-Id", "org_frozen")

	res := runPrincipalMW(t, req, nil, orgStore, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("frozen org + sys-admin: want 200 (break-glass), got %d", res.code)
	}
}

// ── API-key path ──────────────────────────────────────────────────────────────

func TestPrincipalMiddleware_APIKey_Valid(t *testing.T) {
	svc, store := newTestService(t)

	orgStore := org.NewMemStore()
	ctx := context.Background()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_keyed", Slug: "keyed", Status: org.StatusActive,
	})

	keys := apikey.NewMemStore()
	_, rawKey, err := keys.Create("org_keyed", "ci-key")
	if err != nil {
		t.Fatalf("Create apikey: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)

	res := runPrincipalMW(t, req, keys, orgStore, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("api-key valid: want 200, got %d", res.code)
	}
	if res.subject.OrgID != "org_keyed" {
		t.Errorf("api-key: Subject.OrgID=%q, want org_keyed", res.subject.OrgID)
	}
	if res.subject.UserID == "" {
		t.Error("api-key: Subject.UserID must not be empty")
	}
	if !res.orgIDOK || res.orgID != "org_keyed" {
		t.Errorf("api-key: OrgFromContext got (%q,%v), want (org_keyed,true)", res.orgID, res.orgIDOK)
	}
}

func TestPrincipalMiddleware_APIKey_Invalid_Returns401(t *testing.T) {
	svc, store := newTestService(t)
	keys := apikey.NewMemStore() // empty store — key not found

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer spck_totally-invalid-key")

	res := runPrincipalMW(t, req, keys, nil, svc.verifier, store)
	if res.code != http.StatusUnauthorized {
		t.Fatalf("invalid api-key: want 401, got %d", res.code)
	}
}

func TestPrincipalMiddleware_APIKey_FrozenOrg_Returns403(t *testing.T) {
	svc, store := newTestService(t)

	orgStore := org.NewMemStore()
	ctx := context.Background()
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org_frozen", Slug: "frozen", Status: org.StatusFrozen,
	})

	keys := apikey.NewMemStore()
	_, rawKey, err := keys.Create("org_frozen", "ci-key")
	if err != nil {
		t.Fatalf("Create apikey: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)

	res := runPrincipalMW(t, req, keys, orgStore, svc.verifier, store)
	if res.code != http.StatusForbidden {
		t.Fatalf("api-key frozen org: want 403, got %d", res.code)
	}
}

func TestPrincipalMiddleware_APIKey_OrgGone_SetsSubjectNoActiveOrg(t *testing.T) {
	// REGISTRY-DESIGN §3: if the pinned org row is gone, do NOT synthesise an
	// active+admin phantom org — fail-closed: activeOrg stays nil.
	svc, store := newTestService(t)

	orgStore := org.NewMemStore() // "org_deleted" was never added

	keys := apikey.NewMemStore()
	_, rawKey, err := keys.Create("org_deleted", "ci-key")
	if err != nil {
		t.Fatalf("Create apikey: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)

	res := runPrincipalMW(t, req, keys, orgStore, svc.verifier, store)
	if res.code != http.StatusOK {
		t.Fatalf("api-key deleted org: want 200 (subject set), got %d", res.code)
	}
	// Subject is set (the key itself is valid).
	if res.subject.UserID == "" {
		t.Error("subject must be set even when org row is gone")
	}
	// But activeOrg must be nil — no phantom org.
	if res.activeOK {
		t.Error("activeOrg must be nil when pinned org row is missing (fail-closed)")
	}
}
