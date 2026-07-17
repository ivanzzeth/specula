package aptpins

// Internal tests for the ONE thing that differs between the sqlite and postgres
// drivers: placeholder syntax.
//
// This matters more than its size suggests. The sqlite path is covered
// end-to-end by aptpins_test.go against a real database, but a live PostgreSQL
// is not available in `go test -short`, so the postgres dialect's generated SQL
// would otherwise ship completely unexercised. Asserting the rendered statement
// text is the strongest check available without a server — and a mis-numbered
// placeholder is exactly the kind of bug that would make the pin store fail on
// postgres only, i.e. only in the HA topology PRD §G3 specifies, which is the
// one this whole change exists to support.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRewrite_SQLite_LeavesQuestionMarks(t *testing.T) {
	s := New(nil, SQLite)
	const q = `SELECT sha256 FROM apt_pool_pins WHERE scope = ? AND pool_path = ?`
	assert.Equal(t, q, s.rewrite(q), "sqlite must keep ? placeholders verbatim")
}

func TestRewrite_Postgres_NumbersPlaceholdersInOrder(t *testing.T) {
	s := New(nil, Postgres)
	got := s.rewrite(`SELECT sha256 FROM apt_pool_pins WHERE scope = ? AND pool_path = ? LIMIT 2`)
	assert.Equal(t,
		`SELECT sha256 FROM apt_pool_pins WHERE scope = $1 AND pool_path = $2 LIMIT 2`,
		got,
		"postgres placeholders must be numbered left-to-right from $1")
}

func TestRewrite_Postgres_ThreeArgDelete(t *testing.T) {
	s := New(nil, Postgres)
	got := s.rewrite(`DELETE FROM apt_index_pins WHERE scope = ? AND repo = ? AND suite = ?`)
	assert.Equal(t,
		`DELETE FROM apt_index_pins WHERE scope = $1 AND repo = $2 AND suite = $3`,
		got)
}

// TestRewrite_Postgres_NoPlaceholders is the identity case: a statement with no
// binds must pass through untouched rather than gain a stray $1.
func TestRewrite_Postgres_NoPlaceholders(t *testing.T) {
	s := New(nil, Postgres)
	const q = `SELECT 1`
	assert.Equal(t, q, s.rewrite(q))
}

// TestRewrite_Postgres_PreservesNonASCII guards the rune-wise loop: rewriting
// must not mangle multi-byte characters if one ever appears in a literal.
func TestRewrite_Postgres_PreservesNonASCII(t *testing.T) {
	s := New(nil, Postgres)
	got := s.rewrite(`SELECT '值' WHERE x = ?`)
	assert.Equal(t, `SELECT '值' WHERE x = $1`, got)
}

func TestPlaceholder_PerDialect(t *testing.T) {
	assert.Equal(t, "?", New(nil, SQLite).placeholder(1))
	assert.Equal(t, "?", New(nil, SQLite).placeholder(7),
		"sqlite placeholders are positional and always ?")

	assert.Equal(t, "$1", New(nil, Postgres).placeholder(1))
	assert.Equal(t, "$7", New(nil, Postgres).placeholder(7))
	assert.Equal(t, "$1001", New(nil, Postgres).placeholder(1001),
		"a full batch of 1000 rows pushes placeholder numbers into four digits")
}

// TestBatchRows_StaysWithinPostgresParamLimit pins the batch size against the
// constraint that motivates it: PostgreSQL refuses more than 65535 bind
// parameters in one statement. Index pins bind 5 params per row — the widest of
// the two tables — so that is the one to check.
func TestBatchRows_StaysWithinPostgresParamLimit(t *testing.T) {
	const postgresMaxParams = 65535
	assert.LessOrEqual(t, batchRows*5, postgresMaxParams,
		"batchRows * (params per index-pin row) must stay under PostgreSQL's bind-parameter ceiling")
}
