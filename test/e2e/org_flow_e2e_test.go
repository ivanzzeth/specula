package e2e

// org_flow_e2e_test.go drives the multi-tenant org lifecycle over REAL HTTP
// against a REAL server on a REAL free port with a REAL SQLite database.
//
// This is the flow a human actually performs, and it is the flow that was broken:
//
//	register user1  → system admin + owner of the Default org
//	register user2  → NO org (registration is open; membership is invitation-only)
//	/me (user2)     → tells the truth: no active org, orgs: []
//	user1 invites   → response carries a token AND a real expiry
//	user2 accepts   → membership row written, /me now reflects it
//	no-org user3    → creates their own org, becomes its owner
//
// Technique (per ai-sandbox's e2e suite): the listener binds 127.0.0.1:0 and the
// assigned port is read back, and the database lives in t.TempDir(). Nothing is
// hardcoded, so this runs in parallel and repeats cleanly.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/admin"
	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/grant"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ---- harness ----------------------------------------------------------------

type orgFlowServer struct {
	base string
	orgs org.Store
}

// newOrgFlowServer starts the control-plane API on an OS-assigned free port with
// a fresh SQLite DB in the test's temp dir.
func newOrgFlowServer(t *testing.T) *orgFlowServer {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "specula-orgflow.db")
	meta, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "open temp sqlite")
	t.Cleanup(func() { _ = meta.Close() })

	users, ok := any(meta).(auth.UserStore)
	require.True(t, ok, "sqlite store must implement auth.UserStore")

	db := meta.DB()
	orgStore := org.NewSQLStore(db)

	tokens := auth.NewHS256Verifier([]byte("e2e-secret-32-bytes-minimum!!!!!"))
	// orgStore wired in => first-user bootstrap seeds the Default org + owner.
	authSvc := auth.NewService(users, auth.NewBcryptHasher(), tokens, false, orgStore)

	srv := admin.New(admin.Deps{
		Meta:       meta,
		Users:      users,
		Auth:       authSvc,
		Tokens:     tokens,
		Config:     &config.Config{Auth: config.AuthConfig{JWTSecret: "e2e-secret-32-bytes-minimum!!!!!"}},
		Secure:     false,
		OrgStore:   orgStore,
		KeyStore:   apikey.NewSQLStore(db),
		GrantStore: grant.NewSQLStore(db),
		RepoStore:  repo.NewSQLStore(db),
		TagStore:   repo.NewSQLStore(db),
	})

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Bind :0 and read back the assigned port — never hardcode one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "bind free port")
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})

	return &orgFlowServer{
		base: fmt.Sprintf("http://%s", ln.Addr().String()),
		orgs: orgStore,
	}
}

