package admin

// Ported (translated + re-pointed at Specula's harness) from ai-sandbox
// internal/controlplane/api/orgs_selfcreate_test.go — the quota paths the R1 org
// port had to skip for want of a settings layer:
//
//   - 201: the default limit of 1 lets a fresh user create their first org.
//   - 409: a second create at the limit is refused, and the message names the limit.
//   - limit=0 → unlimited.
//   - the limit is HOT: raising it via the resolver takes effect with no restart.
//   - system admins are exempt.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/settings"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// quotaStack is an admin API over a real SQLite org store with a settings
// resolver injected, so CountOrgsByCreator is exercised against real SQL rather
// than a fake that could agree with a wrong implementation.
type quotaStack struct {
	*orgTestStack
	fr *fakeResolver
}

func newQuotaStack(t *testing.T, limit string) *quotaStack {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "org_quota_test.db")
	sq, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "open temp sqlite")
	t.Cleanup(func() { _ = sq.Close() })
	orgStore := org.NewSQLStore(sq.DB())

	st := &orgTestStack{
		orgs:     orgStore,
		users:    newFakeUserStore(),
		keys:     newFakeAPIKeyStore(),
		repos:    newFakeRepoStore(),
		verifier: auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!")),
	}

	fr := newFakeResolver(true)
	if limit != "" {
		fr.overrides[settings.KeyOrgMaxPerUser] = limit
	} else {
		// No override and no bootstrap value → the handler must fall back to
		// settings.DefaultOrgMaxPerUser (1).
		delete(fr.env, settings.KeyOrgMaxPerUser)
	}

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
		Settings:  fr,
	})
	srv.hasher = hasher

	st.mux = http.NewServeMux()
	srv.RegisterRoutes(st.mux)
	return &quotaStack{orgTestStack: st, fr: fr}
}

// TestSelfCreateOrg_DefaultLimitAllowsFirst: the default limit (1) must not
// block a user's FIRST org — this endpoint is the escape hatch for a user who
// belongs to none.
func TestSelfCreateOrg_DefaultLimitAllowsFirst(t *testing.T) {
	st := newQuotaStack(t, "") // no override, no bootstrap → DefaultOrgMaxPerUser
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs", "creator@x.com", "", `{"name":"My Org"}`)
	require.Equal(t, http.StatusCreated, code, "body=%s", body)
}

// TestSelfCreateOrg_409_LimitReached: at limit=1, the second create is refused
// with 409 and the message names the limit (so the user knows what to ask for).
func TestSelfCreateOrg_409_LimitReached(t *testing.T) {
	st := newQuotaStack(t, "1")

	code1, body1 := st.human(t, http.MethodPost, "/api/v1/orgs", "limited@x.com", "", `{"name":"First Org"}`)
	require.Equal(t, http.StatusCreated, code1, "first create: body=%s", body1)

	code2, body2 := st.human(t, http.MethodPost, "/api/v1/orgs", "limited@x.com", "", `{"name":"Second Org"}`)
	require.Equal(t, http.StatusConflict, code2, "second create at limit: body=%s", body2)
	assert.Contains(t, body2, "1", "the 409 should name the limit so the user can act on it")
	assert.Contains(t, body2, "org.max_per_user", "the 409 should name the setting an admin must raise")
}

// TestSelfCreateOrg_Unlimited_WhenLimitIsZero: limit=0 means unlimited.
func TestSelfCreateOrg_Unlimited_WhenLimitIsZero(t *testing.T) {
	st := newQuotaStack(t, "0")

	code1, body1 := st.human(t, http.MethodPost, "/api/v1/orgs", "unlimited@x.com", "", `{"name":"Org One"}`)
	require.Equal(t, http.StatusCreated, code1, "body=%s", body1)
	code2, body2 := st.human(t, http.MethodPost, "/api/v1/orgs", "unlimited@x.com", "", `{"name":"Org Two"}`)
	require.Equal(t, http.StatusCreated, code2, "limit=0 must be unlimited; body=%s", body2)
}

