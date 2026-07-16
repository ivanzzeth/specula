// Package dbx holds tiny, dependency-free database/sql helpers shared by the
// hand-written SQL stores (org / apikey / grant / repo). Its reason to exist is
// the placeholder-dialect gap: the ported control-plane SQL is written with
// SQLite/MySQL-style "?" positional placeholders, but the pgx stdlib driver
// used for PostgreSQL only understands "$N" ordinal placeholders. Rebind
// converts between the two so a single query string works on both backends.
package dbx

import "strings"

// Dialect selects the SQL placeholder style a store must emit. The zero value
// is SQLite (the historical default), so a store that never sets a dialect
// keeps its original "?" behaviour unchanged.
type Dialect int

const (
	// SQLite uses "?" positional placeholders verbatim (also valid for the
	// modernc.org/sqlite driver and MySQL). This is the zero value.
	SQLite Dialect = iota
	// Postgres uses "$1, $2, …" ordinal placeholders (pgx stdlib driver).
	Postgres
)

// Rebind rewrites the positional "?" placeholders in query for the given
// dialect:
//
//   - SQLite   → returned unchanged (its driver already speaks "?").
//   - Postgres → each "?" is replaced left-to-right with "$1", "$2", … in
//     occurrence order, which is exactly what database/sql passes args as.
//
// Rebind is a purely lexical pass modelled on jmoiron/sqlx's Rebind: it counts
// every "?" byte and does NOT attempt to skip "?" that appear inside string
// literals or quoted identifiers. The Specula control-plane SQL never embeds a
// literal "?" in its query text (all "?" are bind markers), so this limitation
// is not reachable here; do not introduce a literal "?" into a rebindable query
// without escaping it out of the parameter stream first.
func Rebind(d Dialect, query string) string {
	if d != Postgres {
		return query
	}
	n := strings.Count(query, "?")
	if n == 0 {
		return query
	}
	// Pre-size: original length plus up to a couple of extra digits per marker.
	var b strings.Builder
	b.Grow(len(query) + n*2)

	ordinal := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c != '?' {
			b.WriteByte(c)
			continue
		}
		ordinal++
		b.WriteByte('$')
		b.WriteString(itoa(ordinal))
	}
	return b.String()
}

// itoa renders a small positive int without importing strconv (keeps this
// package allocation-light and dependency-free).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
