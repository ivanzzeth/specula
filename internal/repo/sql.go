package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/dbx"
)

// SQLStore implements RepoStore + TagStore over the repos / repo_tags tables
// (migrations 0002 + 0003). Control-plane timestamps are stored as RFC3339 TEXT
// so the same SQL runs verbatim on SQLite and PostgreSQL; the dialect field
// rebinds "?" placeholders to "$N" on PostgreSQL. Construct with NewSQLStore
// (SQLite) or NewSQLStorePostgres.
type SQLStore struct {
	db      *sql.DB
	dialect dbx.Dialect
}

// NewSQLStore constructs the SQLite repo/tag store over an already-migrated
// database handle. The caller must have applied migration 0003 (repo_tags).
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db, dialect: dbx.SQLite} }

// NewSQLStorePostgres constructs the PostgreSQL repo/tag store, rebinding "?"
// placeholders to "$N" for the pgx stdlib driver.
func NewSQLStorePostgres(db *sql.DB) *SQLStore {
	return &SQLStore{db: db, dialect: dbx.Postgres}
}

// rb rewrites a query's "?" placeholders for the store's dialect (no-op on SQLite).
func (s *SQLStore) rb(query string) string { return dbx.Rebind(s.dialect, query) }

// Compile-time assertions: SQLStore satisfies both persistence contracts.
var (
	_ RepoStore = (*SQLStore)(nil)
	_ TagStore  = (*SQLStore)(nil)
)

// scanner abstracts *sql.Row and *sql.Rows for shared row scanning.
type scanner interface{ Scan(dest ...any) error }

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

// ── repos ───────────────────────────────────────────────────────────────────

const repoCols = `id, org_id, name, visibility, owner_user_id, created_at`

// CreateRepo inserts a hosted repo. Visibility defaults to private; the
// (org_id, name) unique index rejects duplicates.
func (s *SQLStore) CreateRepo(ctx context.Context, orgID, name, visibility, ownerUserID string) (*Repo, error) {
	r := &Repo{
		ID:          newID("repo_"),
		OrgID:       orgID,
		Name:        strings.TrimSpace(name),
		Visibility:  NormalizeVisibility(visibility),
		OwnerUserID: ownerUserID,
		CreatedAt:   time.Now().UTC(),
	}
	_, err := s.db.ExecContext(ctx,
		s.rb(`INSERT INTO repos (`+repoCols+`) VALUES (?, ?, ?, ?, ?, ?)`),
		r.ID, r.OrgID, r.Name, r.Visibility, r.OwnerUserID,
		r.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// GetRepo returns the repo for (orgID, name), or ErrNotFound.
func (s *SQLStore) GetRepo(ctx context.Context, orgID, name string) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+repoCols+` FROM repos WHERE org_id = ? AND name = ?`),
		orgID, strings.TrimSpace(name)))
}

// ListRepos returns all repos in an org, newest-first.
func (s *SQLStore) ListRepos(ctx context.Context, orgID string) ([]*Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+repoCols+` FROM repos WHERE org_id = ? ORDER BY created_at DESC`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetVisibility updates a repo's visibility (normalised to private|public).
func (s *SQLStore) SetVisibility(ctx context.Context, orgID, name, visibility string) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`UPDATE repos SET visibility = ? WHERE org_id = ? AND name = ?`),
		NormalizeVisibility(visibility), orgID, strings.TrimSpace(name))
	return err
}

// DeleteRepo removes the repo and its tag rows atomically.
func (s *SQLStore) DeleteRepo(ctx context.Context, orgID, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	err = tx.QueryRowContext(ctx,
		s.rb(`SELECT id FROM repos WHERE org_id = ? AND name = ?`),
		orgID, strings.TrimSpace(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM repo_tags WHERE repo_id = ?`), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM repos WHERE id = ?`), id); err != nil {
		return err
	}
	return tx.Commit()
}

func scanRepo(row scanner) (*Repo, error) {
	var r Repo
	var created sql.NullString
	if err := row.Scan(&r.ID, &r.OrgID, &r.Name, &r.Visibility, &r.OwnerUserID, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// ── repo_tags ───────────────────────────────────────────────────────────────

const tagCols = `repo_id, tag, digest, updated_at`

// PutTag upserts the tag→digest pointer (ON CONFLICT overwrites digest +
// updated_at; the (repo_id, tag) primary key backs the conflict target, valid
// on both SQLite and PostgreSQL).
func (s *SQLStore) PutTag(ctx context.Context, repoID, tag, digest string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		s.rb(`INSERT INTO repo_tags (`+tagCols+`) VALUES (?, ?, ?, ?)
		 ON CONFLICT(repo_id, tag) DO UPDATE SET
		     digest     = excluded.digest,
		     updated_at = excluded.updated_at`),
		repoID, tag, digest, now)
	return err
}

// GetTag returns the pointer for (repoID, tag), or ErrNotFound.
func (s *SQLStore) GetTag(ctx context.Context, repoID, tag string) (*Tag, error) {
	return scanTag(s.db.QueryRowContext(ctx,
		s.rb(`SELECT `+tagCols+` FROM repo_tags WHERE repo_id = ? AND tag = ?`),
		repoID, tag))
}

// ListTags returns all tags for a repo, tag-name ascending.
func (s *SQLStore) ListTags(ctx context.Context, repoID string) ([]*Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rb(`SELECT `+tagCols+` FROM repo_tags WHERE repo_id = ? ORDER BY tag ASC`), repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tag
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTag removes a tag pointer (no-op if absent).
func (s *SQLStore) DeleteTag(ctx context.Context, repoID, tag string) error {
	_, err := s.db.ExecContext(ctx,
		s.rb(`DELETE FROM repo_tags WHERE repo_id = ? AND tag = ?`), repoID, tag)
	return err
}

func scanTag(row scanner) (*Tag, error) {
	var t Tag
	var updated sql.NullString
	if err := row.Scan(&t.RepoID, &t.Tag, &t.Digest, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.UpdatedAt = parseTS(updated)
	return &t, nil
}
