package postgres

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"

	"github.com/pressly/goose/v3"
)

// openAtVersion opens a fresh connection against the SPECULA_TEST_POSTGRES_DSN
// database (dropping the goose bookkeeping table so each test starts clean),
// runs the embedded migrations up to (and including) version, and returns the
// handle for direct seeds. White-box (package postgres, not postgres_test) so
// it can reach migrationsFS and stop short of the migration under test.
//
// Mirrors internal/store/sqlite's openAtVersion/upToLatest test helpers so
// the same legacy-org_default scenario is proven on both dialects.
func openAtVersion(t *testing.T, version int64) *sql.DB {
	t.Helper()
	dsn := os.Getenv(envTestDSN)
	if dsn == "" {
		t.Skipf("skipping live-DB test: set %s to a PostgreSQL DSN to enable", envTestDSN)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Reset to a clean slate: drop every table (including goose's own) so
	// this test's migration run starts from nothing, independent of any
	// other test that shares the same live DSN.
	resetSQL := []string{
		`DROP TABLE IF EXISTS resource_grants, repo_tags, repos, api_keys,
			org_invitations, org_members, orgs, apt_index_highwater,
			verification_events, upstream_blocks, stats_series_samples,
			apt_pool_pins, apt_index_pins, system_config, cache_origin,
			cache_pinned, mutable_entries, cache_entries, users,
			goose_db_version CASCADE`,
	}
	for _, q := range resetSQL {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}

	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}
	return db
}

// upToLatest runs every remaining pending migration (including
// 012_default_org_id_align.sql under test) against db.
func upToLatest(t *testing.T, db *sql.DB) {
	t.Helper()
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// TestMigration012_RewritesOrgIDAndCollapsesDuplicateRepos is the PostgreSQL
// mirror of internal/store/sqlite's TestMigration0012_* — same legacy shape
// (an "org_default" org owning both "default/<repo>" and "org_default/<repo>"
// for the same repo, plus membership/api_key/tag/grant rows scoped to the
// legacy org id), same expected outcome.
func TestMigration012_RewritesOrgIDAndCollapsesDuplicateRepos(t *testing.T) {
	db := openAtVersion(t, 11)

	seeds := []string{
		`INSERT INTO orgs (id, name, slug, status, created_by, created_at)
		 VALUES ('org_default', 'Default', 'default', 'active', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO org_members (id, org_id, email, role, created_at)
		 VALUES ('mem1', 'org_default', 'owner@x.com', 'owner', '2026-01-01T00:00:00Z')`,
		`INSERT INTO org_invitations (id, org_id, email, role, token, status, created_at)
		 VALUES ('inv1', 'org_default', 'invitee@x.com', 'viewer', 'tok1', 'pending', '2026-01-01T00:00:00Z')`,
		`INSERT INTO api_keys (key_hash, id, label, prefix, org_id, user_id, created_at)
		 VALUES ('hash1', 'key1', 'ci', 'spck_abc', 'org_default', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoA_slug', 'org_default', 'default/app', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoA_id', 'org_default', 'org_default/app', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoB_id', 'org_default', 'org_default/solo', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repo_tags (repo_id, tag, digest, updated_at)
		 VALUES ('repoA_slug', 'latest', 'sha256:aaa', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repo_tags (repo_id, tag, digest, updated_at)
		 VALUES ('repoA_id', 'latest', 'sha256:bbb', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repo_tags (repo_id, tag, digest, updated_at)
		 VALUES ('repoB_id', 'latest', 'sha256:ccc', '2026-01-01T00:00:00Z')`,
		`INSERT INTO resource_grants (resource_type, resource_id, subject_type, subject_id, access, granted_by, created_at)
		 VALUES ('repo', 'repoA_id', 'user', 'user:2', 'read', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO resource_grants (resource_type, resource_id, subject_type, subject_id, access, granted_by, created_at)
		 VALUES ('repo', 'repoB_id', 'user', 'user:2', 'read', 'user:1', '2026-01-01T00:00:00Z')`,
	}
	for _, q := range seeds {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	upToLatest(t, db)

	var id, slug string
	if err := db.QueryRow(`SELECT id, slug FROM orgs`).Scan(&id, &slug); err != nil {
		t.Fatalf("query orgs: %v", err)
	}
	if id != "default" || slug != "default" {
		t.Fatalf("orgs = (id=%q, slug=%q), want (default, default)", id, slug)
	}

	for table, col := range map[string]string{
		"org_members":     "org_id",
		"org_invitations": "org_id",
		"api_keys":        "org_id",
	} {
		var got string
		if err := db.QueryRow(`SELECT ` + col + ` FROM ` + table).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if got != "default" {
			t.Fatalf("%s.%s = %q, want %q", table, col, got, "default")
		}
	}

	rows, err := db.Query(`SELECT id, org_id, name FROM repos ORDER BY name`)
	if err != nil {
		t.Fatalf("query repos: %v", err)
	}
	type repoRow struct{ id, orgID, name string }
	var got []repoRow
	for rows.Next() {
		var r repoRow
		if err := rows.Scan(&r.id, &r.orgID, &r.name); err != nil {
			t.Fatalf("scan repo: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()

	want := []repoRow{
		{id: "repoA_slug", orgID: "default", name: "default/app"},
		{id: "repoB_id", orgID: "default", name: "default/solo"},
	}
	if len(got) != len(want) {
		t.Fatalf("repos = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("repos[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	var tagCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM repo_tags WHERE repo_id = 'repoA_id'`).Scan(&tagCount); err != nil {
		t.Fatalf("count orphan tags: %v", err)
	}
	if tagCount != 0 {
		t.Fatalf("repo_tags for deleted repoA_id = %d, want 0 (orphan not cleaned up)", tagCount)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM repo_tags WHERE repo_id IN ('repoA_slug', 'repoB_id')`).Scan(&tagCount); err != nil {
		t.Fatalf("count surviving tags: %v", err)
	}
	if tagCount != 2 {
		t.Fatalf("repo_tags for surviving repos = %d, want 2", tagCount)
	}

	var grantCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM resource_grants WHERE resource_id = 'repoA_id'`).Scan(&grantCount); err != nil {
		t.Fatalf("count orphan grants: %v", err)
	}
	if grantCount != 0 {
		t.Fatalf("resource_grants for deleted repoA_id = %d, want 0 (orphan not cleaned up)", grantCount)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM resource_grants WHERE resource_id = 'repoB_id'`).Scan(&grantCount); err != nil {
		t.Fatalf("count surviving grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("resource_grants for repoB_id = %d, want 1", grantCount)
	}

	// Idempotency: re-running Up() (nothing pending) must not error or
	// change any row.
	upToLatest(t, db)
	rows2, err := db.Query(`SELECT id, org_id, name FROM repos ORDER BY name`)
	if err != nil {
		t.Fatalf("re-query repos: %v", err)
	}
	var got2 []repoRow
	for rows2.Next() {
		var r repoRow
		if err := rows2.Scan(&r.id, &r.orgID, &r.name); err != nil {
			t.Fatalf("scan repo: %v", err)
		}
		got2 = append(got2, r)
	}
	rows2.Close()
	if len(got2) != len(want) {
		t.Fatalf("repos after no-op re-run = %+v, want unchanged %+v", got2, want)
	}
}

// TestMigration012_NoLegacyRows_IsNoOp guards the fresh-DB path: a database
// that never had an "org_default" row must migrate cleanly with zero rows
// touched.
func TestMigration012_NoLegacyRows_IsNoOp(t *testing.T) {
	db := openAtVersion(t, 11)

	if _, err := db.Exec(`INSERT INTO orgs (id, name, slug, status, created_by, created_at)
		VALUES ('default', 'Default', 'default', 'active', 'user:1', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		VALUES ('repo1', 'default', 'default/app', 'private', 'user:1', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	upToLatest(t, db)

	var id string
	if err := db.QueryRow(`SELECT id FROM orgs`).Scan(&id); err != nil {
		t.Fatalf("query orgs: %v", err)
	}
	if id != "default" {
		t.Fatalf("orgs.id = %q, want unchanged %q", id, "default")
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM repos`).Scan(&name); err != nil {
		t.Fatalf("query repos: %v", err)
	}
	if name != "default/app" {
		t.Fatalf("repos.name = %q, want unchanged %q", name, "default/app")
	}
}
