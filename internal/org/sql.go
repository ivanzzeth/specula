package org

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/dbx"
)

// SQLStore is the persistent Store backed by the orgs / org_members /
// org_invitations tables (goose migration 0002_multitenant). Callers construct
// it with the *sql.DB exposed by the sqlite or postgres stores.
//
// The ported SQL is written with "?" positional placeholders. On SQLite the
// driver consumes them directly; on PostgreSQL (pgx stdlib) they must be
// rewritten to "$N". The dialect field selects the placeholder style and every
// query is passed through rb() before execution, so one query string is correct
// on both backends. Construct with NewSQLStore (SQLite) or NewSQLStorePostgres.
type SQLStore struct {
	db      *sql.DB
	dialect dbx.Dialect
}

// NewSQLStore constructs a SQLStore over an already-migrated SQLite database
// handle ("?" placeholders passed through verbatim).
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db, dialect: dbx.SQLite} }

// NewSQLStorePostgres constructs a SQLStore over an already-migrated PostgreSQL
// database handle, rebinding "?" placeholders to "$N" for the pgx stdlib driver.
func NewSQLStorePostgres(db *sql.DB) *SQLStore {
	return &SQLStore{db: db, dialect: dbx.Postgres}
}

// rb rewrites a query's "?" placeholders for the store's dialect (no-op on
// SQLite). Call it on every query string before handing it to database/sql.
func (s *SQLStore) rb(query string) string { return dbx.Rebind(s.dialect, query) }

// scanner abstracts *sql.Row and *sql.Rows so single-row and multi-row
// scanning can share one scan function.
type scanner interface {
	Scan(dest ...any) error
}

// nowTS returns the current UTC time formatted as RFC3339Nano (TEXT storage).
func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// parseTS parses a nullable RFC3339/RFC3339Nano TEXT timestamp column.
func parseTS(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s.String); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s.String); err == nil {
		return t
	}
	return time.Time{}
}

// prefixCols prefixes each comma-separated column name with the table alias
// (e.g. prefixCols("o", "id, name") → "o.id, o.name").
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// ── orgs ──────────────────────────────────────────────────────────────────

const orgCols = `id, name, slug, status, created_by, created_at`

// CreateOrg inserts an organization. Status defaults to "active"; CreatedAt
// defaults to now; ID is generated if empty.
func (s *SQLStore) CreateOrg(ctx context.Context, o *Org) error {
	if o.Status == "" {
		o.Status = StatusActive
	}
	if o.ID == "" {
		o.ID = newID("org_")
	}
	created := o.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		s.rb(`INSERT INTO orgs (`+orgCols+`) VALUES (?, ?, ?, ?, ?, ?)`),
		o.ID, o.Name, o.Slug, o.Status, o.CreatedBy,
		created.UTC().Format(time.RFC3339Nano))
	return err
}

// GetOrg returns the organization by ID, or ErrNotFound.
func (s *SQLStore) GetOrg(ctx context.Context, id string) (*Org, error) {
	return scanOrg(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+orgCols+` FROM orgs WHERE id = ?`), id))
}

// GetOrgBySlug returns the organization by slug, or ErrNotFound.
func (s *SQLStore) GetOrgBySlug(ctx context.Context, slug string) (*Org, error) {
	return scanOrg(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+orgCols+` FROM orgs WHERE slug = ?`), slug))
}

// ListOrgs returns all organizations newest-first.
func (s *SQLStore) ListOrgs(ctx context.Context) ([]*Org, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+orgCols+` FROM orgs ORDER BY created_at DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateOrg updates the organization's display name (slug / status / created
// fields each have dedicated paths).
func (s *SQLStore) UpdateOrg(ctx context.Context, o *Org) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`UPDATE orgs SET name = ? WHERE id = ?`), o.Name, o.ID)
	return err
}

// DeleteOrg removes the organization and its identity-domain rows
// (org_invitations + org_members + orgs) atomically.
func (s *SQLStore) DeleteOrg(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM org_invitations WHERE org_id = ?`), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM org_members WHERE org_id = ?`), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM orgs WHERE id = ?`), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListOrgsForEmail returns every org the email address is a member of
// (newest→oldest), with Org.Role set to the member's role in that org.
func (s *SQLStore) ListOrgsForEmail(ctx context.Context, email string) ([]*Org, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+prefixCols("o", orgCols)+`, m.role
		   FROM orgs o JOIN org_members m ON m.org_id = o.id
		  WHERE m.email = ? ORDER BY o.created_at DESC`),
		strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Org
	for rows.Next() {
		var o Org
		var created, role sql.NullString
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.Status,
			&o.CreatedBy, &created, &role); err != nil {
			return nil, err
		}
		o.CreatedAt = parseTS(created)
		o.Role = role.String
		out = append(out, &o)
	}
	return out, rows.Err()
}

// SetOrgStatus sets the organization's lifecycle status (active|frozen).
func (s *SQLStore) SetOrgStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`UPDATE orgs SET status = ? WHERE id = ?`), status, id)
	return err
}

// CountOrgAdmins returns the number of admin-role members in an org.
// Includes the legacy "org_admin" alias for history rows.
func (s *SQLStore) CountOrgAdmins(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		s.rb(`SELECT COUNT(1) FROM org_members WHERE org_id = ? AND role IN (?, 'org_admin')`),
		orgID, RoleAdmin).Scan(&n)
	return n, err
}

// CountOrgOwners returns the number of owner-role members in an org.
// Used by the last-owner guard.
func (s *SQLStore) CountOrgOwners(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		s.rb(`SELECT COUNT(1) FROM org_members WHERE org_id = ? AND role = ?`),
		orgID, RoleOwner).Scan(&n)
	return n, err
}