// TestSelfCreateOrg_LimitIsHot is the point of the whole settings layer: raising
// the limit at runtime must take effect on the very next request, with no
// restart and no redeploy.
func TestSelfCreateOrg_LimitIsHot(t *testing.T) {
	st := newQuotaStack(t, "1")

	code, _ := st.human(t, http.MethodPost, "/api/v1/orgs", "hot@x.com", "", `{"name":"One"}`)
	require.Equal(t, http.StatusCreated, code)
	code, _ = st.human(t, http.MethodPost, "/api/v1/orgs", "hot@x.com", "", `{"name":"Two"}`)
	require.Equal(t, http.StatusConflict, code, "at limit=1 the second must be refused")

	// An admin raises the limit at runtime.
	require.NoError(t, st.fr.Set(context.Background(), settings.KeyOrgMaxPerUser, "3"))

	code, body := st.human(t, http.MethodPost, "/api/v1/orgs", "hot@x.com", "", `{"name":"Two"}`)
	assert.Equal(t, http.StatusCreated, code,
		"raising org.max_per_user must take effect on the next request, no restart; body=%s", body)
}

// TestSelfCreateOrg_SystemAdminExempt: a system admin administers the limit and
// must never be locked out by it.
func TestSelfCreateOrg_SystemAdminExempt(t *testing.T) {
	st := newQuotaStack(t, "1")

	code, _ := st.humanSystem(t, http.MethodPost, "/api/v1/orgs", "root@x.com", "admin", "", `{"name":"One"}`)
	require.Equal(t, http.StatusCreated, code)
	code, body := st.humanSystem(t, http.MethodPost, "/api/v1/orgs", "root@x.com", "admin", "", `{"name":"Two"}`)
	assert.Equal(t, http.StatusCreated, code,
		"a system admin must be exempt from org.max_per_user; body=%s", body)
}

// TestSelfCreateOrg_QuotaCountsCreatedNotJoined: being invited into other
// people's orgs must not consume your own create allowance.
func TestSelfCreateOrg_QuotaCountsCreatedNotJoined(t *testing.T) {
	st := newQuotaStack(t, "1")
	ctx := context.Background()

	// Somebody else's org, which joiner@x.com is a member of.
	require.NoError(t, st.orgs.CreateOrg(ctx, &org.Org{
		ID: "org_other", Name: "Other", Slug: "other", Status: org.StatusActive, CreatedBy: "user:999",
	}))
	require.NoError(t, st.orgs.AddOrgMember(ctx, &org.Member{
		OrgID: "org_other", Email: "joiner@x.com", Role: org.RoleEditor,
	}))

	code, body := st.human(t, http.MethodPost, "/api/v1/orgs", "joiner@x.com", "", `{"name":"My Own"}`)
	assert.Equal(t, http.StatusCreated, code,
		"membership in another user's org must not consume the create quota; body=%s", body)
}

// TestSelfCreateOrg_NoSettingsResolverUsesDefault: with no settings resolver
// wired at all, the quota must still apply at its documented default rather than
// failing open to unlimited.
func TestSelfCreateOrg_NoSettingsResolverUsesDefault(t *testing.T) {
	st := newQuotaStack(t, "1")
	// Drop the resolver, as a deployment without a settings layer would have.
	st.mux = http.NewServeMux()
	dsn := filepath.Join(t.TempDir(), "no_settings.db")
	sq, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sq.Close() })
	orgStore := org.NewSQLStore(sq.DB())
	hasher := &fakeHasher{}
	srv := New(Deps{
		Stats: newFakeStatsCollector(), Meta: &fakeMetaStore{}, Users: st.users,
		Auth:   auth.NewService(st.users, hasher, st.verifier, false, nil),
		Tokens: st.verifier, Config: testConfig(), Blobs: &fakeBlobReporter{usedBytes: 1},
		OrgStore: orgStore, KeyStore: st.keys, RepoStore: st.repos, TagStore: st.repos,
		Settings: nil,
	})
	srv.hasher = hasher
	srv.RegisterRoutes(st.mux)
	st.orgs = orgStore

	code, _ := st.human(t, http.MethodPost, "/api/v1/orgs", "nores@x.com", "", `{"name":"One"}`)
	require.Equal(t, http.StatusCreated, code)
	code, body := st.human(t, http.MethodPost, "/api/v1/orgs", "nores@x.com", "", `{"name":"Two"}`)
	assert.Equal(t, http.StatusConflict, code,
		"with no settings resolver the quota must fall back to DefaultOrgMaxPerUser, not to unlimited; body=%s", body)
	_ = strings.TrimSpace(body)
}
