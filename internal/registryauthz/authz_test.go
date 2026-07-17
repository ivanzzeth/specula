// Package registryauthz_test is the black-box test suite for the registry
// authorization glue. Each test names the REQUIREMENT it asserts — primarily
// from REGISTRY-DESIGN §2.3, §2.4, §3 and ARCHITECTURE §11.
//
// Core invariants tested:
//   - Everything fails closed: unknown namespace, missing repo, store errors,
//     anonymous callers all produce deny/not-found, never phantom allow.
//   - Permission is decided BEFORE existence: a caller without push scope gets
//     ErrForbidden (403), not ErrNotFound (404), even when the repo is absent.
//   - A pull-only caller gets ErrNotFound (not ErrForbidden) for a missing
//     repo in an org they have pull scope for.
//   - Non-org names (e.g. "library/nginx") get pull-through pull-only.
//   - A public repo is readable by anyone (including anonymous) without a token.
package registryauthz_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/registryauthz"
	"github.com/ivanzzeth/specula/internal/registrytoken"
	"github.com/ivanzzeth/specula/internal/repo"
)

// ── fake repo store ───────────────────────────────────────────────────────────

// fakeRepoStore is an in-memory implementation of repo.RepoStore for unit tests.
// The err field injects a uniform store error for all operations, exercising the
// "store error → deny, not allow" (fail-closed) invariant.
type fakeRepoStore struct {
	mu    sync.Mutex
	repos map[string]*repo.Repo // key: orgID+"\x00"+name
	err   error                 // if non-nil, all operations return this error
}

func newFakeRepoStore() *fakeRepoStore {
	return &fakeRepoStore{repos: make(map[string]*repo.Repo)}
}

func repoStoreKey(orgID, name string) string { return orgID + "\x00" + name }

func (s *fakeRepoStore) CreateRepo(_ context.Context, orgID, name, visibility, ownerUserID string) (*repo.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	k := repoStoreKey(orgID, name)
	if _, dup := s.repos[k]; dup {
		return nil, errors.New("repo: duplicate (org_id, name)")
	}
	if visibility == "" {
		visibility = repo.VisibilityPrivate
	}
	r := &repo.Repo{
		ID:          "repo_" + name,
		OrgID:       orgID,
		Name:        name,
		Visibility:  visibility,
		OwnerUserID: ownerUserID,
	}
	s.repos[k] = r
	cp := *r
	return &cp, nil
}

func (s *fakeRepoStore) GetRepo(_ context.Context, orgID, name string) (*repo.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if r, ok := s.repos[repoStoreKey(orgID, name)]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, repo.ErrNotFound
}

