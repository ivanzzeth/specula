// Internal whitebox tests for apikey — package apikey (same package) so we
// can inspect unexported fields. IMPORTANT: this file must NOT import
// internal/store/sqlite (or anything that transitively imports internal/auth,
// which imports internal/apikey), as that would create an import cycle in the
// test binary.
package apikey

import (
	"testing"
)

// TestHashAtRest_MemStore_Whitebox verifies that the MemStore's internal map
// is keyed by the SHA-256 hash of the raw key, never by the plaintext itself.
// This is a whitebox test because it directly inspects the unexported dyn field.
func TestHashAtRest_MemStore_Whitebox(t *testing.T) {
	m := NewMemStore()
	_, rawKey, err := m.Create("acme", "whitebox")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// The plaintext must NOT appear as a map key.
	if _, found := m.dyn[rawKey]; found {
		t.Fatal("plaintext key stored as dyn map key in MemStore: hash-at-rest violated")
	}

	// The SHA-256 hex hash MUST be the map key.
	h := hashKey(rawKey)
	entry, found := m.dyn[h]
	if !found {
		t.Fatalf("SHA-256 hash %q not found in MemStore dyn map; expected hash-at-rest indexing", h)
	}

	// Sanity: the stored entry has a non-empty ID and the correct org.
	if entry.ID == "" {
		t.Fatal("stored entry ID is empty")
	}
	if entry.OrgID != "acme" {
		t.Fatalf("stored entry OrgID = %q; want acme", entry.OrgID)
	}
}

// TestSubjectID_Internal pins the synthetic subject format from inside the
// package (access to unexported SubjectPrefix constant usage).
func TestSubjectID_Internal(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc123", SubjectPrefix + "abc123"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := SubjectID(tc.in)
		if got != tc.want {
			t.Errorf("SubjectID(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestPrefixOf_Internal pins the display-prefix helper: short keys pass
// through unchanged; long keys are truncated with an ellipsis.
func TestPrefixOf_Internal(t *testing.T) {
	shortKey := "spck_ab"
	if got := prefixOf(shortKey); got != shortKey {
		t.Fatalf("prefixOf(%q) = %q; want unchanged", shortKey, got)
	}

	longKey := KeyPrefix + "abcdefghijklmnopqrstuvwxyz"
	got := prefixOf(longKey)
	if got == longKey {
		t.Fatalf("prefixOf(long) returned full key; want truncated")
	}
	// Must start with the key prefix.
	if got[:len(KeyPrefix)] != KeyPrefix {
		t.Fatalf("prefixOf(long) prefix = %q; want %q", got[:len(KeyPrefix)], KeyPrefix)
	}
	// Must end with the ellipsis rune.
	runes := []rune(got)
	if runes[len(runes)-1] != '…' {
		t.Fatalf("prefixOf(long) does not end with ellipsis: %q", got)
	}
}

// TestCopyKeyInfo_Internal verifies that copyKeyInfo returns an independent
// copy: mutating the copy's pointer fields does not affect the original.
func TestCopyKeyInfo_Internal(t *testing.T) {
	m := NewMemStore()
	_, rawKey, err := m.Create("org1", "label")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Prime LastUsedAt via a lookup.
	if _, _, ok := m.LookupSubject(rawKey); !ok {
		t.Fatal("LookupSubject: ok=false")
	}

	// Find the stored entry.
	m.mu.RLock()
	var orig *KeyInfo
	for _, e := range m.dyn {
		orig = e
		break
	}
	m.mu.RUnlock()

	if orig == nil {
		t.Fatal("no entry found in dyn")
	}

	cp := copyKeyInfo(orig)

	// Mutate the copy's LastUsedAt pointer destination.
	if cp.LastUsedAt == nil {
		t.Fatal("copy LastUsedAt = nil; want set after lookup")
	}
	// The copy's LastUsedAt must be a different pointer than the original's.
	if cp.LastUsedAt == orig.LastUsedAt {
		t.Fatal("copyKeyInfo returned same LastUsedAt pointer; want deep copy")
	}
}
