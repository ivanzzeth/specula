package configstore

// Ported (translated + re-pointed at Specula's SQLite store) from ai-sandbox
// internal/controlplane/config/store_test.go. The contract test storeRoundTrip
// is run against BOTH implementations, which is exactly what keeps MemStore and
// SQLStore from drifting.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// storeRoundTrip exercises the shared Store contract against any implementation.
func storeRoundTrip(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	if _, ok, err := s.Get(ctx, "missing"); err != nil || ok {
		t.Fatalf("Get missing: ok=%v err=%v", ok, err)
	}
	if err := s.Set(ctx, "auth.jwt_secret", "tok-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	pt, ok, err := s.Get(ctx, "auth.jwt_secret")
	if err != nil || !ok || pt != "tok-123" {
		t.Fatalf("Get: %q ok=%v err=%v", pt, ok, err)
	}
	// upsert overwrite
	if err := s.Set(ctx, "auth.jwt_secret", "tok-456"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if pt, _, _ := s.Get(ctx, "auth.jwt_secret"); pt != "tok-456" {
		t.Fatalf("overwrite mismatch: %q", pt)
	}
	_ = s.Set(ctx, "other", "v")
	keys, err := s.Keys(ctx)
	if err != nil || len(keys) != 2 || keys[0] != "auth.jwt_secret" || keys[1] != "other" {
		t.Fatalf("Keys: %v err=%v", keys, err)
	}
	if err := s.Delete(ctx, "auth.jwt_secret"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "auth.jwt_secret"); ok {
		t.Fatal("expected deleted key gone")
	}
}

// testDB opens a migrated SQLite database and returns its *sql.DB handle.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := sqlite.NewSQLiteStore(t.TempDir() + "/cfg.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.DB()
}

func TestMemStoreRoundTrip(t *testing.T) {
	c, _ := NewCrypter(testSecret(t))
	storeRoundTrip(t, NewMemStore(c))
}

func TestSQLStoreRoundTrip(t *testing.T) {
	c, _ := NewCrypter(testSecret(t))
	storeRoundTrip(t, NewSQLStore(testDB(t), c))
}

// TestSQLStorePersistsCiphertext proves the value in the database really is
// ciphertext. This is the assertion that would catch a "disabled crypter that
// silently stores plaintext" regression.
func TestSQLStorePersistsCiphertext(t *testing.T) {
	db := testDB(t)
	c, _ := NewCrypter(testSecret(t))
	s := NewSQLStore(db, c)
	ctx := context.Background()
	const plain = "super-secret-session-signing-key"
	if err := s.Set(ctx, "k", plain); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM system_config WHERE key = ?`, "k").Scan(&raw); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if bytes.Contains(raw, []byte(plain)) {
		t.Fatal("DB stored plaintext, expected ciphertext")
	}
	// A second store over the same key still decrypts (key is external, the
	// stored value is self-contained).
	s2 := NewSQLStore(db, c)
	if pt, ok, _ := s2.Get(ctx, "k"); !ok || pt != plain {
		t.Fatalf("re-read: %q ok=%v", pt, ok)
	}
}

// TestSQLStoreWrongMasterKeyFailsClosed is Specula-local and covers the
// operational accident the reference did not: an operator rotates
// auth.config_secret without re-encrypting. The read must FAIL, not return
// garbage — a silently-empty jwt_secret would be re-generated and silently
// invalidate every session.
func TestSQLStoreWrongMasterKeyFailsClosed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	c1, _ := NewCrypter(testSecret(t))
	if err := NewSQLStore(db, c1).Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c2, _ := NewCrypter(testSecret(t)) // different master key
	if _, ok, err := NewSQLStore(db, c2).Get(ctx, "k"); err == nil {
		t.Fatalf("Get with wrong master key = ok:%v err:nil; want an error (fail closed)", ok)
	}
}

func TestStoreDisabled(t *testing.T) {
	c, _ := NewCrypter("") // disabled
	ctx := context.Background()
	for name, s := range map[string]Store{
		"mem": NewMemStore(c),
		"sql": NewSQLStore(testDB(t), c),
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.Set(ctx, "k", "v"); !errors.Is(err, ErrConfigDisabled) {
				t.Fatalf("Set: %v", err)
			}
			if _, _, err := s.Get(ctx, "k"); !errors.Is(err, ErrConfigDisabled) {
				t.Fatalf("Get: %v", err)
			}
			if err := s.Delete(ctx, "k"); !errors.Is(err, ErrConfigDisabled) {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := s.Keys(ctx); !errors.Is(err, ErrConfigDisabled) {
				t.Fatalf("Keys: %v", err)
			}
		})
	}
}