func (s *fakeRepoStore) ListRepos(_ context.Context, orgID string) ([]*repo.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var out []*repo.Repo
	for _, r := range s.repos {
		if r.OrgID == orgID {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeRepoStore) SetVisibility(_ context.Context, orgID, name, visibility string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if r, ok := s.repos[repoStoreKey(orgID, name)]; ok {
		r.Visibility = visibility
	}
	return nil
}

func (s *fakeRepoStore) DeleteRepo(_ context.Context, orgID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	k := repoStoreKey(orgID, name)
	if _, ok := s.repos[k]; !ok {
		return repo.ErrNotFound
	}
	delete(s.repos, k)
	return nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

const (
	testOrgSlug  = "myorg"
	testOrgID    = "org_myorg"
	testRepoName = "myorg/myapp"
	testOwner    = "user:1"
	memberEmail  = "member@example.com"
)

// newTokenSvc creates a registrytoken.Service backed by a freshly-generated RSA
// key pair in a temp directory.
func newTokenSvc(t *testing.T) *registrytoken.Service {
	t.Helper()
	key, err := registrytoken.EnsureKeyPair(t.TempDir() + "/reg.pem")
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	return registrytoken.NewService(key, "specula-test", "specula", 0)
}

// ctxWithClaims builds a context carrying AccessClaims for subject+access by
// routing a /v2/ base-probe request through the Challenge middleware. The base
// probe (isRepoReq=false) lets any valid token pass regardless of scope, so we
// only need the claims injected — not the scope to match the path.
func ctxWithClaims(
	t *testing.T,
	svc *registrytoken.Service,
	subject string,
	access []registrytoken.Access,
) context.Context {
	t.Helper()
	tok, err := svc.Mint(subject, access)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var capturedCtx context.Context
	handler := svc.Challenge("http://localhost/token")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedCtx = r.Context()
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if capturedCtx == nil {
		t.Fatal("Challenge middleware did not inject context — token likely rejected")
	}
	return capturedCtx
}

// setupOrgAndRepo creates a standard org + member + private repo fixture.
func setupOrgAndRepo(t *testing.T) (*org.MemStore, *fakeRepoStore) {
	t.Helper()
	orgs := org.NewMemStore()
	repos := newFakeRepoStore()
	ctx := context.Background()

	if err := orgs.CreateOrg(ctx, &org.Org{
		ID:     testOrgID,
		Name:   "My Org",
		Slug:   testOrgSlug,
		Status: org.StatusActive,
	}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if err := orgs.AddOrgMember(ctx, &org.Member{
		OrgID: testOrgID,
		Email: memberEmail,
		Role:  org.RoleEditor,
	}); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	if _, err := repos.CreateRepo(ctx, testOrgID, testRepoName, repo.VisibilityPrivate, testOwner); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return orgs, repos
}

// ── GrantedActions ────────────────────────────────────────────────────────────

func TestGrantedActions_UnknownNamespace_PullOnly(t *testing.T) {
	// REGISTRY-DESIGN §3: a non-org name (e.g. "library/nginx") is treated as
	// a pull-through upstream name. pull is granted; push/delete are not.
	orgs := org.NewMemStore()
	repos := newFakeRepoStore()
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: testOwner} // authenticated but no org binding
	got := a.GrantedActions(context.Background(), p, "library/nginx", []string{"pull", "push", "delete"})

	if len(got) != 1 || got[0] != "pull" {
		t.Errorf("unknown namespace: got %v, want [pull]", got)
	}
}

func TestGrantedActions_UnknownNamespace_NoActions(t *testing.T) {
	// If pull is not requested there should be nothing granted for an unknown namespace.
	orgs := org.NewMemStore()
	repos := newFakeRepoStore()
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: testOwner}
	got := a.GrantedActions(context.Background(), p, "library/nginx", []string{"push"})
	if len(got) != 0 {
		t.Errorf("unknown namespace push: got %v, want []", got)
	}
}

func TestGrantedActions_OrgNamespace_AnonymousDeniedPrivate(t *testing.T) {
	// REGISTRY-DESIGN §2.4: anonymous Subject{} can only read public repos.
	// An anonymous caller on a private repo must get no actions.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Anonymous: true} // no subject, no org
	got := a.GrantedActions(context.Background(), p, testRepoName, []string{"pull"})
	if len(got) != 0 {
		t.Errorf("anonymous pull on private repo: got %v, want []", got)
	}
}

func TestGrantedActions_OrgNamespace_AnonymousPullPublic(t *testing.T) {
	// Public repos are readable by anonymous callers (acl.Public visibility).
	orgs, repos := setupOrgAndRepo(t)
	ctx := context.Background()
	_ = repos.SetVisibility(ctx, testOrgID, testRepoName, repo.VisibilityPublic)

	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Anonymous: true}
	got := a.GrantedActions(ctx, p, testRepoName, []string{"pull"})
	if len(got) != 1 || got[0] != "pull" {
		t.Errorf("anonymous pull on public repo: got %v, want [pull]", got)
	}
}

func TestGrantedActions_OrgNamespace_OwnerCanPullAndPush(t *testing.T) {
	// The owner of a private repo can pull and push it (isOwner short-circuit).
	// The owner here is testOwner ("user:1"), which matches the OwnerUserID set
	// in setupOrgAndRepo.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: testOwner}
	got := a.GrantedActions(context.Background(), p, testRepoName, []string{"pull", "push"})

	wantSet := map[string]bool{"pull": false, "push": false}
	for _, g := range got {
		wantSet[g] = true
	}
	if !wantSet["pull"] || !wantSet["push"] {
		t.Errorf("owner pull+push: got %v, want [pull push]", got)
	}
}

func TestGrantedActions_OrgNamespace_MemberCanPushNonexistentRepo(t *testing.T) {
	// An org member can create a new repo via a first push. The fallback
	// resource for a nonexistent repo is Org+Write, which allows same-org
	// members to both read and write — enabling the first-push bootstrap path.
	orgs, _ := setupOrgAndRepo(t) // reuse orgs+member; use a fresh repos store
	repos := newFakeRepoStore()   // "myorg/newapp" does NOT exist
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: "user:99", Email: memberEmail}
	got := a.GrantedActions(context.Background(), p, "myorg/newapp", []string{"pull", "push"})

	wantSet := map[string]bool{"pull": false, "push": false}
	for _, g := range got {
		wantSet[g] = true
	}
	if !wantSet["push"] {
		t.Errorf("org member first-push: got %v, want push in result", got)
	}
}

