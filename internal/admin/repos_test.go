package admin

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
)

// ---- fake repo/tag store -----------------------------------------------------

// fakeRepoStore is a thread-safe in-memory repo.RepoStore + repo.TagStore.
type fakeRepoStore struct {
	mu     sync.Mutex
	repos  map[string]*repo.Repo // key: orgID + "|" + name
	tags   map[string][]*repo.Tag
	nextID int
}

func newFakeRepoStore() *fakeRepoStore {
	return &fakeRepoStore{
		repos: make(map[string]*repo.Repo),
		tags:  make(map[string][]*repo.Tag),
	}
}

var (
	_ repo.RepoStore = (*fakeRepoStore)(nil)
	_ repo.TagStore  = (*fakeRepoStore)(nil)
)

func repoKey(orgID, name string) string { return orgID + "|" + name }

func (f *fakeRepoStore) CreateRepo(_ context.Context, orgID, name, visibility, owner string) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r := &repo.Repo{
		ID:          "repo_" + string(rune('a'+f.nextID)),
		OrgID:       orgID,
		Name:        name,
		Visibility:  repo.NormalizeVisibility(visibility),
		OwnerUserID: owner,
		CreatedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
	f.repos[repoKey(orgID, name)] = r
	return r, nil
}

func (f *fakeRepoStore) GetRepo(_ context.Context, orgID, name string) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[repoKey(orgID, name)]
	if !ok {
		return nil, repo.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRepoStore) ListRepos(_ context.Context, orgID string) ([]*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*repo.Repo
	for _, r := range f.repos {
		if r.OrgID == orgID {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRepoStore) SetVisibility(_ context.Context, orgID, name, visibility string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[repoKey(orgID, name)]
	if !ok {
		return repo.ErrNotFound
	}
	r.Visibility = visibility
	return nil
}

func (f *fakeRepoStore) DeleteRepo(_ context.Context, orgID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := repoKey(orgID, name)
	r, ok := f.repos[k]
	if !ok {
		return repo.ErrNotFound
	}
	delete(f.repos, k)
	delete(f.tags, r.ID)
	return nil
}

func (f *fakeRepoStore) PutTag(_ context.Context, repoID, tag, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tags[repoID] {
		if t.Tag == tag {
			t.Digest = digest
			return nil
		}
	}
	f.tags[repoID] = append(f.tags[repoID], &repo.Tag{
		RepoID: repoID, Tag: tag, Digest: digest,
		UpdatedAt: time.Unix(1_700_000_100, 0).UTC(),
	})
	return nil
}

func (f *fakeRepoStore) GetTag(_ context.Context, repoID, tag string) (*repo.Tag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tags[repoID] {
		if t.Tag == tag {
			cp := *t
			return &cp, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *fakeRepoStore) ListTags(_ context.Context, repoID string) ([]*repo.Tag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*repo.Tag
	for _, t := range f.tags[repoID] {
		cp := *t
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out, nil
}

func (f *fakeRepoStore) DeleteTag(_ context.Context, repoID, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.tags[repoID][:0]
	for _, t := range f.tags[repoID] {
		if t.Tag != tag {
			kept = append(kept, t)
		}
	}
	f.tags[repoID] = kept
	return nil
}

// ---- harness -----------------------------------------------------------------

// repoHarness bundles the server with the stores the repo tests drive.
type repoHarness struct {
	*harness
	orgs  *fakeOrgStore
	repos *fakeRepoStore
}

// newRepoHarness builds a server with org + repo stores and one org ("acme")
// containing a private repo "acme/app" owned by owner@example.com.
func newRepoHarness(t *testing.T) *repoHarness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	orgStore := newFakeOrgStore()
	repoStore := newFakeRepoStore()

	srv := New(Deps{
		Stats:     newFakeStatsCollector(),
		Meta:      &fakeMetaStore{},
		Users:     store,
		Auth:      auth.NewService(store, hasher, verifier, false, nil),
		Tokens:    verifier,
		Config:    testConfig(),
		Blobs:     &fakeBlobReporter{usedBytes: 999},
		OrgStore:  orgStore,
		RepoStore: repoStore,
		TagStore:  repoStore,
	})
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &repoHarness{
		harness: &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher},
		orgs:    orgStore,
		repos:   repoStore,
	}
}

// seedOrg creates an org with the given slug.
func (h *repoHarness) seedOrg(t *testing.T, id, slug string) {
	t.Helper()
	require.NoError(t, h.orgs.CreateOrg(context.Background(), &org.Org{
		ID: id, Name: slug, Slug: slug, Status: "active",
	}))
}

// seedMember adds email to an org with role, creating the user and returning a
// session token for them.
func (h *repoHarness) seedMember(t *testing.T, orgID, email, role string) string {
	t.Helper()
	u, err := h.store.CreateUser(context.Background(), auth.User{
		Email: email, PasswordHash: "hash:pw", SystemRole: "user",
	})
	require.NoError(t, err)
	require.NoError(t, h.orgs.AddOrgMember(context.Background(), &org.Member{
		OrgID: orgID, Email: email, Role: role,
	}))
	tok, err := h.verifier.Sign(*u)
	require.NoError(t, err)
	return tok
}

// seedRepo creates a repo owned by ownerSubject with the given visibility.
func (h *repoHarness) seedRepo(t *testing.T, orgID, name, visibility, ownerSubject string) *repo.Repo {
	t.Helper()
	r, err := h.repos.CreateRepo(context.Background(), orgID, name, visibility, ownerSubject)
	require.NoError(t, err)
	return r
}

// setup builds the standard fixture and returns the org id.
func (h *repoHarness) setup(t *testing.T) string {
	t.Helper()
	h.seedOrg(t, "org_1", "acme")
	return "org_1"
}

// ---- list --------------------------------------------------------------------

func TestListRepos_OrgMemberSeesOrgRepos(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	h.seedRepo(t, orgID, "acme/web", repo.VisibilityPublic, "user:99")

	rr := h.do("GET", "/api/v1/orgs/acme/repos", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp ReposResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Repos, 2)
	// An org member must see the org's PRIVATE repos: that is the whole point
	// of the org repo list, and acl alone (owner-only for private) would hide it.
	assert.Equal(t, "acme/app", resp.Repos[0].Name)
	assert.Equal(t, "private", resp.Repos[0].Visibility)
	assert.Equal(t, "acme/web", resp.Repos[1].Name)
	assert.Equal(t, "public", resp.Repos[1].Visibility)
}

// The {org} segment must accept the org id as well as its slug.
func TestListRepos_AcceptsOrgIDOrSlug(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	for _, ref := range []string{"acme", "org_1"} {
		rr := h.do("GET", "/api/v1/orgs/"+ref+"/repos", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "org ref %q", ref)
	}
}

func TestListRepos_NonMemberIsForbidden(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	_, tok := h.mustCreateUser(t, "outsider@example.com")

	rr := h.do("GET", "/api/v1/orgs/acme/repos", tok, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestListRepos_UnknownOrgIs404(t *testing.T) {
	h := newRepoHarness(t)
	h.setup(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/orgs/ghost/repos", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListRepos_IncludesTagAggregates(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	r := h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v1", "sha256:aaa"))
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v2", "sha256:bbb"))

	var resp ReposResponse
	decodeJSON(t, h.do("GET", "/api/v1/orgs/acme/repos", tok, nil), &resp)
	require.Len(t, resp.Repos, 1)
	assert.Equal(t, 2, resp.Repos[0].TagCount)
	assert.False(t, resp.Repos[0].LastPushedAt.IsZero())
}

// ---- get ---------------------------------------------------------------------

func TestGetRepo_MemberReadsPrivateRepo(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("GET", "/api/v1/orgs/acme/repos/app", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var dto RepoDTO
	decodeJSON(t, rr, &dto)
	assert.Equal(t, "acme/app", dto.Name)
	assert.Equal(t, "private", dto.Visibility)
}

// The {repo} segment is the bare repo name, and the {org} segment may be the
// slug or the id — so all four combinations must address the same repo.
func TestGetRepo_ResolvesByOrgSlugOrID(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	for _, ref := range []string{"acme", "org_1"} {
		rr := h.do("GET", "/api/v1/orgs/"+ref+"/repos/app", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "org ref %q", ref)
	}
}

// A repo pushed under the org-id namespace ("org_1/app") must still be findable
// when the UI navigates by slug — registryauthz accepts either namespace at
// push time, so both forms legitimately exist in the same org.
func TestGetRepo_FindsRepoPushedUnderOrgIDNamespace(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	h.seedRepo(t, orgID, "org_1/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("GET", "/api/v1/orgs/acme/repos/app", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var dto RepoDTO
	decodeJSON(t, rr, &dto)
	assert.Equal(t, "org_1/app", dto.Name)
}

// A non-member must not be able to confirm a private repo exists.
func TestGetRepo_PrivateRepoIs404ToOutsider(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	_, tok := h.mustCreateUser(t, "outsider@example.com")

	rr := h.do("GET", "/api/v1/orgs/acme/repos/app", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code,
		"403 would confirm the repo exists, leaking the org's repo names")
}

// A public repo is readable by anyone, per acl — including a non-member.
func TestGetRepo_PublicRepoReadableByOutsider(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/web", repo.VisibilityPublic, "user:99")
	_, tok := h.mustCreateUser(t, "outsider@example.com")

	rr := h.do("GET", "/api/v1/orgs/acme/repos/web", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestGetRepo_MissingRepoIs404(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)

	rr := h.do("GET", "/api/v1/orgs/acme/repos/ghost", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// ---- patch visibility --------------------------------------------------------

// This is the gap the docker real-client test had to work around with a direct
// SQLite write.
func TestPatchRepo_OrgAdminFlipsVisibility(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	vis := repo.VisibilityPublic
	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok,
		jsonBody(PatchRepoRequest{Visibility: &vis}))
	require.Equal(t, http.StatusOK, rr.Code)

	var dto RepoDTO
	decodeJSON(t, rr, &dto)
	assert.Equal(t, "public", dto.Visibility)

	// It must actually persist, not just echo back.
	stored, err := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	require.NoError(t, err)
	assert.Equal(t, "public", stored.Visibility)
}

// The repo's acl owner may change it even without an admin org role.
func TestPatchRepo_OwnerFlipsVisibility(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "owner@acme.example", org.RoleViewer)

	u, err := h.store.GetUserByEmail(context.Background(), "owner@acme.example")
	require.NoError(t, err)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, org.UserSubjectID(u.ID))

	vis := repo.VisibilityPublic
	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok,
		jsonBody(PatchRepoRequest{Visibility: &vis}))
	assert.Equal(t, http.StatusOK, rr.Code)
}

// A viewer who does not own the repo must not be able to expose it publicly.
func TestPatchRepo_ViewerCannotFlipVisibility(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@acme.example", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	vis := repo.VisibilityPublic
	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok,
		jsonBody(PatchRepoRequest{Visibility: &vis}))
	assert.Equal(t, http.StatusForbidden, rr.Code)

	stored, _ := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.Equal(t, "private", stored.Visibility, "a denied patch must not persist")
}

// An unrecognised visibility must be rejected, not normalized: silently mapping
// a typo onto "private" would be a security decision made by a typo.
func TestPatchRepo_RejectsUnknownVisibility(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPublic, "user:99")

	vis := "publicc"
	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok,
		jsonBody(PatchRepoRequest{Visibility: &vis}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	stored, _ := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.Equal(t, "public", stored.Visibility, "a rejected value must change nothing")
}

func TestPatchRepo_NilVisibilityIsNoOp(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok, jsonBody(PatchRepoRequest{}))
	require.Equal(t, http.StatusOK, rr.Code)

	stored, _ := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.Equal(t, "private", stored.Visibility)
}

func TestPatchRepo_RejectsBadBody(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok, strings.NewReader("{nope"))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ---- delete ------------------------------------------------------------------

func TestDeleteRepo_OrgAdminDeletes(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("DELETE", "/api/v1/orgs/acme/repos/app", tok, nil)
	require.Equal(t, http.StatusNoContent, rr.Code)

	_, err := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestDeleteRepo_ViewerIsForbidden(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@acme.example", org.RoleViewer)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")

	rr := h.do("DELETE", "/api/v1/orgs/acme/repos/app", tok, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	_, err := h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.NoError(t, err, "a denied delete must not remove the repo")
}

// ---- tags --------------------------------------------------------------------

func TestListRepoTags_ReturnsTagsWithHonestArch(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@example.com", org.RoleViewer)
	r := h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v1", "sha256:aaa"))
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "latest", "sha256:bbb"))

	rr := h.do("GET", "/api/v1/orgs/acme/repos/app/tags", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp TagsResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Tags, 2)
	assert.Equal(t, "latest", resp.Tags[0].Tag)
	assert.Equal(t, "sha256:bbb", resp.Tags[0].Digest)
	assert.False(t, resp.Tags[0].PushedAt.IsZero())
	// Arch is not parsed from the image config anywhere, so it must stay empty
	// rather than be guessed. The UI renders "—".
	assert.Empty(t, resp.Tags[0].Arch)
}

func TestListRepoTags_PrivateRepoIs404ToOutsider(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	_, tok := h.mustCreateUser(t, "outsider@example.com")

	rr := h.do("GET", "/api/v1/orgs/acme/repos/app/tags", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestDeleteRepoTag_RemovesPointerOnly(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "admin@acme.example", org.RoleAdmin)
	r := h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v1", "sha256:aaa"))
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v2", "sha256:bbb"))

	rr := h.do("DELETE", "/api/v1/orgs/acme/repos/app/tags/v1", tok, nil)
	require.Equal(t, http.StatusNoContent, rr.Code)

	tags, err := h.repos.ListTags(context.Background(), r.ID)
	require.NoError(t, err)
	require.Len(t, tags, 1)
	assert.Equal(t, "v2", tags[0].Tag)

	// The repo itself must survive deleting one of its tags.
	_, err = h.repos.GetRepo(context.Background(), orgID, "acme/app")
	assert.NoError(t, err)
}

func TestDeleteRepoTag_ViewerIsForbidden(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	tok := h.seedMember(t, orgID, "viewer@acme.example", org.RoleViewer)
	r := h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	require.NoError(t, h.repos.PutTag(context.Background(), r.ID, "v1", "sha256:aaa"))

	rr := h.do("DELETE", "/api/v1/orgs/acme/repos/app/tags/v1", tok, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	tags, _ := h.repos.ListTags(context.Background(), r.ID)
	assert.Len(t, tags, 1, "a denied delete must not remove the tag")
}

// ---- auth / wiring -----------------------------------------------------------

func TestRepoEndpoints_RequireAuth(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPublic, "user:99")

	assert.Equal(t, http.StatusUnauthorized, h.do("GET", "/api/v1/orgs/acme/repos", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, h.do("GET", "/api/v1/orgs/acme/repos/app", "", nil).Code)
}

// A system admin bypasses org membership (platform ops).
func TestRepoEndpoints_SystemAdminBypass(t *testing.T) {
	h := newRepoHarness(t)
	orgID := h.setup(t)
	h.seedRepo(t, orgID, "acme/app", repo.VisibilityPrivate, "user:99")
	_, tok := h.mustCreateAdmin(t)

	assert.Equal(t, http.StatusOK, h.do("GET", "/api/v1/orgs/acme/repos", tok, nil).Code)
	assert.Equal(t, http.StatusOK, h.do("GET", "/api/v1/orgs/acme/repos/app", tok, nil).Code)

	vis := repo.VisibilityPublic
	assert.Equal(t, http.StatusOK,
		h.do("PATCH", "/api/v1/orgs/acme/repos/app", tok, jsonBody(PatchRepoRequest{Visibility: &vis})).Code)
}

func TestRepoEndpoints_501WithoutRepoStore(t *testing.T) {
	h := newHarness(t) // no RepoStore dep
	_, tok := h.mustCreateAdmin(t)

	assert.Equal(t, http.StatusNotImplemented, h.do("GET", "/api/v1/orgs/acme/repos", tok, nil).Code)
}
