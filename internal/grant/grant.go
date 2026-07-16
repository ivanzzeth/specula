// Package grant is the persistence layer for centralized cross-org / per-member
// resource sharing. Adapted from ai-sandbox internal/controlplane/grant.
//
// Any resource type records the subjects it is shared with in a single
// resource_grants table, keyed by (ResourceType, ResourceID): SubjectType is
// org|user, SubjectID is the orgID / user subject string, and Access is
// read|write. This package only stores grant records — it never decides "can
// access". Authorization remains solely in acl.CanAccessGranted; the API layer
// feeds GrantedOrgs into it (a granted org is treated as same-org as the
// resource).
package grant

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"
)

// errNotImplemented is kept for compile-time safety; removed from live paths once
// all SQL methods are implemented.
var errNotImplemented = errors.New("grant: SQLStore not implemented")

// Ensure errNotImplemented is used (avoids unused-variable error if all methods
// are implemented; kept here as a guard for future stubs).
var _ = errNotImplemented

// Subject types (resource_grants.subject_type).
const (
	SubjectOrg  = "org"  // granted to an org (its members get org-level read/write per the resource)
	SubjectUser = "user" // granted to a single user (per-member share)
)

// Grant access levels (resource_grants.access).
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// Grant is one resource_grants row.
type Grant struct {
	ResourceType string // e.g. "repo"
	ResourceID   string
	SubjectType  string // org | user
	SubjectID    string // orgID | user subject string
	Access       string // read | write
	GrantedBy    string // granter acl subject string (audit; may be empty)
	CreatedAt    time.Time
}

// Store is the grant record store (SQLStore persistent / MemStore in-memory).
type Store interface {
	// Grants returns all grant records for a resource.
	Grants(resourceType, resourceID string) ([]Grant, error)
	// Upsert idempotently writes a grant (same (type,id,subject_type,subject_id)
	// overwrites access/granted_by).
	Upsert(g Grant) error
	// Delete removes a grant (no-op if absent).
	Delete(resourceType, resourceID, subjectType, subjectID string) error
	// GrantedOrgs returns the org subject ids a resource is shared with, to feed
	// acl.CanAccessGranted.
	GrantedOrgs(resourceType, resourceID string) []string
}

// normAccess maps unknown/empty access to the most conservative read.
func normAccess(a string) string {
	if a == AccessWrite {
		return AccessWrite
	}
	return AccessRead
}

// Compile-time assertions.
var (
	_ Store = (*SQLStore)(nil)
	_ Store = (*MemStore)(nil)
)

// ---- SQLStore: resource_grants-backed implementation -------------------------

// SQLStore is the resource_grants-backed Store. Uses database/sql with ?
// placeholders (SQLite-compatible; callers targeting PostgreSQL via
// database/sql/stdlib should wrap with a rebind step). Ported from
// ai-sandbox internal/controlplane/grant, adapted to Specula's Upsert/Delete
// naming and standalone *sql.DB injection.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore constructs the SQL grant store backed by db (SQLite or Postgres
// via database/sql). The caller is responsible for running the 0002_multitenant
// migration before first use so that resource_grants exists.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// Grants returns all grant records for the given resource, ordered by
// (subject_type, subject_id). Returns an empty slice (not nil) when none exist.
func (s *SQLStore) Grants(resourceType, resourceID string) ([]Grant, error) {
	rows, err := s.db.Query(
		`SELECT resource_type, resource_id, subject_type, subject_id,
		        access, granted_by, created_at
		   FROM resource_grants
		  WHERE resource_type = ? AND resource_id = ?
		  ORDER BY subject_type, subject_id`,
		resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var g Grant
		var created string
		if err := rows.Scan(
			&g.ResourceType, &g.ResourceID,
			&g.SubjectType, &g.SubjectID,
			&g.Access, &g.GrantedBy, &created,
		); err != nil {
			return out, err
		}
		g.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out == nil {
		out = []Grant{}
	}
	return out, nil
}

// Upsert idempotently writes a grant record. If a row with the same
// (resource_type, resource_id, subject_type, subject_id) already exists, its
// access, granted_by, and created_at are overwritten.
func (s *SQLStore) Upsert(g Grant) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	g.Access = normAccess(g.Access)
	_, err := s.db.Exec(
		`INSERT INTO resource_grants
		        (resource_type, resource_id, subject_type, subject_id,
		         access, granted_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(resource_type, resource_id, subject_type, subject_id)
		 DO UPDATE SET
		     access     = excluded.access,
		     granted_by = excluded.granted_by,
		     created_at = excluded.created_at`,
		g.ResourceType, g.ResourceID, g.SubjectType, g.SubjectID,
		g.Access, g.GrantedBy,
		g.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Delete removes the grant identified by the four-column key. No-op if absent.
func (s *SQLStore) Delete(resourceType, resourceID, subjectType, subjectID string) error {
	_, err := s.db.Exec(
		`DELETE FROM resource_grants
		  WHERE resource_type = ? AND resource_id = ?
		    AND subject_type  = ? AND subject_id  = ?`,
		resourceType, resourceID, subjectType, subjectID,
	)
	return err
}

// GrantedOrgs returns the org IDs that have an explicit grant for the given
// resource (subject_type = "org"). Returns nil (not an error) on query failure
// so callers can always safely pass the result into acl.CanAccessGranted.
func (s *SQLStore) GrantedOrgs(resourceType, resourceID string) []string {
	rows, err := s.db.Query(
		`SELECT subject_id FROM resource_grants
		  WHERE resource_type = ? AND resource_id = ? AND subject_type = ?`,
		resourceType, resourceID, SubjectOrg,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return out
		}
		out = append(out, org)
	}
	return out
}

// PurgeSubject deletes every grant for a subject (org or user) — used for
// cascade cleanup when an org or user is deleted.
func (s *SQLStore) PurgeSubject(ctx context.Context, subjectType, subjectID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM resource_grants WHERE subject_type = ? AND subject_id = ?`,
		subjectType, subjectID,
	)
	return err
}

// ---- MemStore: functional in-memory implementation ----------------------------

type memKey struct{ resourceType, resourceID, subjectType, subjectID string }

// MemStore is a concurrency-safe in-memory Store, semantics aligned with the
// SQL implementation. Ported as-is (self-contained, no DB).
type MemStore struct {
	mu sync.Mutex
	m  map[memKey]Grant
}

// NewMemStore constructs an empty in-memory grant store.
func NewMemStore() *MemStore { return &MemStore{m: map[memKey]Grant{}} }

func (s *MemStore) Grants(resourceType, resourceID string) ([]Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Grant
	for k, g := range s.m {
		if k.resourceType == resourceType && k.resourceID == resourceID {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubjectType != out[j].SubjectType {
			return out[i].SubjectType < out[j].SubjectType
		}
		return out[i].SubjectID < out[j].SubjectID
	})
	return out, nil
}

func (s *MemStore) Upsert(g Grant) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	g.Access = normAccess(g.Access)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[memKey{g.ResourceType, g.ResourceID, g.SubjectType, g.SubjectID}] = g
	return nil
}

func (s *MemStore) Delete(resourceType, resourceID, subjectType, subjectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, memKey{resourceType, resourceID, subjectType, subjectID})
	return nil
}

func (s *MemStore) GrantedOrgs(resourceType, resourceID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.m {
		if k.resourceType == resourceType && k.resourceID == resourceID && k.subjectType == SubjectOrg {
			out = append(out, k.subjectID)
		}
	}
	return out
}