func TestGrantedActions_OrgNamespace_CrossOrgDenied(t *testing.T) {
	// A caller whose OrgID does not match the repo's org must be denied.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	// API-key caller pinned to a different org.
	p := registrytoken.Principal{Subject: "apikey:abc", OrgID: "org_other"}
	got := a.GrantedActions(context.Background(), p, testRepoName, []string{"pull", "push"})
	if len(got) != 0 {
		t.Errorf("cross-org caller: got %v, want []", got)
	}
}

func TestGrantedActions_APIKeyCallerSameOrg_CanPushNonexistentRepo(t *testing.T) {
	// An API-key principal pinned to the repo's org can push a nonexistent repo
	// (first-push bootstrap). The fallback resource (Org+Write) allows same-org
	// callers — including pinned API keys — to create new repos.
	orgs, _ := setupOrgAndRepo(t)
	repos := newFakeRepoStore() // "myorg/newapp" does NOT exist
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: "apikey:key1", OrgID: testOrgID}
	got := a.GrantedActions(context.Background(), p, "myorg/newapp", []string{"pull", "push", "delete"})
	if len(got) == 0 {
		t.Errorf("api-key same-org nonexistent repo: got [], want non-empty")
	}
}

func TestGrantedActions_NonMemberUser_DeniedPrivate(t *testing.T) {
	// An authenticated user who is not a member of the org is denied on private.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	p := registrytoken.Principal{Subject: "user:99", Email: "outsider@example.com"}
	got := a.GrantedActions(context.Background(), p, testRepoName, []string{"pull"})
	if len(got) != 0 {
		t.Errorf("outsider pull on private repo: got %v, want []", got)
	}
}

// ── Authorize ─────────────────────────────────────────────────────────────────

func TestAuthorize_NoClaimsInContext_ErrForbidden(t *testing.T) {
	// REGISTRY-DESIGN §3: fail-closed. No verified token → ErrForbidden (403).
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	_, err := a.Authorize(context.Background(), testRepoName, "push")
	if !errors.Is(err, acl.ErrForbidden) {
		t.Fatalf("no claims: got %v, want ErrForbidden", err)
	}
}

func TestAuthorize_ClaimsMissingAction_ErrForbidden(t *testing.T) {
	// Claims present but do not cover the requested action → ErrForbidden.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	// Token only grants pull; requesting push must be 403.
	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"pull"}},
	})

	_, err := a.Authorize(ctx, testRepoName, "push")
	if !errors.Is(err, acl.ErrForbidden) {
		t.Fatalf("missing action in claims: got %v, want ErrForbidden", err)
	}
}

