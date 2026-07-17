// Package aptpins implements the apt GPG trust-chain pin store over database/sql
// for both the sqlite and postgres drivers.
//
// Why one shared implementation rather than a copy per driver: these queries are
// a trust boundary — the pool lookup decides whether unverified bytes reach the
// client. Two hand-maintained copies of that logic would be free to drift, and a
// drift in the fail-closed branch is a silent trust hole. Only the placeholder
// syntax differs between the dialects, so only that is parameterised; the
// schema (see the 0006 / 006 migrations) is deliberately dialect-neutral.
package aptpins

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrAmbiguousPoolPin mirrors verify.ErrAmbiguousPoolPin. It is declared here to
// keep this package free of a dependency on internal/verify (the dependency runs
// the other way: cmd/specula adapts this store to the verify.AptPinStore
// interface). The verifier fails closed on ANY error from PoolPin, so the two
// need not be the same value.
var ErrAmbiguousPoolPin = errors.New("apt pins: pool path pinned to conflicting hashes by two repositories under the same trust anchor")

// Dialect selects placeholder syntax.
type Dialect int

const (
	// SQLite uses "?" placeholders.
	SQLite Dialect = iota
	// Postgres uses "$N" placeholders.
	Postgres
)

// Store implements the apt pin store over an already-migrated *sql.DB.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// New returns a Store over db. db must already have the 0006/006 apt pins
// migration applied (both drivers run their migrations at open time).
func New(db *sql.DB, dialect Dialect) *Store {
	return &Store{db: db, dialect: dialect}
}

// placeholder renders the n-th (1-based) bind placeholder for the dialect.
func (s *Store) placeholder(n int) string {
	if s.dialect == Postgres {
		return "$" + strconv.Itoa(n)
	}
	return "?"
}

