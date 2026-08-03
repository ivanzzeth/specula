package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// openAtVersion opens a fresh SQLite db, runs the embedded goose migrations
// up to (and including) version, and returns the raw handle for direct seeds.
// White-box (package sqlite, not sqlite_test) so it can reach migrationsFS
// and stop short of the migration under test.
func openAtVersion(t *testing.T, version int64) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "migration.db")
	db, err := sql.Open("sqlite", withPragmas(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}
	return db
}

// upToLatest runs every remaining pending migration (including 0012 under
// test) against db.
func upToLatest(t *testing.T, db *sql.DB) {
	t.Helper()
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// TestMigration0012_RewritesOrgIDAndCollapsesDuplicateRepos reproduces the
// live-Specula shape described in the DefaultOrgID unification: a
// "org_default" org that owns BOTH a "default/<repo>" row (pushed by slug)
// and an "org_default/<repo>" row (pushed by id) for the same repo, plus
// membership/api_key/tag/grant rows scoped to the legacy org id. Migration
// 0012 must land on org.DefaultOrgID == "default" for every table, keep the
// canonical "default/<repo>" row, drop the "org_default/<repo>" duplicate
// (and its now-orphaned repo_tags / resource_grants), and rename any
// "org_default/<repo>" that has no canonical counterpart instead of dropping
// it.
func TestMigration0012_RewritesOrgIDAndCollapsesDuplicateRepos(t *testing.T) {
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
		// The duplicate pair: same logical repo, pushed once by slug and once
		// by id, both landing under org_id='org_default' (matches the
		// prompt's stated live shape).
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoA_slug', 'org_default', 'default/app', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoA_id', 'org_default', 'org_default/app', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		// A second repo that ONLY ever got pushed by id — no canonical
		// counterpart exists, so it must be renamed in place, not dropped.
		`INSERT INTO repos (id, org_id, name, visibility, owner_user_id, created_at)
		 VALUES ('repoB_id', 'org_default', 'org_default/solo', 'private', 'user:1', '2026-01-01T00:00:00Z')`,
		// Tags/grants on the duplicate-to-be-deleted row must not survive as
		// orphans; tags/grants on the surviving/renamed rows must.
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

	// orgs: id rewritten, slug untouched.
	var id, slug string
	if err := db.QueryRow(`SELECT id, slug FROM orgs`).Scan(&id, &slug); err != nil {
		t.Fatalf("query orgs: %v", err)
	}
	if id != "default" || slug != "default" {
		t.Fatalf("orgs = (id=%q, slug=%q), want (default, default)", id, slug)
	}

	// org_members / org_invitations / api_keys: org_id rewritten.
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

	// repos: exactly two rows remain (the deduped canonical "default/app"
	// and the renamed "default/solo"); the "org_default/app" duplicate is
	// gone.
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

	// repo_tags: the duplicate's orphaned tag row is gone; the canonical and
	// renamed repos keep theirs.
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

	// resource_grants: the orphaned grant on the deleted repo is gone; the
	// grant on the renamed-in-place repo survives.
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

	// Idempotency: re-running goose Up() (nothing pending) and, separately,
	// re-executing the exact 0012 statements against the already-migrated
	// state must not error or change any row — no "org_default" row remains
	// to match any WHERE clause.
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

// TestMigration0012_NoLegacyRows_IsNoOp guards the fresh-DB path: a database
// that never had an "org_default" row (e.g. bootstrapped after this change)
// must migrate cleanly with zero rows touched.
func TestMigration0012_NoLegacyRows_IsNoOp(t *testing.T) {
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
