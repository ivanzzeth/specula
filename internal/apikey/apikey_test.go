// External (blackbox) tests for internal/apikey covering the four required
// scenarios:
//
//  1. mint → LookupSubject roundtrip
//  2. hash-at-rest: plaintext key is never stored (SQLStore via direct DB query;
//     MemStore whitebox proof is in apikey_internal_test.go)
//  3. revoked / expired key: LookupSubject fails
//  4. org-scoped list: List returns only the queried org's keys
//
// Tests run against both MemStore and SQLStore. The SQLStore is backed by a
// temporary SQLite database with all goose migrations applied via
// sqlite.NewSQLiteStore (0002_multitenant creates the api_keys table).
//
// NOTE: The internal whitebox hash-at-rest test for MemStore lives in
// apikey_internal_test.go (package apikey) because importing
// internal/store/sqlite here would create a cycle:
//
//	package apikey_test → internal/store/sqlite → internal/auth → internal/apikey
package apikey_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ---- test helpers -----------------------------------------------------------

// newTestSQLStore opens a temp SQLite database, applies all embedded goose
// migrations (including 0002_multitenant which creates api_keys), and returns
// both the SQLStore and the raw *sql.DB for direct SQL assertions.
func newTestSQLStore(t *testing.T) (*apikey.SQLStore, *sql.DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "apikey_test.db")
	st, err := sqlite.NewSQLiteStore(dsn)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return apikey.NewSQLStore(st.DB()), st.DB()
}

// sha256hex returns the SHA-256 hex of s — used in blackbox hash-at-rest
// assertions without needing package-internal access to hashKey.
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// storeCase bundles a named Store under test with an optional raw DB for
// SQL-level assertions (non-nil only for SQLStore cases).
type storeCase struct {
	name string
	s    apikey.Store
	db   *sql.DB
}

// storeCases returns a MemStore and an SQLStore for table-driven tests.
func storeCases(t *testing.T) []storeCase {
	t.Helper()
	sqlS, db := newTestSQLStore(t)
	return []storeCase{
		{name: "mem", s: apikey.NewMemStore()},
		{name: "sql", s: sqlS, db: db},
	}
}

// ---- 1. Mint → LookupSubject roundtrip -------------------------------------

// TestMintLookupRoundtrip verifies that a freshly created key can be looked up
// and returns the correct (orgID, "apikey:<id>") pair. It also confirms the
// display prefix is set/truncated and LastUsedAt is populated after a lookup.
func TestMintLookupRoundtrip(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			const orgID = "acme"

			id, rawKey, err := tc.s.Create(orgID, "ci")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if id == "" || rawKey == "" {
				t.Fatalf("Create returned empty id=%q or rawKey=%q", id, rawKey)
			}
			if len(rawKey) < len(apikey.KeyPrefix) || rawKey[:len(apikey.KeyPrefix)] != apikey.KeyPrefix {
				t.Fatalf("rawKey prefix mismatch: got %q, want prefix %q", rawKey, apikey.KeyPrefix)
			}

			// LookupSubject must succeed and return correct org + synthetic subject.
			gotOrg, gotSubject, ok := tc.s.LookupSubject(rawKey)
			if !ok {
				t.Fatal("LookupSubject: ok=false; want true")
			}
			if gotOrg != orgID {
				t.Fatalf("LookupSubject orgID = %q; want %q", gotOrg, orgID)
			}
			wantSubject := apikey.SubjectID(id)
			if gotSubject != wantSubject {
				t.Fatalf("LookupSubject subject = %q; want %q", gotSubject, wantSubject)
			}
			if gotSubject != apikey.SubjectPrefix+id {
				t.Fatalf("subject %q does not equal SubjectPrefix+id", gotSubject)
			}

			// Get must return entry with non-empty/truncated prefix and LastUsedAt set.
			info, ok := tc.s.Get(orgID, id)
			if !ok {
				t.Fatal("Get after LookupSubject: ok=false")
			}
			if info.OrgID != orgID {
				t.Fatalf("Get OrgID = %q; want %q", info.OrgID, orgID)
			}
			if info.Label != "ci" {
				t.Fatalf("Get Label = %q; want ci", info.Label)
			}
			if info.Prefix == "" {
				t.Fatal("Get Prefix is empty; want display prefix")
			}
			if info.Prefix == rawKey {
				t.Fatal("Get Prefix equals rawKey; prefix must be truncated (never the full key)")
			}
			if info.LastUsedAt == nil {
				t.Fatal("Get LastUsedAt = nil after a successful LookupSubject; want set")
			}
			if info.Revoked {
				t.Fatal("Get Revoked = true for a fresh key; want false")
			}
		})
	}
}