// TestAuthorize_PermissionBeforeExistence pins the documented conformance
// guarantee (REGISTRY-DESIGN §3):
//
//	"a caller without push scope → 403; a caller with push scope whose repo
//	does not exist → 404 (not 403). Permission is decided BEFORE existence."
//
// Concretely: if a caller has no push scope, requesting push on a nonexistent
// repo yields ErrForbidden (403), NOT ErrNotFound (404). The permission check
// runs first; existence is irrelevant.
func TestAuthorize_PermissionBeforeExistence(t *testing.T) {
	orgs := org.NewMemStore()
	ctx := context.Background()
	_ = orgs.CreateOrg(ctx, &org.Org{ID: testOrgID, Slug: testOrgSlug, Status: org.StatusActive})
	repos := newFakeRepoStore()
	// repo "myorg/newrepo" does NOT exist in repos.

	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	// Token with pull scope only — no push.
	pullOnlyCtx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: "myorg/newrepo", Actions: []string{"pull"}},
	})

	// Must be ErrForbidden (403), not ErrNotFound (404):
	// scope is checked before repo existence.
	_, err := a.Authorize(pullOnlyCtx, "myorg/newrepo", "push")
	if !errors.Is(err, acl.ErrForbidden) {
		t.Fatalf("push with pull-only scope: got %v, want ErrForbidden (403 before 404)", err)
	}
}

func TestAuthorize_PullWithPushScope_RepoExists(t *testing.T) {
	// Happy path: valid push scope on an existing repo returns the repo.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"push", "pull"}},
	})

	r, err := a.Authorize(ctx, testRepoName, "push")
	if err != nil {
		t.Fatalf("push on existing repo: got %v, want nil", err)
	}
	if r == nil || r.Name != testRepoName {
		t.Errorf("push: returned wrong repo %v", r)
	}
}

func TestAuthorize_FirstPush_CreatesRepo(t *testing.T) {
	// REGISTRY-DESIGN §1: a first push lazily creates the org-owned repo row.
	orgs := org.NewMemStore()
	ctx := context.Background()
	_ = orgs.CreateOrg(ctx, &org.Org{ID: testOrgID, Slug: testOrgSlug, Status: org.StatusActive})
	repos := newFakeRepoStore() // empty — repo does not exist yet

	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	pushCtx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"push"}},
	})

	r, err := a.Authorize(pushCtx, testRepoName, "push")
	if err != nil {
		t.Fatalf("first push must create repo: got %v", err)
	}
	if r == nil || r.Name != testRepoName {
		t.Fatalf("first push: returned wrong repo %v", r)
	}

	// The repo must now exist (second push retrieves it).
	r2, err := a.Authorize(pushCtx, testRepoName, "push")
	if err != nil {
		t.Fatalf("second push must retrieve existing repo: got %v", err)
	}
	if r2.Name != r.Name {
		t.Errorf("second push: returned different repo %v", r2)
	}
}

func TestAuthorize_PullOnMissingRepo_ErrNotFound(t *testing.T) {
	// An authorized puller (token has pull scope) hitting a nonexistent repo gets
	// ErrNotFound (404), not ErrForbidden. Permission passes; existence check fails.
	orgs := org.NewMemStore()
	ctx := context.Background()
	_ = orgs.CreateOrg(ctx, &org.Org{ID: testOrgID, Slug: testOrgSlug, Status: org.StatusActive})
	repos := newFakeRepoStore() // empty

	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	pullCtx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"pull"}},
	})

	_, err := a.Authorize(pullCtx, testRepoName, "pull")
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("pull on missing repo: got %v, want ErrNotFound", err)
	}
}

func TestAuthorize_UnknownOrg_ErrNotFound(t *testing.T) {
	// A push token for a namespace with no matching org → ErrNotFound.
	// The org (namespace segment) is not registered, so any request is 404.
	orgs := org.NewMemStore() // empty — no orgs
	repos := newFakeRepoStore()
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: "unknowntenant/repo", Actions: []string{"push"}},
	})

	_, err := a.Authorize(ctx, "unknowntenant/repo", "push")
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("unknown org namespace: got %v, want ErrNotFound", err)
	}
}

