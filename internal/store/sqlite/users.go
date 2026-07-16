// Package sqlite — users.go: the control-plane UserStore implementation backed
// by the same SQLite database as the MetadataStore. It reuses the `users` table
// created by the embedded goose migrations so a single SQLiteStore value
// satisfies both meta.MetadataStore and auth.UserStore.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/auth"
)

// Compile-time assertion: SQLiteStore satisfies the control-plane UserStore.
var _ auth.UserStore = (*SQLiteStore)(nil)

// CountUsers returns the total number of user rows. Used by the first-user-admin
// bootstrap (CountUsers()==0 → new account becomes admin).
func (s *SQLiteStore) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count users: %w", err)
	}
	return n, nil
}

// GetUserByEmail returns the user with the given (already-normalised) email, or
// auth.ErrUserNotFound if absent.
func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (*auth.User, error) {
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users WHERE email = ?`
	return scanUser(s.db.QueryRowContext(ctx, q, email))
}

// GetUserByID returns the user with the given ID, or auth.ErrUserNotFound.
func (s *SQLiteStore) GetUserByID(ctx context.Context, id int64) (*auth.User, error) {
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users WHERE id = ?`
	return scanUser(s.db.QueryRowContext(ctx, q, id))
}

// CreateUser inserts a new user and returns the stored copy with its assigned
// ID and created_at populated. Returns auth.ErrEmailTaken on a UNIQUE violation.
func (s *SQLiteStore) CreateUser(ctx context.Context, u auth.User) (*auth.User, error) {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if u.SystemRole == "" {
		u.SystemRole = "user"
	}
	const q = `
		INSERT INTO users (email, name, password_hash, system_role, token_gen, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	res, err := s.db.ExecContext(ctx, q,
		u.Email, u.Name, u.PasswordHash, u.SystemRole, u.TokenGen, u.CreatedAt.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, auth.ErrEmailTaken
		}
		return nil, fmt.Errorf("sqlite: create user %q: %w", u.Email, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("sqlite: create user last id: %w", err)
	}
	u.ID = id
	return &u, nil
}

// BumpTokenGen atomically increments the user's token_gen and returns the new
// value, invalidating every previously issued session JWT.
func (s *SQLiteStore) BumpTokenGen(ctx context.Context, id int64) (int64, error) {
	const q = `UPDATE users SET token_gen = token_gen + 1 WHERE id = ? RETURNING token_gen`
	var gen int64
	err := s.db.QueryRowContext(ctx, q, id).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, auth.ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: bump token gen (id=%d): %w", id, err)
	}
	return gen, nil
}

// ListUsers returns a page of users ordered by ID ascending plus the total row
// count. limit <= 0 returns all rows from offset.
func (s *SQLiteStore) ListUsers(ctx context.Context, limit, offset int) ([]auth.User, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sqlite: list users count: %w", err)
	}
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 means "no limit"
	}
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users ORDER BY id ASC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: list users: %w", err)
	}
	defer rows.Close()

	var out []auth.User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sqlite: iterate users: %w", err)
	}
	return out, total, nil
}

// UpdateUserRole sets the user's system_role. Returns auth.ErrUserNotFound
// when no row matches id.
func (s *SQLiteStore) UpdateUserRole(ctx context.Context, id int64, role string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET system_role = ? WHERE id = ?`, role, id)
	if err != nil {
		return fmt.Errorf("sqlite: update user role (id=%d): %w", id, err)
	}
	return affectedOrNotFound(res)
}

// UpdateUserFields updates zero or more mutable fields on the user identified
// by id. A nil pointer means "leave this field unchanged". passwordHash, when
// non-nil, must already be a bcrypt hash. Returns auth.ErrUserNotFound when no
// row matches id. Returns nil (no-op) when both pointers are nil.
func (s *SQLiteStore) UpdateUserFields(ctx context.Context, id int64, name, passwordHash *string) error {
	if name == nil && passwordHash == nil {
		// Verify user exists and return ErrUserNotFound if absent.
		_, err := s.GetUserByID(ctx, id)
		return err
	}
	var setClauses []string
	var args []any
	if name != nil {
		setClauses = append(setClauses, "name = ?")
		args = append(args, *name)
	}
	if passwordHash != nil {
		setClauses = append(setClauses, "password_hash = ?")
		args = append(args, *passwordHash)
	}
	args = append(args, id)
	q := "UPDATE users SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("sqlite: update user fields (id=%d): %w", id, err)
	}
	return affectedOrNotFound(res)
}

// DeleteUser removes the user row. Returns auth.ErrUserNotFound when absent.
func (s *SQLiteStore) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete user (id=%d): %w", id, err)
	}
	return affectedOrNotFound(res)
}

// ---- row scanning helpers -----------------------------------------------------

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanUser scans a single *sql.Row, mapping sql.ErrNoRows to auth.ErrUserNotFound.
func scanUser(row *sql.Row) (*auth.User, error) {
	u, err := scanUserRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrUserNotFound
	}
	return u, err
}

// scanUserRow scans the standard 7-column user projection into an auth.User.
func scanUserRow(row rowScanner) (*auth.User, error) {
	var u auth.User
	var createdAt int64
	if err := row.Scan(
		&u.ID, &u.Email, &u.Name, &u.PasswordHash,
		&u.SystemRole, &u.TokenGen, &createdAt,
	); err != nil {
		return nil, err
	}
	if createdAt != 0 {
		u.CreatedAt = time.Unix(createdAt, 0).UTC()
	}
	return &u, nil
}

// affectedOrNotFound converts a zero-rows-affected result into auth.ErrUserNotFound.
func affectedOrNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: rows affected: %w", err)
	}
	if n == 0 {
		return auth.ErrUserNotFound
	}
	return nil
}