// TestMintLookupEmptyOrgDefault confirms that Create("", …) falls back to
// apikey.DefaultOrgID, preventing cross-org confusion.
func TestMintLookupEmptyOrgDefault(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			id, rawKey, err := tc.s.Create("", "fallback")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			info, ok := tc.s.Get(apikey.DefaultOrgID, id)
			if !ok {
				t.Fatalf("Get(DefaultOrgID, %q): not found", id)
			}
			if info.OrgID != apikey.DefaultOrgID {
				t.Fatalf("OrgID = %q; want DefaultOrgID=%q", info.OrgID, apikey.DefaultOrgID)
			}
			gotOrg, _, ok := tc.s.LookupSubject(rawKey)
			if !ok || gotOrg != apikey.DefaultOrgID {
				t.Fatalf("LookupSubject = (%q,_,%v); want (%q,_,true)", gotOrg, ok, apikey.DefaultOrgID)
			}
		})
	}
}

// TestCreateOwnedSetsUserID confirms CreateOwned records the issuing userID.
func TestCreateOwnedSetsUserID(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			const (
				orgID  = "myorg"
				userID = "user:42"
			)
			id, rawKey, err := tc.s.CreateOwned(orgID, userID, "owned")
			if err != nil {
				t.Fatalf("CreateOwned: %v", err)
			}
			_, subj, ok := tc.s.LookupSubject(rawKey)
			if !ok {
				t.Fatal("LookupSubject: ok=false")
			}
			if subj != apikey.SubjectID(id) {
				t.Fatalf("subject = %q; want %q", subj, apikey.SubjectID(id))
			}
			info, ok := tc.s.Get(orgID, id)
			if !ok {
				t.Fatal("Get: not found")
			}
			if info.UserID != userID {
				t.Fatalf("UserID = %q; want %q", info.UserID, userID)
			}
		})
	}
}

// ---- 2. Hash-at-rest: plaintext never stored --------------------------------

// TestHashAtRest_SQLStore verifies via direct SQL that api_keys.key_hash is a
// 64-char SHA-256 hex string, not the plaintext.
// (MemStore whitebox proof is in apikey_internal_test.go.)
func TestHashAtRest_SQLStore(t *testing.T) {
	s, db := newTestSQLStore(t)
	_, rawKey, err := s.Create("acme", "hash-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var storedHash string
	if err := db.QueryRow(`SELECT key_hash FROM api_keys LIMIT 1`).Scan(&storedHash); err != nil {
		t.Fatalf("query key_hash: %v", err)
	}

	if storedHash == rawKey {
		t.Fatalf("plaintext key stored as key_hash: hash-at-rest violated")
	}
	// Must be a valid 64-char SHA-256 hex digest.
	if len(storedHash) != 64 {
		t.Fatalf("key_hash length = %d; want 64 (SHA-256 hex)", len(storedHash))
	}
	if expected := sha256hex(rawKey); storedHash != expected {
		t.Fatalf("key_hash = %q; want SHA-256 hex %q", storedHash, expected)
	}
}

// ---- 3. Revoked / expired key: LookupSubject fails -------------------------

// TestRevokedKeyLookupFails exercises the full revocation cycle for each store.
func TestRevokedKeyLookupFails(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			const orgID = "acme"
			id, rawKey, err := tc.s.Create(orgID, "to-revoke")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Sanity: valid before revocation.
			if _, _, ok := tc.s.LookupSubject(rawKey); !ok {
				t.Fatal("LookupSubject before revoke: ok=false; want true")
			}

			// First Revoke → true.
			revoked, err := tc.s.Revoke(orgID, id)
			if err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			if !revoked {
				t.Fatal("Revoke: returned false; want true on first call")
			}

			// LookupSubject must reject the key.
			if _, _, ok := tc.s.LookupSubject(rawKey); ok {
				t.Fatal("LookupSubject after Revoke: ok=true; want false")
			}

			// Get still returns the entry with Revoked=true.
			info, ok := tc.s.Get(orgID, id)
			if !ok {
				t.Fatal("Get after Revoke: not found")
			}
			if !info.Revoked {
				t.Fatal("Get after Revoke: Revoked=false; want true")
			}

			// Second Revoke → false (no-op).
			revoked2, err := tc.s.Revoke(orgID, id)
			if err != nil {
				t.Fatalf("second Revoke: %v", err)
			}
			if revoked2 {
				t.Fatal("second Revoke: returned true; want false (already revoked)")
			}
		})
	}
}