func TestAuthorize_StoreError_DenyNotAllow(t *testing.T) {
	// DESIGN-REVIEW §2 (fail-closed): a store error must propagate as denial,
	// never as an allow. A DB outage must not hand push access to any caller.
	orgs := org.NewMemStore()
	ctx := context.Background()
	_ = orgs.CreateOrg(ctx, &org.Org{ID: testOrgID, Slug: testOrgSlug, Status: org.StatusActive})

	repos := newFakeRepoStore()
	repos.err = errors.New("db: connection unavailable") // inject error

	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	pushCtx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"push"}},
	})

	_, err := a.Authorize(pushCtx, testRepoName, "push")
	if err == nil {
		t.Fatal("store error must deny access, not allow (fail-closed)")
	}
	// The error must NOT be ErrForbidden — that would mean a mis-routing
	// through the permission check instead of the store error path.
	if errors.Is(err, acl.ErrForbidden) {
		t.Errorf("store error should propagate, not mask as ErrForbidden")
	}
}

func TestAuthorize_NoNamespaceSegment_ErrNotFound(t *testing.T) {
	// A bare name with no slash ("<repo>" without "<org>/") has no namespace.
	// resolveOrgID returns false → ErrNotFound.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: "noslash", Actions: []string{"push"}},
	})

	_, err := a.Authorize(ctx, "noslash", "push")
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("no-slash name: got %v, want ErrNotFound", err)
	}
}

// ── ResolveHosted ─────────────────────────────────────────────────────────────

func TestResolveHosted_UnknownNamespace_False(t *testing.T) {
	orgs := org.NewMemStore()
	repos := newFakeRepoStore()
	a := registryauthz.New(orgs, repos)

	hosted, err := a.ResolveHosted(context.Background(), "nonexistent/repo")
	if err != nil {
		t.Fatalf("unknown namespace: unexpected error %v", err)
	}
	if hosted {
		t.Error("unknown namespace: must not be hosted")
	}
}

func TestResolveHosted_KnownOrgRepoNotFound_False(t *testing.T) {
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	// "myorg/other" is in a known org but not yet created.
	hosted, err := a.ResolveHosted(context.Background(), "myorg/other")
	if err != nil {
		t.Fatalf("missing repo: unexpected error %v", err)
	}
	if hosted {
		t.Error("missing repo in known org: must not be hosted")
	}
}

func TestResolveHosted_ExistingHostedRepo_True(t *testing.T) {
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	hosted, err := a.ResolveHosted(context.Background(), testRepoName)
	if err != nil {
		t.Fatalf("existing hosted repo: unexpected error %v", err)
	}
	if !hosted {
		t.Error("existing hosted repo: must report hosted=true")
	}
}

func TestResolveHosted_StoreError_ReturnsError(t *testing.T) {
	orgs := org.NewMemStore()
	ctx := context.Background()
	_ = orgs.CreateOrg(ctx, &org.Org{ID: testOrgID, Slug: testOrgSlug, Status: org.StatusActive})

	repos := newFakeRepoStore()
	repos.err = errors.New("db error")

	a := registryauthz.New(orgs, repos)

	_, err := a.ResolveHosted(ctx, testRepoName)
	if err == nil {
		t.Fatal("store error in ResolveHosted must surface the error")
	}
}

func TestResolveHosted_NoSlashName_False(t *testing.T) {
	// A name without a slash has no namespace → not hosted.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	hosted, err := a.ResolveHosted(context.Background(), "noslash")
	if err != nil {
		t.Fatalf("no-slash name: unexpected error %v", err)
	}
	if hosted {
		t.Error("no-slash name: must not be hosted")
	}
}

// ── IsOwnedNamespace ──────────────────────────────────────────────────────────

func TestIsOwnedNamespace_KnownOrg_True(t *testing.T) {
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	owned, err := a.IsOwnedNamespace(context.Background(), testRepoName)
	if err != nil {
		t.Fatalf("known org: unexpected error %v", err)
	}
	if !owned {
		t.Error("known org: must be an owned namespace")
	}
}

func TestIsOwnedNamespace_UnknownOrg_False(t *testing.T) {
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	owned, err := a.IsOwnedNamespace(context.Background(), "stranger/repo")
	if err != nil {
		t.Fatalf("unknown org: unexpected error %v", err)
	}
	if owned {
		t.Error("unknown org: must not be owned namespace")
	}
}

