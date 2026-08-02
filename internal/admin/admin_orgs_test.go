package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
)

const testAdminKey = "specula-test-admin-key-not-for-prod"

// adminOrgStack is orgStack plus break-glass AdminKey wired into Deps.
func adminOrgStack(t *testing.T) *orgTestStack {
	t.Helper()
	st := orgStack(t)

	// Rebuild server with AdminKey — orgStack already registered routes without
	// it; remount on a fresh mux so AdminKey path is live.
	hasher := &fakeHasher{}
	srv := New(Deps{
		Stats:     newFakeStatsCollector(),
		Meta:      &fakeMetaStore{},
		Users:     st.users,
		Auth:      auth.NewService(st.users, hasher, st.verifier, false, nil),
		Tokens:    st.verifier,
		Config:    testConfig(),
		Blobs:     &fakeBlobReporter{usedBytes: 1},
		OrgStore:  st.orgs,
		KeyStore:  st.keys,
		RepoStore: st.repos,
		TagStore:  st.repos,
		AdminKey:  testAdminKey,
	})
	srv.hasher = hasher
	st.mux = http.NewServeMux()
	srv.RegisterRoutes(st.mux)
	return st
}

func TestAdminCreateOrg_AdminKey_SeedsOwner(t *testing.T) {
	st := adminOrgStack(t)

	code, body := st.key(t, http.MethodPost, "/api/v1/admin/orgs", testAdminKey,
		`{"name":"Acme","slug":"acme","admin_email":"owner@example.com"}`)
	require.Equalf(t, http.StatusCreated, code, "body=%s", body)

	var created OrgDTO
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "acme", created.Slug)
	require.NotEmpty(t, created.ID)

	m, err := st.orgs.GetOrgMember(context.Background(), created.ID, "owner@example.com")
	require.NoError(t, err)
	require.Equal(t, org.RoleOwner, org.NormalizeRole(m.Role))
}

func TestAdminCreateOrg_ConflictOnSlug(t *testing.T) {
	st := adminOrgStack(t)
	code, _ := st.key(t, http.MethodPost, "/api/v1/admin/orgs", testAdminKey,
		`{"name":"Active Dup","slug":"active"}`)
	require.Equal(t, http.StatusConflict, code)
}

func TestAdminCreateOrg_RejectsPlainAPIKey(t *testing.T) {
	st := adminOrgStack(t)
	_, rawKey, err := st.keys.CreateOwned(st.orgActive, "apikey:k1", "k")
	require.NoError(t, err)

	code, _ := st.key(t, http.MethodPost, "/api/v1/admin/orgs", rawKey,
		`{"name":"Nope","slug":"nope"}`)
	require.Equal(t, http.StatusUnauthorized, code,
		"an ordinary org API key must not pass admin-key or session admin")
}

func TestAdminCreateOrg_SessionSystemAdmin(t *testing.T) {
	st := adminOrgStack(t)
	code, body := st.humanSystem(t, http.MethodPost, "/api/v1/admin/orgs",
		"sysadmin@example.com", "admin", "",
		`{"name":"From Session","slug":"from-session","admin_email":"boss@example.com"}`)
	require.Equalf(t, http.StatusCreated, code, "body=%s", body)
}

func TestAdminListOrgs_FilterSlug(t *testing.T) {
	st := adminOrgStack(t)
	code, body := st.key(t, http.MethodGet, "/api/v1/admin/orgs?slug=active", testAdminKey, "")
	require.Equal(t, http.StatusOK, code)
	var resp OrgsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Orgs, 1)
	require.Equal(t, "active", resp.Orgs[0].Slug)

	code, body = st.key(t, http.MethodGet, "/api/v1/admin/orgs?slug=missing", testAdminKey, "")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Empty(t, resp.Orgs)
}

func TestAdminCreateOrgKey_MintsPinnedKey(t *testing.T) {
	st := adminOrgStack(t)

	code, body := st.key(t, http.MethodPost, "/api/v1/admin/orgs", testAdminKey,
		`{"name":"Keyed","slug":"keyed"}`)
	require.Equal(t, http.StatusCreated, code)
	var created OrgDTO
	require.NoError(t, json.Unmarshal([]byte(body), &created))

	code, body = st.key(t, http.MethodPost, "/api/v1/admin/orgs/"+created.ID+"/keys",
		testAdminKey, `{"label":"chorei-push"}`)
	require.Equalf(t, http.StatusCreated, code, "body=%s", body)
	var key KeyDTO
	require.NoError(t, json.Unmarshal([]byte(body), &key))
	require.Equal(t, created.ID, key.OrgID)
	require.NotEmpty(t, key.RawKey)
	require.True(t, len(key.RawKey) > 5 && key.RawKey[:5] == "spck_")
}
