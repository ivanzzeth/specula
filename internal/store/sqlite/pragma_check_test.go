package sqlite

import (
	"path/filepath"
	"testing"
)

// TestPragmasActuallyApply guards against a silently-ignored DSN: modernc accepts
// unknown query params without error, so a typo'd _pragma would leave
// busy_timeout at 0 and reintroduce hard "database is locked" failures.
func TestPragmasActuallyApply(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var busy int
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000 — the DSN pragma was ignored", busy)
	}
	var mode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	t.Logf("busy_timeout=%d journal_mode=%s", busy, mode)
}
