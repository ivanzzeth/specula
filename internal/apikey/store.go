package apikey

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Compile-time assertions: both stores satisfy the Store interface.
var (
	_ Store = (*SQLStore)(nil)
	_ Store = (*MemStore)(nil)
)

// ---- SQLStore ----------------------------------------------------------------

// SQLStore is the DB-authoritative Store backed by the api_keys table (goose
// migration 0002_multitenant). Multiple replicas sharing the same database
// see each other's keys immediately. Only the SHA-256 hash of each key is
// persisted; the plaintext is returned exactly once (at creation) and then
// gone from memory.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore constructs a SQLStore over an already-migrated database handle.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Create issues a key for an org (no issuing user). Returns the stable id and
// the plaintext key — the plaintext is visible only at this call.
func (s *SQLStore) Create(orgID, label string) (string, string, error) {
	return s.sqlInsert(orgID, "", label, time.Time{})
}

// CreateOwned issues a key for an org and records the issuing userID (the acl
// subject string, e.g. org.UserSubjectID(user.ID)). orgID empty → DefaultOrgID.
func (s *SQLStore) CreateOwned(orgID, userID, label string) (string, string, error) {
	return s.sqlInsert(orgID, userID, label, time.Time{})
}

// sqlInsert is the single insert path (Create / CreateOwned share it).
func (s *SQLStore) sqlInsert(orgID, userID, label string, expiresAt time.Time) (string, string, error) {
	if orgID == "" {
		orgID = DefaultOrgID
	}
	id := newID()
	rawKey := newRawKey()
	now := time.Now().UTC()
	pfx := prefixOf(rawKey)
	h := hashKey(rawKey)

	var exp any // NULL = never expires
	if !expiresAt.IsZero() {
		exp = expiresAt.UTC().Format(time.RFC3339Nano)
	}

	const q = `INSERT INTO api_keys(key_hash,id,label,prefix,org_id,user_id,created_at,expires_at,revoked)
	           VALUES(?,?,?,?,?,?,?,?,0)`
	if _, err := s.db.Exec(q, h, id, label, pfx, orgID, userID,
		now.Format(time.RFC3339Nano), exp); err != nil {
		return "", "", fmt.Errorf("apikey: insert: %w", err)
	}
	return id, rawKey, nil
}

// LookupSubject verifies a presented plaintext key and returns its (orgID,
// synthetic subject "apikey:<id>"). ok=false for unknown/revoked/expired keys
// (uniform failure, no distinction leaked). On success, last_used_at is
// touched.
func (s *SQLStore) LookupSubject(token string) (string, string, bool) {
	var (
		id        sql.NullString
		orgID     sql.NullString
		revoked   sql.NullInt64
		expiresAt sql.NullString
	)
	h := hashKey(token)
	err := s.db.QueryRow(
		`SELECT id, org_id, revoked, expires_at FROM api_keys WHERE key_hash=?`, h,
	).Scan(&id, &orgID, &revoked, &expiresAt)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		log.Printf("apikey: lookup: %v", err)
		return "", "", false
	}
	if revoked.Valid && revoked.Int64 != 0 {
		return "", "", false
	}
	if expiresAt.Valid && expiresAt.String != "" {
		if exp, perr := time.Parse(time.RFC3339Nano, expiresAt.String); perr == nil && !time.Now().UTC().Before(exp) {
			return "", "", false // expired: uniform silent failure
		}
	}
	if _, err := s.db.Exec(
		`UPDATE api_keys SET last_used_at=? WHERE key_hash=?`,
		time.Now().UTC().Format(time.RFC3339Nano), h,
	); err != nil {
		log.Printf("apikey: touch last_used_at: %v", err)
	}
	return orgID.String, SubjectID(id.String), true
}

// List returns an org's keys newest→oldest (including revoked).
func (s *SQLStore) List(orgID string) ([]KeyInfo, error) {
	const q = `SELECT id,org_id,user_id,label,prefix,created_at,last_used_at,revoked,expires_at
	           FROM api_keys WHERE org_id=? ORDER BY created_at DESC, id DESC`
	rows, err := s.db.Query(q, orgID)
	if err != nil {
		return nil, fmt.Errorf("apikey: list: %w", err)
	}
	defer rows.Close()
	out := make([]KeyInfo, 0)
	for rows.Next() {
		if info, ok := scanKeyInfo(rows); ok {
			out = append(out, info)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apikey: list iterate: %w", err)
	}
	return out, nil
}

// Get returns a single key by (orgID, id); cross-org lookups miss (ok=false).
func (s *SQLStore) Get(orgID, id string) (KeyInfo, bool) {
	const q = `SELECT id,org_id,user_id,label,prefix,created_at,last_used_at,revoked,expires_at
	           FROM api_keys WHERE id=? AND org_id=?`
	return scanKeyInfo(s.db.QueryRow(q, id, orgID))
}

// Revoke soft-deletes a key by (orgID, id). Returns (true, nil) when a
// matching, not-already-revoked key was found and revoked. Returns
// (false, nil) when the key is missing or already revoked; returns
// (false, err) on a database error.
func (s *SQLStore) Revoke(orgID, id string) (bool, error) {
	const q = `UPDATE api_keys SET revoked=1 WHERE id=? AND org_id=? AND (revoked IS NULL OR revoked=0)`
	res, err := s.db.Exec(q, id, orgID)
	if err != nil {
		return false, fmt.Errorf("apikey: revoke: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// keyScanner abstracts *sql.Row and *sql.Rows for the shared scanKeyInfo.
type keyScanner interface{ Scan(...any) error }

// scanKeyInfo populates a KeyInfo from the 9-column api_keys projection:
// id, org_id, user_id, label, prefix, created_at, last_used_at, revoked, expires_at.
func scanKeyInfo(sc keyScanner) (KeyInfo, bool) {
	var (
		id, orgID, userID     sql.NullString
		label, prefix         sql.NullString
		createdAt, lastUsedAt sql.NullString
		revoked               sql.NullInt64
		expiresAt             sql.NullString
	)
	if err := sc.Scan(&id, &orgID, &userID, &label, &prefix,
		&createdAt, &lastUsedAt, &revoked, &expiresAt); err != nil {
		return KeyInfo{}, false
	}
	info := KeyInfo{
		ID:      id.String,
		OrgID:   orgID.String,
		UserID:  userID.String,
		Label:   label.String,
		Prefix:  prefix.String,
		Revoked: revoked.Valid && revoked.Int64 != 0,
	}
	if createdAt.Valid && createdAt.String != "" {
		info.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt.String)
	}
	if lastUsedAt.Valid && lastUsedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, lastUsedAt.String); err == nil {
			info.LastUsedAt = &t
		}
	}
	if expiresAt.Valid && expiresAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, expiresAt.String); err == nil {
			info.ExpiresAt = &t
		}
	}
	return info, true
}

