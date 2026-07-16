package postgres

// TestMultiTenantPostgres is an integration test that proves the postgres
// placeholder rebind ($N instead of ?) works end-to-end for every multi-tenant
// SQL store. It is gated behind SPECULA_TEST_POSTGRES_DSN; when that env var is
// absent the test skips (same convention used in postgres_test.go).
//
// The test exercises three stores in sequence on a shared *sql.DB opened by
// postgres.OpenSQLDB (the pgx stdlib driver) with goose migrations applied:
//
//  1. org.NewSQLStorePostgres   — CreateOrg + AddOrgMember + GetOrgMember +
//     CountOrgOwners + ListOrgsForEmail
//  2. apikey.NewSQLStorePostgres — CreateOwned + LookupSubject + List
//  3. grant.NewSQLStorePostgres  — Upsert (idempotent ON CONFLICT) + GrantedOrgs +
//     Grants + Delete
//
// If the rebind were NOT applied, any of these would fail with a pgx error:
//
//	ERROR: syntax error at or near "$" / could not determine data type of parameter
//
// because the pgx stdlib driver only understands "$N" ordinal placeholders, not
// the "?" positional form the hand-written SQL uses.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/grant"
	"github.com/ivanzzeth/specula/internal/org"
)

// mtRunID returns a short random hex suffix used to make test-owned rows
// unique across repeated runs against the same Postgres instance.
func mtRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func TestMultiTenantPostgres(t *testing.T) {
	dsn := os.Getenv(envTestDSN)
	if dsn == "" {
		t.Skipf("skipping live-DB multi-tenant test: set %s to a PostgreSQL DSN to enable", envTestDSN)
	}

	// Open a database/sql handle via the pgx stdlib driver (same driver used by
	// the multi-tenant SQL stores in production).
	db, err := OpenSQLDB(dsn)
	require.NoError(t, err, "OpenSQLDB")
	t.Cleanup(func() { db.Close() })

	// Apply all embedded goose migrations (idempotent — every statement uses IF
	// NOT EXISTS). This ensures the orgs / org_members / api_keys /
	// resource_grants tables exist even on a fresh database.
	require.NoError(t, Migrate(db), "Migrate")

	// Unique org ID scoped to this test run so parallel or repeated invocations
	// do not collide on the unique index orgs_slug.
	testOrgID := "pg_mt_" + mtRunID()

	// Cleanup: remove all rows written by this run, keyed to testOrgID. Uses raw
	// $N placeholders because this code bypasses the store layer (direct cleanup).
	t.Cleanup(func() {
		ctx := context.Background()
		db.ExecContext(ctx, `DELETE FROM resource_grants WHERE subject_id = $1`, testOrgID) //nolint:errcheck
		db.ExecContext(ctx, `DELETE FROM api_keys WHERE org_id = $1`, testOrgID)            //nolint:errcheck
		db.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = $1`, testOrgID)         //nolint:errcheck
		db.ExecContext(ctx, `DELETE FROM org_invitations WHERE org_id = $1`, testOrgID)     //nolint:errcheck
		db.ExecContext(ctx, `DELETE FROM repos WHERE org_id = $1`, testOrgID)               //nolint:errcheck
		db.ExecContext(ctx, `DELETE FROM orgs WHERE id = $1`, testOrgID)                    //nolint:errcheck
	})

	ctx := context.Background()

	// ── 1. org store ──────────────────────────────────────────────────────────
	//
	// CreateOrg   → INSERT INTO orgs (6 ?) — proves $1…$6 rebind.
	// GetOrg      → SELECT … WHERE id = ?  — proves $1 rebind.
	// AddOrgMember→ INSERT INTO org_members (6 ?) — $1…$6.
	// GetOrgMember→ SELECT … WHERE org_id = ? AND email = ? — $1,$2.
	// CountOrgOwners → SELECT COUNT(1) … WHERE org_id = ? AND role = ? — $1,$2.
	// ListOrgsForEmail → JOIN query WHERE email = ? — $1.

	orgStore := org.NewSQLStorePostgres(db)

	o := &org.Org{
		ID:        testOrgID,
		Name:      "PG Multi-Tenant Test Org",
		Slug:      testOrgID, // unique slug (same as ID; no spaces)
		CreatedBy: "user:1",
	}
	require.NoError(t, orgStore.CreateOrg(ctx, o), "CreateOrg on postgres")
	assert.Equal(t, org.StatusActive, o.Status, "Status defaulted to active")

	gotOrg, err := orgStore.GetOrg(ctx, testOrgID)
	require.NoError(t, err, "GetOrg on postgres")
	assert.Equal(t, "PG Multi-Tenant Test Org", gotOrg.Name)

	m := &org.Member{
		OrgID: testOrgID,
		Email: "alice-mt@example.com",
		Role:  org.RoleOwner,
	}
	require.NoError(t, orgStore.AddOrgMember(ctx, m), "AddOrgMember on postgres")

	mem, err := orgStore.GetOrgMember(ctx, testOrgID, "alice-mt@example.com")
	require.NoError(t, err, "GetOrgMember on postgres")
	assert.Equal(t, org.RoleOwner, mem.Role)
	assert.Equal(t, testOrgID, mem.OrgID)

	owners, err := orgStore.CountOrgOwners(ctx, testOrgID)
	require.NoError(t, err, "CountOrgOwners on postgres")
	assert.Equal(t, 1, owners)

	memberOrgs, err := orgStore.ListOrgsForEmail(ctx, "alice-mt@example.com")
	require.NoError(t, err, "ListOrgsForEmail on postgres")
	require.Len(t, memberOrgs, 1, "alice must be in exactly 1 org")
	assert.Equal(t, testOrgID, memberOrgs[0].ID)
	assert.Equal(t, org.RoleOwner, memberOrgs[0].Role)

	// ── 2. apikey store ───────────────────────────────────────────────────────
	//
	// sqlInsert (Create/CreateOwned) → INSERT INTO api_keys (9 ?) — $1…$9.
	// LookupSubject → SELECT … WHERE key_hash=? then UPDATE … WHERE key_hash=?
	//   — two separate queries; proves both SELECT ? and UPDATE ?,? rebind.
	// List          → SELECT … WHERE org_id=? ORDER BY … — $1.

	keyStore := apikey.NewSQLStorePostgres(db)

	keyID, rawKey, err := keyStore.CreateOwned(testOrgID, "user:1", "pg-ci-key")
	require.NoError(t, err, "CreateOwned on postgres")
	assert.NotEmpty(t, keyID)
	assert.NotEmpty(t, rawKey)

	gotOrgID, gotSubj, ok := keyStore.LookupSubject(rawKey)
	require.True(t, ok, "LookupSubject must succeed for a fresh key")
	assert.Equal(t, testOrgID, gotOrgID, "LookupSubject returns correct org")
	assert.Equal(t, apikey.SubjectID(keyID), gotSubj, "LookupSubject returns correct synthetic subject")

	keys, err := keyStore.List(testOrgID)
	require.NoError(t, err, "List on postgres")
	require.Len(t, keys, 1, "List must return the 1 key we created")
	assert.Equal(t, keyID, keys[0].ID)
	assert.NotNil(t, keys[0].LastUsedAt, "LastUsedAt set by LookupSubject touch")

	// ── 3. grant store ────────────────────────────────────────────────────────
	//
	// Upsert (insert path) → INSERT … VALUES (7 ?) ON CONFLICT DO UPDATE — $1…$7.
	// Upsert (update path) → same statement; ON CONFLICT branch executes.
	// GrantedOrgs          → SELECT … WHERE resource_type=? AND resource_id=? AND subject_type=? — $1,$2,$3.
	// Grants               → SELECT … WHERE resource_type=? AND resource_id=? ORDER BY … — $1,$2.
	// Delete               → DELETE … WHERE resource_type=? AND resource_id=? AND subject_type=? AND subject_id=? — $1…$4.

	grantStore := grant.NewSQLStorePostgres(db)

	testResource := testOrgID + "/myrepo"

	// First upsert: INSERT path.
	require.NoError(t, grantStore.Upsert(grant.Grant{
		ResourceType: "repo",
		ResourceID:   testResource,
		SubjectType:  grant.SubjectOrg,
		SubjectID:    testOrgID,
		Access:       grant.AccessRead,
		GrantedBy:    "user:1",
	}), "Upsert (insert) on postgres")

	// Second upsert: ON CONFLICT DO UPDATE path — access read → write.
	require.NoError(t, grantStore.Upsert(grant.Grant{
		ResourceType: "repo",
		ResourceID:   testResource,
		SubjectType:  grant.SubjectOrg,
		SubjectID:    testOrgID,
		Access:       grant.AccessWrite,
		GrantedBy:    "user:1",
	}), "Upsert (ON CONFLICT update) on postgres")

	grantedOrgs := grantStore.GrantedOrgs("repo", testResource)
	assert.Contains(t, grantedOrgs, testOrgID, "GrantedOrgs must contain the test org")

	grants, err := grantStore.Grants("repo", testResource)
	require.NoError(t, err, "Grants on postgres")
	require.Len(t, grants, 1, "upsert is idempotent — exactly 1 grant row")
	assert.Equal(t, grant.AccessWrite, grants[0].Access, "access updated by second Upsert")

	require.NoError(t, grantStore.Delete("repo", testResource, grant.SubjectOrg, testOrgID),
		"Delete on postgres")
	assert.Empty(t, grantStore.GrantedOrgs("repo", testResource),
		"GrantedOrgs must be empty after Delete")
}
