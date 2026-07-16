package admin

// org_ported_test.go is ai-sandbox's organization contract, ported.
//
// The cases and assertions are theirs (internal/controlplane/api/{org_isolation,
// orgs,org_roles_b1,org_invitations}_test.go); only the plumbing is adapted to
// Specula's types, routes and resources. The point of porting rather than
// hand-writing is that their suite already encodes the behaviour we are trying
// to match, so a red test here is our bug, not a disagreement.
//
// Mapping of concepts that differ:
//
//   - Their org-scoped resource is a sandbox; ours is a hosted repo. Isolation
//     and the read/write role gate are proven on repos — the case is preserved,
//     the resource is Specula's.
//   - Their users live in the org store; ours live in auth.UserStore, joined to
//     membership by email.
//   - member.InvitedBy holds an acl subject string ("user:<id>") here, where
//     ai-sandbox stores a bare user id.
//
// The stack runs on a real SQLite store in a per-test temp dir, so the SQL
// implementation — not just a fake — is what answers these assertions.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ---- scaffolding (ports orgStack / human / mustOrg / mustMember) -------------

type orgTestStack struct {
	mux       *http.ServeMux
	orgs      org.Store
	users     *fakeUserStore
	keys      *fakeAPIKeyStore
	repos     *fakeRepoStore
	verifier  auth.TokenVerifier
	orgActive string
	orgFrozen string
}

// orgStack builds an API over a real SQLite org store, seeding an active org
// (with viewer/editor/admin members) and a frozen org — mirroring ai-sandbox's
// orgStack.
func orgStack(t *testing.T) *orgTestStack {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "org_api_test.db")
	sq, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "open temp sqlite")
	t.Cleanup(func() { _ = sq.Close() })
	orgStore := org.NewSQLStore(sq.DB())

	const orgActive, orgFrozen = "org_active", "org_frozen"
	st := &orgTestStack{
		orgs:      orgStore,
		users:     newFakeUserStore(),
		keys:      newFakeAPIKeyStore(),
		repos:     newFakeRepoStore(),
		verifier:  auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!")),
		orgActive: orgActive,
		orgFrozen: orgFrozen,
	}

	mustOrg(t, orgStore, &org.Org{ID: orgActive, Name: "Active", Slug: "active", Status: org.StatusActive})
	mustOrg(t, orgStore, &org.Org{ID: orgFrozen, Name: "Frozen", Slug: "frozen", Status: org.StatusFrozen})
	mustMember(t, orgStore, orgActive, "viewer@x.com", org.RoleViewer)
	mustMember(t, orgStore, orgActive, "editor@x.com", org.RoleEditor)
	mustMember(t, orgStore, orgActive, "adm@x.com", org.RoleAdmin)

	hasher := &fakeHasher{}
	srv := New(Deps{
		Stats:     newFakeStatsCollector(),
		Meta:      &fakeMetaStore{},
		Users:     st.users,
		Auth:      auth.NewService(st.users, hasher, st.verifier, false, nil),
		Tokens:    st.verifier,
		Config:    testConfig(),
		Blobs:     &fakeBlobReporter{usedBytes: 1},
		OrgStore:  orgStore,
		KeyStore:  st.keys,
		RepoStore: st.repos,
		TagStore:  st.repos,
	})
	srv.hasher = hasher

	st.mux = http.NewServeMux()
	srv.RegisterRoutes(st.mux)
	return st
}

// mustUser provisions (or fetches) a user with the given email and system role.
func (s *orgTestStack) mustUser(t *testing.T, email, systemRole string) *auth.User {
	t.Helper()
	if u, err := s.users.GetUserByEmail(context.Background(), email); err == nil && u != nil {
		return u
	}
	u, err := s.users.CreateUser(context.Background(), auth.User{
		Email: email, Name: email, PasswordHash: "hash:x", SystemRole: systemRole,
	})
	require.NoError(t, err, "provision %s", email)
	return u
}

// human issues a session-authenticated request as email, scoped to orgID via
// X-Org-Id — the port of ai-sandbox's orgTestStack.human.
func (s *orgTestStack) human(t *testing.T, method, path, email, orgID, body string) (int, string) {
	t.Helper()
	u := s.mustUser(t, email, "user")
	return s.request(t, method, path, s.tokenFor(t, u), orgID, body)
}