// ---- MemStore ----------------------------------------------------------------

// MemStore is the in-memory Store for dev/tests. dyn is keyed by
// hashKey(rawKey) so the plaintext is never retained after creation.
// All methods are safe for concurrent use.
type MemStore struct {
	mu  sync.RWMutex
	dyn map[string]*KeyInfo // key_hash → KeyInfo (plaintext key is never stored)
}

// NewMemStore constructs an empty in-memory Store.
func NewMemStore() *MemStore { return &MemStore{dyn: map[string]*KeyInfo{}} }

// Create issues a key for an org (no issuing user). Returns id and plaintext
// key — the plaintext is not retained after this call returns.
func (m *MemStore) Create(orgID, label string) (string, string, error) {
	return m.memMint(orgID, "", label, time.Time{})
}

// CreateOwned issues a key for an org and records the issuing userID. orgID
// empty → DefaultOrgID.
func (m *MemStore) CreateOwned(orgID, userID, label string) (string, string, error) {
	return m.memMint(orgID, userID, label, time.Time{})
}

// memMint is the single create path for MemStore.
func (m *MemStore) memMint(orgID, userID, label string, expiresAt time.Time) (string, string, error) {
	if orgID == "" {
		orgID = DefaultOrgID
	}
	id := newID()
	rawKey := newRawKey()
	now := time.Now().UTC()
	info := KeyInfo{
		ID:        id,
		OrgID:     orgID,
		UserID:    userID,
		Label:     label,
		Prefix:    prefixOf(rawKey),
		CreatedAt: now,
	}
	if !expiresAt.IsZero() {
		exp := expiresAt.UTC()
		info.ExpiresAt = &exp
	}
	m.mu.Lock()
	m.dyn[hashKey(rawKey)] = &info // store hash as key, not the plaintext
	m.mu.Unlock()
	return id, rawKey, nil
}

// LookupSubject verifies a presented plaintext key (hashes it, looks up the
// hash) and returns (orgID, "apikey:<id>"). ok=false for unknown/revoked/
// expired. On success, LastUsedAt is updated.
func (m *MemStore) LookupSubject(token string) (string, string, bool) {
	h := hashKey(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.dyn[h]
	if !ok || e.Revoked {
		return "", "", false
	}
	now := time.Now().UTC()
	if e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
		return "", "", false // expired: uniform silent failure
	}
	e.LastUsedAt = &now
	return e.OrgID, SubjectID(e.ID), true
}

// List returns all keys for orgID newest→oldest (including revoked).
func (m *MemStore) List(orgID string) ([]KeyInfo, error) {
	m.mu.RLock()
	out := make([]KeyInfo, 0)
	for _, e := range m.dyn {
		if e.OrgID != orgID {
			continue
		}
		out = append(out, copyKeyInfo(e))
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Get returns a single key by (orgID, id); cross-org lookups miss.
func (m *MemStore) Get(orgID, id string) (KeyInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.dyn {
		if e.ID == id && e.OrgID == orgID {
			return copyKeyInfo(e), true
		}
	}
	return KeyInfo{}, false
}

// Revoke soft-deletes a key by (orgID, id). Returns (true, nil) on first
// revocation; (false, nil) for unknown or already-revoked keys.
func (m *MemStore) Revoke(orgID, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.dyn {
		if e.ID == id && e.OrgID == orgID {
			if e.Revoked {
				return false, nil
			}
			e.Revoked = true
			return true, nil
		}
	}
	return false, nil
}

// copyKeyInfo returns a value copy of src with time-pointer fields deep-copied
// so callers cannot mutate stored state through the returned struct.
func copyKeyInfo(src *KeyInfo) KeyInfo {
	cp := *src
	if src.LastUsedAt != nil {
		t := *src.LastUsedAt
		cp.LastUsedAt = &t
	}
	if src.ExpiresAt != nil {
		t := *src.ExpiresAt
		cp.ExpiresAt = &t
	}
	return cp
}