// rewrite converts a "?"-placeholder query to the store's dialect.
func (s *Store) rewrite(q string) string {
	if s.dialect != Postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// batchRows caps how many pin rows go into one multi-row INSERT. A real
// Packages index pins tens of thousands of .debs (noble/universe is ~100k), so
// one statement per row would make `apt-get update` crawl, while one statement
// for all of them would blow past PostgreSQL's 65535 bind-parameter ceiling.
//
// Index pins bind 5 params per row (the wider of the two tables), so 1000 rows
// is 5000 params — comfortably inside both PostgreSQL's 65535 limit and SQLite's
// default 32766. TestBatchRows_StaysWithinPostgresParamLimit pins that headroom
// so a future column cannot quietly erode it.
const batchRows = 1000

// ReplaceIndexPins atomically makes pins the complete pin set for
// (scope, repo, suite).
//
// Delete-then-insert inside ONE transaction, so a concurrent reader on another
// replica sees either the old InRelease's pin set or the new one — never a
// half-applied mixture, which could fail a Packages file that is in fact pinned.
//
// Concurrency: two replicas verifying the same InRelease race here. Both run the
// same DELETE predicate first, so the second blocks on the first's row locks
// until it commits, then replays its own (identical) delete + insert. The result
// converges regardless of who wins. Rows are inserted in sorted key order so two
// transactions can never grab the same locks in opposite orders and deadlock —
// Go map iteration order is randomised, so relying on it would make deadlock a
// rare, load-dependent flake.
func (s *Store) ReplaceIndexPins(ctx context.Context, scope, repo, suite string, pins map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apt pins: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.ExecContext(ctx,
		s.rewrite(`DELETE FROM apt_index_pins WHERE scope = ? AND repo = ? AND suite = ?`),
		scope, repo, suite,
	); err != nil {
		return fmt.Errorf("apt pins: clear index pins (suite=%q): %w", suite, err)
	}

	paths := sortedKeys(pins)
	for start := 0; start < len(paths); start += batchRows {
		end := min(start+batchRows, len(paths))
		chunk := paths[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO apt_index_pins (scope, repo, suite, rel_path, sha256) VALUES `)
		args := make([]any, 0, len(chunk)*5)
		for i, p := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			n := len(args)
			sb.WriteString("(" +
				s.placeholder(n+1) + "," + s.placeholder(n+2) + "," + s.placeholder(n+3) + "," +
				s.placeholder(n+4) + "," + s.placeholder(n+5) + ")")
			args = append(args, scope, repo, suite, p, pins[p])
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("apt pins: insert index pins (suite=%q): %w", suite, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apt pins: commit index pins (suite=%q): %w", suite, err)
	}
	return nil
}

// IndexPins returns the pins the most recent verified InRelease established for
// (scope, repo, suite). An empty map means no InRelease has been verified — the
// caller must treat that as "cannot chain verify", never as "anything goes".
func (s *Store) IndexPins(ctx context.Context, scope, repo, suite string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rewrite(`SELECT rel_path, sha256 FROM apt_index_pins WHERE scope = ? AND repo = ? AND suite = ?`),
		scope, repo, suite,
	)
	if err != nil {
		return nil, fmt.Errorf("apt pins: query index pins (suite=%q): %w", suite, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var path, sum string
		if err := rows.Scan(&path, &sum); err != nil {
			return nil, fmt.Errorf("apt pins: scan index pin: %w", err)
		}
		out[path] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apt pins: iterate index pins: %w", err)
	}
	return out, nil
}

// PutPoolPins upserts pool-path → sha256 pins from a Packages index that has
// itself been verified against a signed InRelease.
//
// Upsert rather than replace: pool pins are immutable-tier facts that must
// outlive the InRelease that produced them (see verify.GPGVerifier.verifyPool).
// Within one (scope, repo, pool_path) the newest signed statement wins — if a
// repository re-pins a path to different bytes, the latest signed InRelease is
// the authority, and stale cached bytes then fail closed on the hash mismatch.
//
// Rows are sorted for the same anti-deadlock reason as ReplaceIndexPins.
func (s *Store) PutPoolPins(ctx context.Context, scope, repo string, pins map[string]string) error {
	if len(pins) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apt pins: begin pool pins: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	paths := sortedKeys(pins)
	for start := 0; start < len(paths); start += batchRows {
		end := min(start+batchRows, len(paths))
		chunk := paths[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO apt_pool_pins (scope, repo, pool_path, sha256) VALUES `)
		args := make([]any, 0, len(chunk)*4)
		for i, p := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			n := len(args)
			sb.WriteString("(" +
				s.placeholder(n+1) + "," + s.placeholder(n+2) + "," +
				s.placeholder(n+3) + "," + s.placeholder(n+4) + ")")
			args = append(args, scope, repo, p, pins[p])
		}
		sb.WriteString(` ON CONFLICT (scope, repo, pool_path) DO UPDATE SET sha256 = excluded.sha256`)

		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("apt pins: upsert pool pins (repo=%q): %w", repo, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apt pins: commit pool pins (repo=%q): %w", repo, err)
	}
	return nil
}

// PoolPin returns the sha256hex pinned for poolPath anywhere in scope, or "" if
// no verified Packages index has ever pinned it.
//
// The lookup cannot be narrowed by repo — an immutable pool ref carries no
// repository prefix — so it fails closed on ambiguity: two repositories under
// the same trust anchor pinning one path to different hashes gives no basis to
// choose, and choosing would let one repo's InRelease vouch for another's bytes.
//
// LIMIT 2 on DISTINCT is all the ambiguity check needs: one row is an answer,
// two rows is a conflict, and no more rows need to be read to know that.
func (s *Store) PoolPin(ctx context.Context, scope, poolPath string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		s.rewrite(`SELECT DISTINCT sha256 FROM apt_pool_pins WHERE scope = ? AND pool_path = ? LIMIT 2`),
		scope, poolPath,
	)
	if err != nil {
		return "", fmt.Errorf("apt pins: query pool pin (%q): %w", poolPath, err)
	}
	defer func() { _ = rows.Close() }()

	sums := make([]string, 0, 2)
	for rows.Next() {
		var sum string
		if err := rows.Scan(&sum); err != nil {
			return "", fmt.Errorf("apt pins: scan pool pin (%q): %w", poolPath, err)
		}
		sums = append(sums, sum)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("apt pins: iterate pool pin (%q): %w", poolPath, err)
	}

	switch len(sums) {
	case 0:
		return "", nil
	case 1:
		return sums[0], nil
	default:
		return "", fmt.Errorf("%w: %q", ErrAmbiguousPoolPin, poolPath)
	}
}

// sortedKeys returns the map's keys in deterministic order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