// do performs a real HTTP request, returning status and body.
func (s *orgFlowServer) do(t *testing.T, method, path, sessionCookie, orgID string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sessionCookie != "" {
		req.Header.Set("Cookie", sessionCookie)
	}
	if orgID != "" {
		req.Header.Set("X-Org-Id", orgID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// register creates an account over HTTP and returns its session cookie.
func (s *orgFlowServer) register(t *testing.T, email, password string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.base+"/api/v1/auth/register",
		bytes.NewReader(mustJSONBytes(t, map[string]string{
			"email": email, "password": password, "name": email,
		})))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "register %s: %s", email, body)

	for _, c := range resp.Cookies() {
		if c.Name == auth.TokenCookieName {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatalf("register %s returned no session cookie", email)
	return ""
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func decodeInto(t *testing.T, body string, dst any) {
	t.Helper()
	require.NoErrorf(t, json.Unmarshal([]byte(body), dst), "decode: %s", body)
}

// ---- the flow ---------------------------------------------------------------

func TestOrgFlow_RegisterInviteAcceptEndToEnd(t *testing.T) {
	s := newOrgFlowServer(t)

	// ── 1. First user: system admin + owner of the Default org ──────────────
	user1 := s.register(t, "user1@example.com", "correct-horse-battery")

	code, body := s.do(t, http.MethodGet, "/api/v1/me", user1, org.DefaultOrgID, nil)
	require.Equalf(t, http.StatusOK, code, "user1 /me: %s", body)

	var me1 admin.MeResponse
	decodeInto(t, body, &me1)
	require.True(t, me1.IsAdmin, "first user must be system admin")
	require.Equal(t, org.DefaultOrgID, me1.ActiveOrgID)
	require.Equal(t, org.RoleOwner, me1.ActiveOrgRole, "first user must OWN the default org")
	require.Len(t, me1.Orgs, 1)

	// ── 2. Second user: registration is open, but joins NOTHING ─────────────
	user2 := s.register(t, "user2@example.com", "correct-horse-battery")

	code, body = s.do(t, http.MethodGet, "/api/v1/me", user2, "", nil)
	require.Equalf(t, http.StatusOK, code, "user2 /me: %s", body)

	var me2 admin.MeResponse
	decodeInto(t, body, &me2)
	require.False(t, me2.IsAdmin, "second user must not be system admin")
	require.Emptyf(t, me2.ActiveOrgID,
		"a user in NO org must have no active org — org_default here was the phantom membership: %s", body)
	require.Emptyf(t, me2.ActiveOrgRole, "no org means no role: %s", body)
	require.NotNil(t, me2.Orgs)
	require.Lenf(t, me2.Orgs, 0, "user2 belongs to no org: %s", body)
	require.Containsf(t, body, `"orgs":[]`, "orgs must be [] not null: %s", body)

	// user2 cannot simply assert their way into the default org.
	code, _ = s.do(t, http.MethodGet, "/api/v1/me", user2, org.DefaultOrgID, nil)
	require.Equal(t, http.StatusForbidden, code, "a non-member naming an org must be refused")

	// ── 3. user1 invites user2 ──────────────────────────────────────────────
	code, body = s.do(t, http.MethodPost, "/api/v1/orgs/"+org.DefaultOrgID+"/invitations",
		user1, org.DefaultOrgID, map[string]string{"email": "user2@example.com", "role": "editor"})
	require.Equalf(t, http.StatusCreated, code, "create invitation: %s", body)

	var inv admin.InvitationDTO
	decodeInto(t, body, &inv)
	require.NotEmptyf(t, inv.Token, "invitation MUST carry a token — without it the invitee cannot accept: %s", body)
	require.Equal(t, org.InviteStatusPending, inv.Status)
	require.Falsef(t, inv.ExpiresAt.IsZero(), "expires_at must be a real time, not the zero time: %s", body)
	require.Truef(t, inv.ExpiresAt.After(time.Now()), "expiry must be in the future: %s", body)

	// The invitation alone must not have created a membership.
	_, err := s.orgs.GetOrgMember(context.Background(), org.DefaultOrgID, "user2@example.com")
	require.Error(t, err, "creating an invitation must NOT create a member")

	// ── 4. user2 accepts ────────────────────────────────────────────────────
	code, body = s.do(t, http.MethodPatch, "/api/v1/invitations/"+inv.Token,
		user2, "", map[string]string{"status": "accepted"})
	require.Equalf(t, http.StatusOK, code, "accept invitation: %s", body)

	m, err := s.orgs.GetOrgMember(context.Background(), org.DefaultOrgID, "user2@example.com")
	require.NoError(t, err, "accepting must write the membership row")
	require.Equal(t, org.RoleEditor, org.NormalizeRole(m.Role), "role must come from the invitation")
	require.NotEmpty(t, m.InvitedBy, "invited_by must be backfilled")

	// ── 5. user2's /me now reflects the membership ──────────────────────────
	code, body = s.do(t, http.MethodGet, "/api/v1/me", user2, org.DefaultOrgID, nil)
	require.Equalf(t, http.StatusOK, code, "user2 /me after accept: %s", body)

	var me2After admin.MeResponse
	decodeInto(t, body, &me2After)
	require.Equal(t, org.DefaultOrgID, me2After.ActiveOrgID)
	require.Equal(t, org.RoleEditor, me2After.ActiveOrgRole)
	require.Len(t, me2After.Orgs, 1, "the org switcher must now list the org")

	// The token is spent: a second accept is refused.
	code, _ = s.do(t, http.MethodPatch, "/api/v1/invitations/"+inv.Token,
		user2, "", map[string]string{"status": "accepted"})
	require.Equal(t, http.StatusConflict, code, "an accepted invitation is no longer pending")
}

// TestOrgFlow_NoOrgUserCreatesOwnOrg proves the escape hatch: a user who belongs
// to nothing is not stuck — they can create their own org and own it.
func TestOrgFlow_NoOrgUserCreatesOwnOrg(t *testing.T) {
	s := newOrgFlowServer(t)
	_ = s.register(t, "first@example.com", "correct-horse-battery") // consumes bootstrap
	user3 := s.register(t, "user3@example.com", "correct-horse-battery")

	// Starts with nothing.
	code, body := s.do(t, http.MethodGet, "/api/v1/me", user3, "", nil)
	require.Equal(t, http.StatusOK, code)
	var before admin.MeResponse
	decodeInto(t, body, &before)
	require.Len(t, before.Orgs, 0)

	// Creates their own org — slug derived from the name.
	code, body = s.do(t, http.MethodPost, "/api/v1/orgs", user3, "",
		map[string]string{"name": "User Three Labs"})
	require.Equalf(t, http.StatusCreated, code, "self-service org creation: %s", body)

	var created admin.OrgDTO
	decodeInto(t, body, &created)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "user-three-labs", created.Slug)
	require.Equal(t, org.RoleOwner, created.Role)

	// Owns it, and it is now usable.
	code, body = s.do(t, http.MethodGet, "/api/v1/me", user3, created.ID, nil)
	require.Equal(t, http.StatusOK, code)
	var after admin.MeResponse
	decodeInto(t, body, &after)
	require.Equal(t, created.ID, after.ActiveOrgID)
	require.Equal(t, org.RoleOwner, after.ActiveOrgRole)
	require.Len(t, after.Orgs, 1)

	// As owner they can invite others into their own org.
	code, body = s.do(t, http.MethodPost, "/api/v1/orgs/"+created.ID+"/invitations",
		user3, created.ID, map[string]string{"email": "friend@example.com"})
	require.Equalf(t, http.StatusCreated, code, "owner invites into own org: %s", body)
	var inv admin.InvitationDTO
	decodeInto(t, body, &inv)
	require.NotEmpty(t, inv.Token)
	require.Equal(t, org.RoleViewer, inv.Role, "an invitation with no role defaults to viewer")
}