func TestIsOwnedNamespace_NoSlash_False(t *testing.T) {
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	owned, err := a.IsOwnedNamespace(context.Background(), "noslash")
	if err != nil {
		t.Fatalf("no-slash name: unexpected error %v", err)
	}
	if owned {
		t.Error("no-slash name: must not be owned")
	}
}

// ── AuthorizeRead ─────────────────────────────────────────────────────────────

func TestAuthorizeRead_ValidClaimsCoveringPull_Nil(t *testing.T) {
	// A token that explicitly grants pull on the repo → nil (allow).
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"pull"}},
	})

	if err := a.AuthorizeRead(ctx, testRepoName); err != nil {
		t.Fatalf("valid pull claims: got %v, want nil", err)
	}
}

func TestAuthorizeRead_NoClaimsPrivateRepo_ErrUnauthorized(t *testing.T) {
	// No token at all + private repo → ErrUnauthorized (401).
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)

	// Background context has no claims (simulates a request that bypassed the
	// Challenge middleware — e.g. the OCI handler called directly in a test).
	if err := a.AuthorizeRead(context.Background(), testRepoName); !errors.Is(err, oci.ErrUnauthorized) {
		t.Fatalf("no claims, private repo: got %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeRead_PublicRepo_NoTokenRequired(t *testing.T) {
	// A public repo is readable by anyone, no token required.
	orgs, repos := setupOrgAndRepo(t)
	ctx := context.Background()
	_ = repos.SetVisibility(ctx, testOrgID, testRepoName, repo.VisibilityPublic)
	a := registryauthz.New(orgs, repos)

	if err := a.AuthorizeRead(ctx, testRepoName); err != nil {
		t.Fatalf("public repo no token: got %v, want nil", err)
	}
}

func TestAuthorizeRead_ClaimsNoPullScope_PrivateRepo_ErrForbidden(t *testing.T) {
	// Token is present but does not include pull scope for this repo.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	// Token grants push-only scope (no pull).
	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: testRepoName, Actions: []string{"push"}},
	})

	if err := a.AuthorizeRead(ctx, testRepoName); !errors.Is(err, oci.ErrForbidden) {
		t.Fatalf("push-only token + private repo: got %v, want ErrForbidden", err)
	}
}

func TestAuthorizeRead_ClaimsWrongRepo_PrivateRepo_ErrForbidden(t *testing.T) {
	// Token grants pull but for a DIFFERENT repo → ErrForbidden.
	orgs, repos := setupOrgAndRepo(t)
	a := registryauthz.New(orgs, repos)
	svc := newTokenSvc(t)

	ctx := ctxWithClaims(t, svc, testOwner, []registrytoken.Access{
		{Type: "repository", Name: "myorg/other", Actions: []string{"pull"}},
	})

	if err := a.AuthorizeRead(ctx, testRepoName); !errors.Is(err, oci.ErrForbidden) {
		t.Fatalf("wrong-repo token + private repo: got %v, want ErrForbidden", err)
	}
}

// ── PasswordAuth ──────────────────────────────────────────────────────────────

// minUserStore is a minimal auth.UserStore implementation for PasswordAuth tests,
// so we can construct an auth.Service without importing the test-only fakeStore.
type minUserStore struct {
	mu    sync.Mutex
	users map[string]*auth.User
	byID  map[int64]*auth.User
	seq   int64
}

func newMinUserStore() *minUserStore {
	return &minUserStore{
		users: make(map[string]*auth.User),
		byID:  make(map[int64]*auth.User),
	}
}

func (s *minUserStore) CountUsers(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.users)), nil
}

func (s *minUserStore) GetUserByEmail(_ context.Context, email string) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[email]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *minUserStore) CreateUser(_ context.Context, u auth.User) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.users[u.Email]; dup {
		return nil, auth.ErrEmailTaken
	}
	s.seq++
	u.ID = s.seq
	stored := u
	s.users[u.Email] = &stored
	s.byID[u.ID] = &stored
	return &stored, nil
}

