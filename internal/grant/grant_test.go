package grant

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite" // registers "sqlite" driver
)

// createTestDB opens an in-memory SQLite database and creates the
// resource_grants table (matching the 0002_multitenant migration DDL). Using
// an in-memory database keeps the test self-contained and avoids importing the
// internal/store/sqlite package (no import cycle risk).
func createTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE resource_grants (
		    resource_type TEXT NOT NULL,
		    resource_id   TEXT NOT NULL,
		    subject_type  TEXT NOT NULL,
		    subject_id    TEXT NOT NULL,
		    access        TEXT NOT NULL DEFAULT 'read',
		    granted_by    TEXT NOT NULL DEFAULT '',
		    created_at    TEXT NOT NULL DEFAULT '',
		    PRIMARY KEY (resource_type, resource_id, subject_type, subject_id)
		)`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create resource_grants table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// storeChecks is the dialect-neutral contract suite for grant.Store.
// Run against both MemStore and SQLStore to ensure behavioral equivalence.
func storeChecks(t *testing.T, s Store) {
	t.Helper()

	// Empty resource: no grants, no grantedOrgs.
	if gs, err := s.Grants("repo", "r1"); err != nil || len(gs) != 0 {
		t.Fatalf("empty grants: got %v (err=%v)", gs, err)
	}
	if orgs := s.GrantedOrgs("repo", "r1"); len(orgs) != 0 {
		t.Fatalf("empty granted orgs: got %v", orgs)
	}

	// Upsert orgA (write) + orgB (read) + userU (read, default).
	if err := s.Upsert(Grant{
		ResourceType: "repo", ResourceID: "r1",
		SubjectType: SubjectOrg, SubjectID: "orgA",
		Access: AccessWrite, GrantedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Grant{
		ResourceType: "repo", ResourceID: "r1",
		SubjectType: SubjectOrg, SubjectID: "orgB",
		Access: AccessRead,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Grant{
		ResourceType: "repo", ResourceID: "r1",
		SubjectType: SubjectUser, SubjectID: "userU",
		// Access intentionally omitted to verify normAccess default.
	}); err != nil {
		t.Fatal(err)
	}

	// GrantedOrgs returns only org subjects (not user subjects).
	orgs := s.GrantedOrgs("repo", "r1")
	if len(orgs) != 2 || !containsStr(orgs, "orgA") || !containsStr(orgs, "orgB") {
		t.Fatalf("GrantedOrgs = %v, want [orgA orgB]", orgs)
	}

	// Grants returns all three.
	gs, err := s.Grants("repo", "r1")
	if err != nil || len(gs) != 3 {
		t.Fatalf("Grants = %v (err=%v), want 3 entries", gs, err)
	}

	// Idempotent upsert: change orgB access from read → write; count stays 3.
	if err := s.Upsert(Grant{
		ResourceType: "repo", ResourceID: "r1",
		SubjectType: SubjectOrg, SubjectID: "orgB",
		Access: AccessWrite,
	}); err != nil {
		t.Fatal(err)
	}
	if orgs := s.GrantedOrgs("repo", "r1"); len(orgs) != 2 {
		t.Fatalf("after upsert orgs = %v, want 2", orgs)
	}
	gs2, _ := s.Grants("repo", "r1")
	var orgBAccess string
	for _, g := range gs2 {
		if g.SubjectType == SubjectOrg && g.SubjectID == "orgB" {
			orgBAccess = g.Access
		}
	}
	if orgBAccess != AccessWrite {
		t.Fatalf("upsert did not update orgB access: got %q, want %q", orgBAccess, AccessWrite)
	}

	// Empty access normalises to read (userU was inserted without Access).
	for _, g := range gs2 {
		if g.SubjectType == SubjectUser && g.Access != AccessRead {
			t.Fatalf("user default access = %q, want %q", g.Access, AccessRead)
		}
	}

	// Isolation: a different resource ID has no grants.
	if orgs := s.GrantedOrgs("repo", "r2"); len(orgs) != 0 {
		t.Fatalf("r2 should have no grants, got %v", orgs)
	}

	// Delete orgA → only orgB remains.
	if err := s.Delete("repo", "r1", SubjectOrg, "orgA"); err != nil {
		t.Fatal(err)
	}
	if orgs := s.GrantedOrgs("repo", "r1"); len(orgs) != 1 || orgs[0] != "orgB" {
		t.Fatalf("after Delete orgA: GrantedOrgs = %v, want [orgB]", orgs)
	}

	// Delete missing entry is a no-op (no error).
	if err := s.Delete("repo", "r1", SubjectOrg, "nope"); err != nil {
		t.Fatalf("Delete missing should be no-op: %v", err)
	}
}

func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func TestMemStore(t *testing.T) {
	storeChecks(t, NewMemStore())
}

func TestSQLStore(t *testing.T) {
	db := createTestDB(t)
	storeChecks(t, NewSQLStore(db))
}
