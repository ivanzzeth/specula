// Package postgres — users.go: the control-plane UserStore backed by the same
// pgx pool as the MetadataStore. It reuses the `users` table (created by the
// embedded goose migrations / ApplySchema) so a single PostgresStore value
// satisfies both meta.MetadataStore and auth.UserStore. Safe for concurrent
// multi-instance use (HA backend).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ivanzzeth/specula/internal/auth"
)

// pgUniqueViolation is the SQLSTATE code for a unique_violation.
const pgUniqueViolation = "23505"

// Compile-time assertion: PostgresStore satisfies the control-plane UserStore.
var _ auth.UserStore = (*PostgresStore)(nil)

// CountUsers returns the total number of user rows (first-user-admin bootstrap).
func (s *PostgresStore) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count users: %w", err)
	}
	return n, nil
}

// GetUserByEmail returns the user with the given (normalised) email, or
// auth.ErrUserNotFound.
func (s *PostgresStore) GetUserByEmail(ctx context.Context, email string) (*auth.User, error) {
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users WHERE email = $1`
	return scanUser(s.pool.QueryRow(ctx, q, email))
}

// GetUserByID returns the user with the given ID, or auth.ErrUserNotFound.
func (s *PostgresStore) GetUserByID(ctx context.Context, id int64) (*auth.User, error) {
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users WHERE id = $1`
	return scanUser(s.pool.QueryRow(ctx, q, id))
}

// CreateUser inserts a new user and returns the stored copy with its assigned
// ID and created_at populated. Returns auth.ErrEmailTaken on unique_violation.
func (s *PostgresStore) CreateUser(ctx context.Context, u auth.User) (*auth.User, error) {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if u.SystemRole == "" {
		u.SystemRole = "user"
	}
	const q = `
		INSERT INTO users (email, name, password_hash, system_role, token_gen, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`
	err := s.pool.QueryRow(ctx, q,
		u.Email, u.Name, u.PasswordHash, u.SystemRole, u.TokenGen, u.CreatedAt,
	).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, auth.ErrEmailTaken
		}
		return nil, fmt.Errorf("postgres: create user %q: %w", u.Email, err)
	}
	return &u, nil
}

// BumpTokenGen atomically increments token_gen and returns the new value,
// invalidating every previously issued session JWT.
func (s *PostgresStore) BumpTokenGen(ctx context.Context, id int64) (int64, error) {
	const q = `UPDATE users SET token_gen = token_gen + 1 WHERE id = $1 RETURNING token_gen`
	var gen int64
	err := s.pool.QueryRow(ctx, q, id).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, auth.ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: bump token gen (id=%d): %w", id, err)
	}
	return gen, nil
}

// ListUsers returns a page of users ordered by ID ascending plus the total row
// count. limit <= 0 returns all rows from offset.
func (s *PostgresStore) ListUsers(ctx context.Context, limit, offset int) ([]auth.User, int64, error) {
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("postgres: list users count: %w", err)
	}

	// NULL limit → all rows (Postgres treats LIMIT NULL as no limit).
	var lim any
	if limit > 0 {
		lim = limit
	}
	const q = `
		SELECT id, email, name, password_hash, system_role, token_gen, created_at
		FROM   users ORDER BY id ASC LIMIT $1 OFFSET $2`
	rows, err := s.pool.Query(ctx, q, lim, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: list users: %w", err)
	}
	defer rows.Close()

	var out []auth.User
	for rows.Next() {
		u, err := scanUserFromRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("postgres: iterate users: %w", err)
	}
	return out, total, nil
}

// UpdateUserRole sets the user's system_role. Returns auth.ErrUserNotFound
// when no row matches id.
func (s *PostgresStore) UpdateUserRole(ctx context.Context, id int64, role string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET system_role = $1 WHERE id = $2`, role, id)
	if err != nil {
		return fmt.Errorf("postgres: update user role (id=%d): %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrUserNotFound
	}
	return nil
}

// DeleteUser removes the user row. Returns auth.ErrUserNotFound when absent.
func (s *PostgresStore) DeleteUser(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete user (id=%d): %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrUserNotFound
	}
	return nil
}

// ---- row scanning helpers -----------------------------------------------------

// pgRow is satisfied by both pgx.Row and pgx.Rows.
type pgRow interface {
	Scan(dest ...any) error
}

// scanUser scans a single pgx.Row, mapping pgx.ErrNoRows to auth.ErrUserNotFound.
func scanUser(row pgx.Row) (*auth.User, error) {
	u, err := scanUserFromRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrUserNotFound
	}
	return u, err
}

// scanUserFromRow scans the standard 7-column user projection into an auth.User.
func scanUserFromRow(row pgRow) (*auth.User, error) {
	var u auth.User
	var createdAt time.Time
	if err := row.Scan(
		&u.ID, &u.Email, &u.Name, &u.PasswordHash,
		&u.SystemRole, &u.TokenGen, &createdAt,
	); err != nil {
		return nil, err
	}
	u.CreatedAt = createdAt.UTC()
	return &u, nil
}