func (s *minUserStore) GetUserByID(_ context.Context, id int64) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *minUserStore) BumpTokenGen(_ context.Context, id int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return 0, auth.ErrUserNotFound
	}
	u.TokenGen++
	return u.TokenGen, nil
}

func (s *minUserStore) ListUsers(_ context.Context, _, _ int) ([]auth.User, int64, error) {
	return nil, 0, nil
}

func (s *minUserStore) UpdateUserRole(_ context.Context, _ int64, _ string) error { return nil }

func (s *minUserStore) UpdateUserFields(_ context.Context, _ int64, _, _ *string) error {
	return nil
}

func (s *minUserStore) DeleteUser(_ context.Context, _ int64) error { return nil }

// newAuthSvc creates a minimal auth.Service backed by minUserStore for PasswordAuth tests.
func newAuthSvc(t *testing.T) *auth.Service {
	t.Helper()
	store := newMinUserStore()
	svc := auth.NewService(store, auth.NewBcryptHasher(), auth.NewHS256Verifier([]byte("test-secret")), false)
	return svc
}

func TestPasswordAuth_NilSvc_ReturnsFalse(t *testing.T) {
	pa := &registryauthz.PasswordAuth{Svc: nil}
	_, ok := pa.VerifyPassword(context.Background(), "user@example.com", "anypass")
	if ok {
		t.Error("nil Svc must return ok=false")
	}
}

func TestPasswordAuth_LoginFailure_ReturnsFalse(t *testing.T) {
	svc := newAuthSvc(t)
	pa := &registryauthz.PasswordAuth{Svc: svc}

	_, ok := pa.VerifyPassword(context.Background(), "ghost@example.com", "wrongpass")
	if ok {
		t.Error("unknown user: must return ok=false")
	}
}

func TestPasswordAuth_LoginSuccess_ReturnsSubject(t *testing.T) {
	svc := newAuthSvc(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "alice@example.com", "alicepass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	pa := &registryauthz.PasswordAuth{Svc: svc}
	subject, ok := pa.VerifyPassword(ctx, "alice@example.com", "alicepass1")
	if !ok {
		t.Fatal("valid credentials: must return ok=true")
	}
	if subject == "" {
		t.Error("subject must not be empty on success")
	}
}

func TestPasswordAuth_WrongPassword_ReturnsFalse(t *testing.T) {
	svc := newAuthSvc(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "bob@example.com", "bobpass123", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	pa := &registryauthz.PasswordAuth{Svc: svc}
	_, ok := pa.VerifyPassword(ctx, "bob@example.com", "wrongpassword")
	if ok {
		t.Error("wrong password: must return ok=false")
	}
}

// ── nil store edge cases ──────────────────────────────────────────────────────

func TestGrantedActions_NilOrgs_PullOnly(t *testing.T) {
	// When orgs is nil, all names resolve as non-org → pull-through pull-only.
	a := registryauthz.New(nil, newFakeRepoStore())
	p := registrytoken.Principal{Subject: testOwner}
	got := a.GrantedActions(context.Background(), p, testRepoName, []string{"pull", "push"})
	if len(got) != 1 || got[0] != "pull" {
		t.Errorf("nil orgs: got %v, want [pull]", got)
	}
}

func TestResolveHosted_NilOrgs_False(t *testing.T) {
	a := registryauthz.New(nil, newFakeRepoStore())
	hosted, err := a.ResolveHosted(context.Background(), testRepoName)
	if err != nil || hosted {
		t.Errorf("nil orgs: got hosted=%v err=%v, want false/nil", hosted, err)
	}
}

func TestIsOwnedNamespace_NilOrgs_False(t *testing.T) {
	a := registryauthz.New(nil, newFakeRepoStore())
	owned, err := a.IsOwnedNamespace(context.Background(), testRepoName)
	if err != nil || owned {
		t.Errorf("nil orgs: got owned=%v err=%v, want false/nil", owned, err)
	}
}