// TestExpiredKeyLookupFails_SQLStore verifies that a key with expires_at in
// the past is rejected by LookupSubject (same as revoked — uniform silent
// failure). We inject the past timestamp via a direct SQL UPDATE so there is
// no clock-skew race.
func TestExpiredKeyLookupFails_SQLStore(t *testing.T) {
	s, db := newTestSQLStore(t)

	// Create a normal key, then backdate its expires_at.
	id, rawKey, err := s.Create("acme", "soon-expired")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	past := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE api_keys SET expires_at=? WHERE id=?`, past, id); err != nil {
		t.Fatalf("UPDATE expires_at: %v", err)
	}

	// Key must now be rejected.
	if _, _, ok := s.LookupSubject(rawKey); ok {
		t.Fatal("LookupSubject on expired key: ok=true; want false")
	}

	// Get still returns the entry (soft data, not deleted on expiry).
	if _, ok := s.Get("acme", id); !ok {
		t.Fatal("Get expired key: not found; want entry present")
	}
}

// TestRevokeMissingKey verifies that Revoke on a non-existent id is a no-op.
func TestRevokeMissingKey(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ok, err := tc.s.Revoke("org", "does-not-exist")
			if err != nil {
				t.Fatalf("Revoke(missing): %v", err)
			}
			if ok {
				t.Fatal("Revoke(missing): returned true; want false")
			}
		})
	}
}

// TestLookupSubjectUnknown confirms that an unrecognised token returns false.
func TestLookupSubjectUnknown(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if org, subj, ok := tc.s.LookupSubject("spck_notakey"); ok {
				t.Fatalf("LookupSubject(unknown) = (%q,%q,true); want (_,_,false)", org, subj)
			}
		})
	}
}

// ---- 4. Org-scoped list ----------------------------------------------------

// TestOrgScopedList verifies that List returns only keys for the queried org
// and that the result is sorted newest→oldest.
func TestOrgScopedList(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			const orgA, orgB = "alpha", "beta"

			// Create 2 keys in org-A and 1 in org-B.
			id1, _, err := tc.s.Create(orgA, "key-1")
			if err != nil {
				t.Fatalf("Create orgA key-1: %v", err)
			}
			// Ensure distinct created_at timestamps (SQLite TEXT ordering by
			// RFC3339 string, which has millisecond resolution at minimum).
			time.Sleep(5 * time.Millisecond)
			id2, _, err := tc.s.Create(orgA, "key-2")
			if err != nil {
				t.Fatalf("Create orgA key-2: %v", err)
			}
			if _, _, err := tc.s.Create(orgB, "key-b"); err != nil {
				t.Fatalf("Create orgB: %v", err)
			}

			// List org-A: exactly 2 entries, both with OrgID = orgA.
			listA, err := tc.s.List(orgA)
			if err != nil {
				t.Fatalf("List(orgA): %v", err)
			}
			if len(listA) != 2 {
				t.Fatalf("List(orgA) len = %d; want 2", len(listA))
			}
			for _, info := range listA {
				if info.OrgID != orgA {
					t.Fatalf("List(orgA) entry has OrgID=%q; want %q", info.OrgID, orgA)
				}
			}

			// List org-B: exactly 1 entry.
			listB, err := tc.s.List(orgB)
			if err != nil {
				t.Fatalf("List(orgB): %v", err)
			}
			if len(listB) != 1 {
				t.Fatalf("List(orgB) len = %d; want 1", len(listB))
			}
			if listB[0].OrgID != orgB {
				t.Fatalf("List(orgB)[0].OrgID = %q; want %q", listB[0].OrgID, orgB)
			}

			// Newest→oldest: id2 was created after id1, so it must come first.
			if listA[0].ID != id2 {
				t.Fatalf("List(orgA)[0].ID = %q; want newest id %q", listA[0].ID, id2)
			}
			if listA[1].ID != id1 {
				t.Fatalf("List(orgA)[1].ID = %q; want oldest id %q", listA[1].ID, id1)
			}
		})
	}
}

// TestOrgScopedGet verifies that Get with the wrong org returns not-found
// (cross-org isolation).
func TestOrgScopedGet(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			id, _, err := tc.s.Create("orgA", "x")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, ok := tc.s.Get("orgA", id); !ok {
				t.Fatal("Get(orgA, id): not found; want found")
			}
			if _, ok := tc.s.Get("orgB", id); ok {
				t.Fatal("Get(orgB, id_in_orgA): found; want cross-org isolation (not found)")
			}
		})
	}
}

// TestOrgScopedRevoke verifies that Revoke with the wrong org is a no-op —
// the key remains valid in its real org.
func TestOrgScopedRevoke(t *testing.T) {
	for _, tc := range storeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			id, rawKey, err := tc.s.Create("orgA", "x")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			ok, err := tc.s.Revoke("orgB", id) // wrong org
			if err != nil {
				t.Fatalf("Revoke(wrong org): %v", err)
			}
			if ok {
				t.Fatal("Revoke with wrong org: returned true; want false")
			}
			// Key must still be valid.
			if _, _, ok := tc.s.LookupSubject(rawKey); !ok {
				t.Fatal("LookupSubject after cross-org Revoke attempt: ok=false; want still valid")
			}
		})
	}
}

// TestSubjectID pins the synthetic subject format.
func TestSubjectID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc123", "apikey:abc123"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := apikey.SubjectID(tc.in); got != tc.want {
			t.Errorf("SubjectID(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