// CountOrgs returns the total org count. Used by the bootstrap to decide
// whether the default org must be seeded (CountOrgs()==0 → seed).
func (s *SQLStore) CountOrgs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rb(`SELECT COUNT(1) FROM orgs`)).Scan(&n)
	return n, err
}

// CountOrgsByCreator returns how many orgs the given user self-created (by
// created_by). This is the query behind the org.max_per_user quota.
//
// Counting created_by rather than membership is deliberate: the limit is on
// how many orgs you may CREATE, not how many you may belong to. Being invited
// into ten orgs must never exhaust your own allowance.
func (s *SQLStore) CountOrgsByCreator(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		s.rb(`SELECT COUNT(1) FROM orgs WHERE created_by = ?`), userID).Scan(&n)
	return n, err
}

func scanOrg(row scanner) (*Org, error) {
	var o Org
	var created sql.NullString
	if err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.Status, &o.CreatedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	o.CreatedAt = parseTS(created)
	return &o, nil
}

// ── org_members ───────────────────────────────────────────────────────────

const memberCols = `id, org_id, email, role, invited_by, created_at`

// AddOrgMember upserts a (org_id, email) membership: if the row already exists
// it updates the role; otherwise it inserts. Email is normalised to lower-case.
// Role defaults to editor; ID is generated if empty.
func (s *SQLStore) AddOrgMember(ctx context.Context, m *Member) error {
	m.Email = strings.ToLower(strings.TrimSpace(m.Email))
	if m.Role == "" {
		m.Role = RoleEditor
	}
	// Check-then-insert: dialect-portable alternative to ON CONFLICT DO UPDATE.
	if existing, err := s.GetOrgMember(ctx, m.OrgID, m.Email); err == nil {
		_, e := s.db.ExecContext(ctx,
			s.rb(`UPDATE org_members SET role = ? WHERE id = ?`), m.Role, existing.ID)
		return e
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if m.ID == "" {
		m.ID = newID("mem_")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		s.rb(`INSERT INTO org_members (`+memberCols+`) VALUES (?, ?, ?, ?, ?, ?)`),
		m.ID, m.OrgID, m.Email, m.Role, m.InvitedBy,
		created.UTC().Format(time.RFC3339Nano))
	return err
}

// GetOrgMember returns the membership record for (orgID, email), or ErrNotFound.
func (s *SQLStore) GetOrgMember(ctx context.Context, orgID, email string) (*Member, error) {
	return scanMember(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+memberCols+` FROM org_members WHERE org_id = ? AND email = ?`),
		orgID, strings.ToLower(strings.TrimSpace(email))))
}

// ListOrgMembers returns all members of an org newest-first.
func (s *SQLStore) ListOrgMembers(ctx context.Context, orgID string) ([]*Member, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+memberCols+` FROM org_members WHERE org_id = ? ORDER BY created_at DESC`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveOrgMember deletes the membership for (orgID, email). No-op if absent.
func (s *SQLStore) RemoveOrgMember(ctx context.Context, orgID, email string) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`DELETE FROM org_members WHERE org_id = ? AND email = ?`),
		orgID, strings.ToLower(strings.TrimSpace(email)))
	return err
}

func scanMember(row scanner) (*Member, error) {
	var m Member
	var created, invitedBy sql.NullString
	if err := row.Scan(&m.ID, &m.OrgID, &m.Email, &m.Role, &invitedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.InvitedBy = invitedBy.String
	m.CreatedAt = parseTS(created)
	return &m, nil
}

// ── org_invitations ───────────────────────────────────────────────────────

const invitationCols = `id, org_id, email, role, invited_by, token, status, expires_at, created_at`

// CreateInvitation inserts a pending invitation. Email is normalised; Role
// defaults to viewer; Status defaults to pending; ID is generated if empty.
func (s *SQLStore) CreateInvitation(ctx context.Context, inv *Invitation) error {
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	if inv.Role == "" {
		inv.Role = RoleViewer
	}
	if inv.Status == "" {
		inv.Status = InviteStatusPending
	}
	if inv.ID == "" {
		inv.ID = newID("inv_")
	}
	created := inv.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	expires := ""
	if !inv.ExpiresAt.IsZero() {
		expires = inv.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx,
		s.rb(`INSERT INTO org_invitations (`+invitationCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		inv.ID, inv.OrgID, inv.Email, inv.Role, inv.InvitedBy,
		inv.Token, inv.Status, expires, created.UTC().Format(time.RFC3339Nano))
	return err
}

// GetInvitationByToken returns the invitation with the given token, or ErrNotFound.
func (s *SQLStore) GetInvitationByToken(ctx context.Context, token string) (*Invitation, error) {
	return scanInvitation(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+invitationCols+` FROM org_invitations WHERE token = ?`), token))
}

// ListInvitations returns all invitations for an org newest-first.
func (s *SQLStore) ListInvitations(ctx context.Context, orgID string) ([]*Invitation, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+invitationCols+` FROM org_invitations WHERE org_id = ? ORDER BY created_at DESC`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// SetInvitationStatus transitions an invitation to a new status
// (pending→accepted/declined/expired).
func (s *SQLStore) SetInvitationStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`UPDATE org_invitations SET status = ? WHERE id = ?`), status, id)
	return err
}

func scanInvitation(row scanner) (*Invitation, error) {
	var inv Invitation
	var expires, created sql.NullString
	if err := row.Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Token, &inv.Status, &expires, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	inv.ExpiresAt = parseTS(expires)
	inv.CreatedAt = parseTS(created)
	return &inv, nil
}