// humanSystem is human() for a caller holding a system role.
func (s *orgTestStack) humanSystem(t *testing.T, method, path, email, systemRole, orgID, body string) (int, string) {
	t.Helper()
	u := s.mustUser(t, email, systemRole)
	return s.request(t, method, path, s.tokenFor(t, u), orgID, body)
}

func (s *orgTestStack) tokenFor(t *testing.T, u *auth.User) string {
	t.Helper()
	tok, err := s.verifier.Sign(*u)
	require.NoError(t, err)
	return tok
}

func (s *orgTestStack) request(t *testing.T, method, path, token, orgID, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: auth.TokenCookieName, Value: token})
	}
	if orgID != "" {
		r.Header.Set("X-Org-Id", orgID)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// key issues an API-key-authenticated request (Bearer), for the api-key path.
func (s *orgTestStack) key(t *testing.T, method, path, rawKey, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	r.Header.Set("Authorization", "Bearer "+rawKey)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func mustOrg(t *testing.T, s org.Store, o *org.Org) {
	t.Helper()
	require.NoError(t, s.CreateOrg(context.Background(), o), "create org %s", o.ID)
}

func mustMember(t *testing.T, s org.Store, orgID, email, role string) {
	t.Helper()
	require.NoError(t, s.AddOrgMember(context.Background(), &org.Member{
		OrgID: orgID, Email: email, Role: role,
	}), "add member %s", email)
}

// seedRepo puts a private repo in an org so isolation has a real resource.
func (s *orgTestStack) seedRepo(t *testing.T, orgID, name, ownerEmail string) *repo.Repo {
	t.Helper()
	u := s.mustUser(t, ownerEmail, "user")
	rp, err := s.repos.CreateRepo(context.Background(), orgID, name,
		repo.VisibilityPrivate, org.UserSubjectID(u.ID))
	require.NoError(t, err, "seed repo %s/%s", orgID, name)
	return rp
}

// ---- B2: invitation lifecycle (ports org_invitations_test.go) ----------------

// createInviteToken has adm@x.com invite email, returning the token. It is the
// port of ai-sandbox's helper of the same name, and it asserts the two things
// that were broken here: the response carries a token, and the invitation is
// pending.
func createInviteToken(t *testing.T, st *orgTestStack, email, role string) string {
	t.Helper()
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/invitations",
		"adm@x.com", st.orgActive, `{"email":"`+email+`","role":"`+role+`"}`)
	require.Equalf(t, http.StatusCreated, code, "create invite: body=%s", body)

	var resp struct {
		Token     string    `json:"token"`
		Status    string    `json:"status"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp), "decode invite: %s", body)
	require.NotEmptyf(t, resp.Token, "invitation response MUST carry a token, else the invitee cannot accept: %s", body)
	require.Equal(t, org.InviteStatusPending, resp.Status)
	require.Falsef(t, resp.ExpiresAt.IsZero(),
		"invitation MUST have a real expiry, not the zero time: %s", body)
	require.Truef(t, resp.ExpiresAt.After(time.Now()), "expiry must be in the future: %s", body)
	return resp.Token
}

func TestOrgB2_InviteAcceptCreatesMemberWithInvitedBy(t *testing.T) {
	st := orgStack(t)
	token := createInviteToken(t, st, "newbie@x.com", "editor")

	// Creating the invitation must NOT create a member.
	_, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "newbie@x.com")
	require.Error(t, err, "member must NOT exist before accept")

	code, body := st.human(t, http.MethodPatch, "/api/v1/invitations/"+token,
		"newbie@x.com", "", `{"status":"accepted"}`)
	require.Equalf(t, http.StatusOK, code, "accept: body=%s", body)

	inviter := st.mustUser(t, "adm@x.com", "user")
	m, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "newbie@x.com")
	require.NoError(t, err, "member must exist after accept")
	require.Equal(t, org.RoleEditor, org.NormalizeRole(m.Role), "role must come from the invitation")
	require.Equal(t, org.UserSubjectID(inviter.ID), m.InvitedBy, "invited_by must be backfilled from the invitation")

	inv, err := st.orgs.GetInvitationByToken(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, org.InviteStatusAccepted, inv.Status)
}

func TestOrgB2_ExpiredTokenRejected(t *testing.T) {
	st := orgStack(t)
	require.NoError(t, st.orgs.CreateInvitation(context.Background(), &org.Invitation{
		ID: "inv_exp", OrgID: st.orgActive, Email: "late@x.com", Role: org.RoleEditor,
		InvitedBy: "user:999", Token: "expired-token", Status: org.InviteStatusPending,
		ExpiresAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour),
	}))

	code, body := st.human(t, http.MethodPatch, "/api/v1/invitations/expired-token",
		"late@x.com", "", `{"status":"accepted"}`)
	require.Equalf(t, http.StatusGone, code, "accept expired must be 410: body=%s", body)

	_, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "late@x.com")
	require.Error(t, err, "expired accept must NOT create a member")

	got, err := st.orgs.GetInvitationByToken(context.Background(), "expired-token")
	require.NoError(t, err)
	require.Equal(t, org.InviteStatusExpired, got.Status, "expiry must converge lazily on touch")
}

func TestOrgB2_DeclineLeavesNoMember(t *testing.T) {
	st := orgStack(t)
	token := createInviteToken(t, st, "nope@x.com", "viewer")

	code, body := st.human(t, http.MethodPatch, "/api/v1/invitations/"+token,
		"nope@x.com", "", `{"status":"declined"}`)
	require.Equalf(t, http.StatusOK, code, "decline: body=%s", body)

	_, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "nope@x.com")
	require.Error(t, err, "decline must NOT create a member")

	got, err := st.orgs.GetInvitationByToken(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, org.InviteStatusDeclined, got.Status)
}

func TestOrgB2_AcceptEmailMismatch(t *testing.T) {
	st := orgStack(t)
	token := createInviteToken(t, st, "target@x.com", "editor")

	code, body := st.human(t, http.MethodPatch, "/api/v1/invitations/"+token,
		"impostor@x.com", "", `{"status":"accepted"}`)
	require.Equalf(t, http.StatusForbidden, code, "mismatched accept must be 403: body=%s", body)

	_, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "impostor@x.com")
	require.Error(t, err, "mismatched accept must NOT create a member")
}

func TestOrgB2_InviteRequiresOrgAdmin(t *testing.T) {
	st := orgStack(t)
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/invitations",
		"viewer@x.com", st.orgActive, `{"email":"x@x.com","role":"viewer"}`)
	require.Equalf(t, http.StatusForbidden, code, "viewer must not create invites: body=%s", body)
}

func TestOrgB2_SelfExit(t *testing.T) {
	st := orgStack(t)

	// An ordinary member may leave.
	code, body := st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/me",
		"editor@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusNoContent, code, "editor self-exit: body=%s", body)
	_, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "editor@x.com")
	require.Error(t, err, "editor should be gone after self-exit")

	// The last owner may not leave.
	mustMember(t, st.orgs, st.orgActive, "solo@x.com", org.RoleOwner)
	code, body = st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/me",
		"solo@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusConflict, code, "last owner self-exit must be 409: body=%s", body)
	_, err = st.orgs.GetOrgMember(context.Background(), st.orgActive, "solo@x.com")
	require.NoError(t, err, "last owner must remain after a blocked self-exit")

	// With a co-owner, leaving is allowed again.
	mustMember(t, st.orgs, st.orgActive, "co@x.com", org.RoleOwner)
	code, body = st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/me",
		"solo@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusNoContent, code, "owner self-exit with co-owner: body=%s", body)
}

// TestOrgB2_ListInvitationsWithholdsToken is Specula's addition to their suite:
// the create response is the token's only disclosure. A list that echoed tokens
// would let any org admin accept in someone else's name.
func TestOrgB2_ListInvitationsWithholdsToken(t *testing.T) {
	st := orgStack(t)
	token := createInviteToken(t, st, "listed@x.com", "viewer")

	code, body := st.human(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/invitations",
		"adm@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusOK, code, "list invitations: body=%s", body)
	require.Contains(t, body, "listed@x.com")
	require.NotContainsf(t, body, token, "invitation list must NOT leak tokens: %s", body)
}

// ---- B1: roles, least privilege, ownership, last-owner guard ------------------

func TestOrgB1_InviteDefaultsToViewer(t *testing.T) {
	st := orgStack(t)
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, `{"email":"fresh@x.com"}`)
	require.Equalf(t, http.StatusCreated, code, "add member without role: body=%s", body)

	m, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "fresh@x.com")
	require.NoError(t, err)
	require.Equal(t, org.RoleViewer, org.NormalizeRole(m.Role), "default role must be viewer (least privilege)")
}

func TestOrgB1_OwnershipIsOwnerOnly(t *testing.T) {
	st := orgStack(t)
	mustMember(t, st.orgs, st.orgActive, "own@x.com", org.RoleOwner)

	// A plain org admin passes the member-management gate but must not grant,
	// demote, or remove ownership.
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, `{"email":"grab@x.com","role":"owner"}`)
	require.Equalf(t, http.StatusForbidden, code, "admin grants owner: body=%s", body)

	code, _ = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, `{"email":"own@x.com","role":"admin"}`)
	require.Equal(t, http.StatusForbidden, code, "admin must not demote an owner")

	code, _ = st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/own@x.com",
		"adm@x.com", st.orgActive, "")
	require.Equal(t, http.StatusForbidden, code, "admin must not remove an owner")

	// An owner may share ownership.
	code, body = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"own@x.com", st.orgActive, `{"email":"co@x.com","role":"owner"}`)
	require.Equalf(t, http.StatusCreated, code, "owner grants owner: body=%s", body)

	m, err := st.orgs.GetOrgMember(context.Background(), st.orgActive, "co@x.com")
	require.NoError(t, err)
	require.Equal(t, org.RoleOwner, org.NormalizeRole(m.Role))
}

func TestOrgB1_LastOwnerGuard(t *testing.T) {
	st := orgStack(t)
	mustMember(t, st.orgs, st.orgActive, "solo@x.com", org.RoleOwner)

	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"solo@x.com", st.orgActive, `{"email":"solo@x.com","role":"admin"}`)
	require.Equalf(t, http.StatusConflict, code, "demote last owner must be 409: body=%s", body)

	code, body = st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/solo@x.com",
		"solo@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusConflict, code, "remove last owner must be 409: body=%s", body)

	// With a second owner the demotion is allowed.
	mustMember(t, st.orgs, st.orgActive, "co@x.com", org.RoleOwner)
	code, body = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"solo@x.com", st.orgActive, `{"email":"co@x.com","role":"admin"}`)
	require.Equalf(t, http.StatusCreated, code, "demote one of two owners: body=%s", body)

	n, err := st.orgs.CountOrgOwners(context.Background(), st.orgActive)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// ---- orgs: listing, member management, update, /me ----------------------------

func TestOrgs_ListForMemberAndSystem(t *testing.T) {
	st := orgStack(t)

	// A member sees only their own org.
	code, body := st.human(t, http.MethodGet, "/api/v1/orgs", "editor@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusOK, code, "member list orgs: body=%s", body)
	got := orgListRows(t, body)
	require.True(t, got[st.orgActive].present, "member must see their org")
	require.False(t, got[st.orgFrozen].present, "member must NOT see an org they do not belong to")
	require.Equal(t, org.RoleEditor, got[st.orgActive].role)
	require.False(t, got[st.orgActive].systemAccess, "a real membership is not system access")

	// A system-role holder sees every org; non-member ones as system_access.
	_, body = st.humanSystem(t, http.MethodGet, "/api/v1/orgs", "sysview@x.com", org.RoleViewer, "", "")
	got = orgListRows(t, body)
	require.True(t, got[st.orgActive].present && got[st.orgFrozen].present,
		"system viewer must see ALL orgs, got %v", got)
	require.True(t, got[st.orgActive].systemAccess && got[st.orgFrozen].systemAccess,
		"non-member orgs must be flagged system_access")
}

func TestOrgs_MemberManagement(t *testing.T) {
	st := orgStack(t)

	code, body := st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, `{"email":"new@x.com","role":"viewer"}`)
	require.Equalf(t, http.StatusCreated, code, "admin add member: body=%s", body)

	_, body = st.human(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, "")
	require.Contains(t, body, "new@x.com", "member list must contain the new member")

	// editor / viewer cannot manage members.
	code, _ = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"editor@x.com", st.orgActive, `{"email":"z@x.com","role":"viewer"}`)
	require.Equal(t, http.StatusForbidden, code, "editor must not add members")
	code, _ = st.human(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/members",
		"viewer@x.com", st.orgActive, "")
	require.Equal(t, http.StatusForbidden, code, "viewer must not list members")

	// An unknown role is rejected, not silently downgraded.
	code, _ = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgActive+"/members",
		"adm@x.com", st.orgActive, `{"email":"bad@x.com","role":"superuser"}`)
	require.Equal(t, http.StatusBadRequest, code, "invalid role must be 400")

	// last-admin guard.
	code, _ = st.human(t, http.MethodDelete, "/api/v1/orgs/"+st.orgActive+"/members/adm@x.com",
		"adm@x.com", st.orgActive, "")
	require.Equal(t, http.StatusConflict, code, "removing the last admin must be 409")

	// Cross-org: admin of orgActive must not manage orgFrozen's members.
	code, _ = st.human(t, http.MethodPost, "/api/v1/orgs/"+st.orgFrozen+"/members",
		"adm@x.com", st.orgActive, `{"email":"x@x.com","role":"editor"}`)
	require.Equal(t, http.StatusForbidden, code, "cross-org member management must be 403")

	// A system editor manages members across orgs from the backoffice.
	code, body = st.humanSystem(t, http.MethodPost, "/api/v1/orgs/"+st.orgFrozen+"/members",
		"sysedit@x.com", org.RoleEditor, "", `{"email":"seed@x.com","role":"admin"}`)
	require.Equalf(t, http.StatusCreated, code, "system editor add member: body=%s", body)
}

func TestOrgs_UpdateOrg(t *testing.T) {
	st := orgStack(t)

	code, body := st.human(t, http.MethodPut, "/api/v1/orgs/"+st.orgActive,
		"adm@x.com", st.orgActive, `{"name":"Renamed"}`)
	require.Equalf(t, http.StatusOK, code, "admin update org: body=%s", body)

	o, err := st.orgs.GetOrg(context.Background(), st.orgActive)
	require.NoError(t, err)
	require.Equal(t, "Renamed", o.Name)

	code, _ = st.human(t, http.MethodPut, "/api/v1/orgs/"+st.orgActive,
		"editor@x.com", st.orgActive, `{"name":"Nope"}`)
	require.Equal(t, http.StatusForbidden, code, "editor must not rename the org")
}

func TestMe_ReturnsOrgAndRole(t *testing.T) {
	st := orgStack(t)
	code, body := st.human(t, http.MethodGet, "/api/v1/me", "editor@x.com", st.orgActive, "")
	require.Equalf(t, http.StatusOK, code, "me: body=%s", body)

	var resp MeResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Equal(t, "editor@x.com", resp.User.Email)
	require.Equal(t, st.orgActive, resp.ActiveOrgID)
	require.Equal(t, org.RoleEditor, resp.ActiveOrgRole)
}

// ---- /me truthfulness (the reported bug) --------------------------------------

// TestMe_NonMemberGetsNoPhantomOrg is the regression for the reported failure:
// /me told a user who belonged to nothing that they were in org_default with a
// null role, the UI believed it, and every org-scoped call then 403'd.
func TestMe_NonMemberGetsNoPhantomOrg(t *testing.T) {
	st := orgStack(t)

	code, body := st.human(t, http.MethodGet, "/api/v1/me", "nobody@x.com", "", "")
	require.Equalf(t, http.StatusOK, code, "me: body=%s", body)

	var resp MeResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Emptyf(t, resp.ActiveOrgID, "a user in no org must have NO active org, not org_default: %s", body)
	require.Empty(t, resp.ActiveOrgRole)
	require.NotNilf(t, resp.Orgs, "orgs must serialise as [], never null: %s", body)
	require.Len(t, resp.Orgs, 0)
	require.Containsf(t, body, `"orgs":[]`, "orgs must be an empty list, not null: %s", body)
}

// TestMe_NonMemberNamingAnOrgIsRefused: a no-org user cannot conjure membership
// by asserting X-Org-Id.
func TestMe_NonMemberNamingAnOrgIsRefused(t *testing.T) {
	st := orgStack(t)
	code, _ := st.human(t, http.MethodGet, "/api/v1/me", "nobody@x.com", st.orgActive, "")
	require.Equal(t, http.StatusForbidden, code, "naming an org you have no claim on must be refused")
}

// ---- self-service org creation (the escape hatch from the dead end) -----------

func TestSelfCreateOrg_NoOrgUserBecomesOwner(t *testing.T) {
	st := orgStack(t)

	code, body := st.human(t, http.MethodPost, "/api/v1/orgs", "loner@x.com", "", `{"name":"My Own Org"}`)
	require.Equalf(t, http.StatusCreated, code, "a user with no org must be able to create one: body=%s", body)

	var created OrgDTO
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "my-own-org", created.Slug, "slug must be derived from the name when omitted")
	require.Equal(t, org.RoleOwner, created.Role)

	m, err := st.orgs.GetOrgMember(context.Background(), created.ID, "loner@x.com")
	require.NoError(t, err, "creator must be a member")
	require.Equal(t, org.RoleOwner, org.NormalizeRole(m.Role), "creator must be owner")

	// The new org is now reachable: /me reports it and it lists.
	code, body = st.human(t, http.MethodGet, "/api/v1/me", "loner@x.com", created.ID, "")
	require.Equal(t, http.StatusOK, code)
	var me MeResponse
	require.NoError(t, json.Unmarshal([]byte(body), &me))
	require.Equal(t, created.ID, me.ActiveOrgID)
	require.Equal(t, org.RoleOwner, me.ActiveOrgRole)
	require.Len(t, me.Orgs, 1)
}

func TestSelfCreateOrg_400_NameRequired(t *testing.T) {
	st := orgStack(t)
	code, _ := st.human(t, http.MethodPost, "/api/v1/orgs", "loner@x.com", "", `{"name":"  "}`)
	require.Equal(t, http.StatusBadRequest, code)
}

func TestSelfCreateOrg_403_APIKey(t *testing.T) {
	st := orgStack(t)
	_, rawKey, err := st.keys.CreateOwned(st.orgActive, "apikey:k1", "k")
	require.NoError(t, err)

	code, _ := st.key(t, http.MethodPost, "/api/v1/orgs", rawKey, `{"name":"Machine Org"}`)
	require.Equal(t, http.StatusForbidden, code, "an API key has no email and must not create orgs")
}

// ---- frozen orgs --------------------------------------------------------------

func TestFrozenOrg_HumanReadsBlocked(t *testing.T) {
	st := orgStack(t)
	mustMember(t, st.orgs, st.orgFrozen, "adm@x.com", org.RoleAdmin)

	code, body := st.human(t, http.MethodGet, "/api/v1/orgs/"+st.orgFrozen+"/repos",
		"adm@x.com", st.orgFrozen, "")
	require.Equalf(t, http.StatusForbidden, code, "even an admin is blocked in a frozen org: body=%s", body)
}

func TestAPIKeyIsOrgAdmin_AndFrozenRejects(t *testing.T) {
	st := orgStack(t)

	// A key in an active org passes the role gate.
	_, activeKey, err := st.keys.CreateOwned(st.orgActive, "apikey:a", "a")
	require.NoError(t, err)
	code, body := st.key(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/repos", activeKey, "")
	require.Equalf(t, http.StatusOK, code, "api-key is org admin in its own org: body=%s", body)

	// A key pinned to a frozen org is refused, reads included.
	_, frozenKey, err := st.keys.CreateOwned(st.orgFrozen, "apikey:f", "f")
	require.NoError(t, err)
	code, body = st.key(t, http.MethodGet, "/api/v1/orgs/"+st.orgFrozen+"/repos", frozenKey, "")
	require.Equalf(t, http.StatusForbidden, code, "frozen org must refuse its api-key: body=%s", body)
}

// ---- org isolation, proven on a real Specula resource (hosted repos) ----------

// TestOrgIsolation_APIKeys ports their cross-org invisibility case onto repos:
// org A's key must not see, read, or destroy org B's repo.
func TestOrgIsolation_APIKeys(t *testing.T) {
	st := orgStack(t)
	orgB := "org_b"
	mustOrg(t, st.orgs, &org.Org{ID: orgB, Name: "B", Slug: "b", Status: org.StatusActive})

	st.seedRepo(t, st.orgActive, "active/app-a", "owner-a@x.com")
	st.seedRepo(t, orgB, "b/app-b", "owner-b@x.com")

	_, keyA, err := st.keys.CreateOwned(st.orgActive, "apikey:a", "a")
	require.NoError(t, err)

	// A lists only its own repo.
	code, body := st.key(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/repos", keyA, "")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "app-a")
	require.NotContainsf(t, body, "app-b", "A's repo list leaked B's repo: %s", body)

	// A cannot reach into B — existence is not confirmed either.
	code, _ = st.key(t, http.MethodGet, "/api/v1/orgs/"+orgB+"/repos/app-b", keyA, "")
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, code,
		"A must not read B's repo")
	code, _ = st.key(t, http.MethodDelete, "/api/v1/orgs/"+orgB+"/repos/app-b", keyA, "")
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, code,
		"A must not delete B's repo")

	// B's repo survived A's attempt.
	rp, err := st.repos.GetRepo(context.Background(), orgB, "b/app-b")
	require.NoError(t, err, "B's repo must still exist after A's cross-org delete")
	require.NotNil(t, rp)
}

// TestRBAC_HumanOrgRoles ports the role gate onto repos: every member reads,
// only admin+ writes.
func TestRBAC_HumanOrgRoles(t *testing.T) {
	st := orgStack(t)
	st.seedRepo(t, st.orgActive, "active/app", "owner@x.com")

	for _, c := range []struct {
		email      string
		writeIs403 bool
	}{
		{"viewer@x.com", true},
		{"editor@x.com", true}, // Specula gates repo writes at admin+, not editor+
		{"adm@x.com", false},
	} {
		code, body := st.human(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/repos",
			c.email, st.orgActive, "")
		require.Equalf(t, http.StatusOK, code, "%s read: body=%s", c.email, body)

		code, _ = st.human(t, http.MethodPatch, "/api/v1/orgs/"+st.orgActive+"/repos/app",
			c.email, st.orgActive, `{"visibility":"public"}`)
		if c.writeIs403 {
			require.Equalf(t, http.StatusForbidden, code, "%s write must be refused", c.email)
		} else {
			require.NotEqualf(t, http.StatusForbidden, code, "%s write must pass the role gate", c.email)
		}
	}
}

// TestSystemRole_ImplicitCrossOrgReadOnly: a system-role holder reads any org
// but writes none — the implicit view is read-only.
func TestSystemRole_ImplicitCrossOrgReadOnly(t *testing.T) {
	st := orgStack(t)
	st.seedRepo(t, st.orgActive, "active/app", "owner@x.com")

	code, body := st.humanSystem(t, http.MethodGet, "/api/v1/orgs/"+st.orgActive+"/repos",
		"sysview@x.com", org.RoleViewer, st.orgActive, "")
	require.Equalf(t, http.StatusOK, code, "system viewer cross-org read: body=%s", body)

	code, _ = st.humanSystem(t, http.MethodPatch, "/api/v1/orgs/"+st.orgActive+"/repos/app",
		"sysview@x.com", org.RoleViewer, st.orgActive, `{"visibility":"public"}`)
	require.Equal(t, http.StatusForbidden, code, "the implicit system view must never write")
}

// ---- guard units (ports TestGuardsRoleNormalizationAndRanks) ------------------

func TestGuardsRoleNormalizationAndRanks(t *testing.T) {
	require.Equal(t, org.RoleAdmin, org.NormalizeLegacyRole("org_admin"), "legacy org_admin must map to admin")
	require.Equal(t, org.RoleEditor, org.NormalizeLegacyRole("member"), "legacy member must map to editor")
	require.Equal(t, "", org.NormalizeLegacyRole("superuser"), "an unknown role must not be invented")

	require.True(t, org.AtLeast(org.RoleEditor, org.RoleViewer))
	require.True(t, org.AtLeast(org.RoleAdmin, org.RoleEditor))
	require.True(t, org.AtLeast(org.RoleOwner, org.RoleAdmin))
	require.False(t, org.AtLeast(org.RoleViewer, org.RoleEditor))

	// The system-role axis: "" and the legacy "user" both mean NO system access.
	// Getting this wrong hands every ordinary account cross-org read.
	require.Equal(t, "", org.NormalizeSystemRole(""))
	require.Equal(t, "", org.NormalizeSystemRole("user"))
	require.Equal(t, org.RoleAdmin, org.NormalizeSystemRole("admin"))
}

// ---- parse helpers -----------------------------------------------------------

type orgListRow struct {
	present      bool
	role         string
	systemAccess bool
}

func orgListRows(t *testing.T, body string) map[string]orgListRow {
	t.Helper()
	var env OrgsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &env), "decode org list: %s", body)
	out := map[string]orgListRow{}
	for _, o := range env.Orgs {
		out[o.ID] = orgListRow{present: true, role: o.Role, systemAccess: o.SystemAccess}
	}
	return out
}
