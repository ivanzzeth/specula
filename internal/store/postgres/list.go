package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/dbx"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// listWhere builds the shared WHERE clause (and its args) for ListEntries and
// its companion COUNT. Both must be filtered identically or the reported Total
// would not describe the returned page, so the clause is built once here.
//
// The clause is written with "?" placeholders and rebound to "$N" by dbx.Rebind
// at the call site: the fragment is assembled dynamically (a variable number of
// conditions), so "?" keeps the builder from having to track ordinals by hand —
// which is exactly the bookkeeping that produces off-by-one bind bugs. Every
// value is bound; no caller-supplied string is concatenated into the SQL text.
func listWhere(protocol string, f meta.EntryFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	// An empty protocol deliberately means "all protocols" (the interface
	// documents this), so it is only constrained when non-empty.
	if protocol != "" {
		conds = append(conds, "protocol = ?")
		args = append(args, protocol)
	}
	if f.NameContains != "" {
		// ESCAPE makes a user-typed % or _ literal instead of a wildcard, so a
		// search for "foo_bar" cannot silently match "fooXbar".
		conds = append(conds, `name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(f.NameContains)+"%")
	}
	if f.Tier != nil {
		conds = append(conds, "tier = ?")
		args = append(args, int(*f.Tier))
	}
	if f.Upstream != "" {
		conds = append(conds, "upstream = ?")
		args = append(args, f.Upstream)
	}
	if f.Pinned != nil {
		conds = append(conds, "pinned = ?")
		args = append(args, *f.Pinned)
	}
	if f.Origin != "" {
		conds = append(conds, "origin = ?")
		args = append(args, artifact.NormalizeOrigin(f.Origin))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// escapeLike neutralises the LIKE metacharacters in a user-supplied substring.
// Paired with ESCAPE '\' in the query above.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// orderClause maps a normalized SortField onto SQL. The mapping is a closed
// whitelist — the field never reaches the query as caller text — because ORDER
// BY cannot be parameterised and would otherwise be an injection point.
//
// Every ordering is tie-broken by the full primary key so that a page window is
// stable: without it, rows sharing a created_at/size could be returned in a
// different order per query and paginate into duplicates or gaps.
func orderClause(p meta.Page) string {
	dir := "ASC"
	if p.Desc {
		dir = "DESC"
	}
	var col string
	switch p.Sort {
	case meta.SortSize:
		col = "size"
	case meta.SortName:
		col = "name"
	case meta.SortVerifiedAt:
		col = "verified_at"
	default:
		col = "created_at"
	}
	return fmt.Sprintf(" ORDER BY %s %s, protocol ASC, name ASC, version ASC", col, dir)
}

// ListEntries returns one page of immutable cache entries for protocol, matching
// filter and windowed by page. Total is computed with a COUNT over the same
// predicate so the UI can render "showing N of M".
//
// Note the cache_entries table on PostgreSQL has no ref_digest / ref_upstream /
// mutable columns (they exist only in the SQLite schema), so — exactly as Get
// does — Ref.Digest and Ref.Upstream are back-filled from the entry's own
// digest/upstream and Ref.Mutable is left false.
func (s *PostgresStore) ListEntries(
	ctx context.Context,
	protocol string,
	filter meta.EntryFilter,
	page meta.Page,
) (meta.EntryPage, error) {
	page = page.Normalize()
	where, args := listWhere(protocol, filter)

	// Total first: a page beyond the end still needs an accurate count for the
	// pager, and it must be measured against the same predicate as the rows.
	var total int64
	countQ := dbx.Rebind(dbx.Postgres, `SELECT COUNT(*) FROM cache_entries`+where)
	if err := s.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return meta.EntryPage{}, fmt.Errorf("postgres: count entries(%s): %w", protocol, err)
	}

	q := dbx.Rebind(dbx.Postgres, `
		SELECT protocol, name, version, digest, size, tier,
		       upstream, etag, verified_at, created_at, pinned, origin
		FROM   cache_entries`+where+orderClause(page)+` LIMIT ? OFFSET ?`)

	rows, err := s.pool.Query(ctx, q, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return meta.EntryPage{}, fmt.Errorf("postgres: list entries(%s): %w", protocol, err)
	}
	defer rows.Close()

	entries := make([]meta.Entry, 0, page.Limit)
	for rows.Next() {
		var (
			e                     meta.Entry
			tier                  int
			verifiedAt, createdAt time.Time
			origin                string
		)
		if err := rows.Scan(
			&e.Ref.Protocol, &e.Ref.Name, &e.Ref.Version,
			&e.Digest, &e.Size, &tier, &e.Upstream, &e.ETag,
			&verifiedAt, &createdAt, &e.Pinned, &origin,
		); err != nil {
			return meta.EntryPage{}, fmt.Errorf("postgres: scan entry: %w", err)
		}
		e.Protocol = e.Ref.Protocol
		e.Tier = artifact.Tier(tier)
		e.Origin = artifact.NormalizeOrigin(origin)
		e.Ref.Digest = e.Digest
		e.Ref.Upstream = e.Upstream
		e.VerifiedAt = verifiedAt.UTC()
		e.CreatedAt = createdAt.UTC()
		e.ID = meta.EncodeEntryID(e.Ref)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return meta.EntryPage{}, fmt.Errorf("postgres: iterate entries: %w", err)
	}

	return meta.EntryPage{
		Entries: entries,
		Total:   total,
		Limit:   page.Limit,
		Offset:  page.Offset,
	}, nil
}

// SetPinned marks or unmarks an entry as protected from eviction. Missing rows
// are a no-op rather than an error: pinning is idempotent from the caller's
// point of view, and an entry evicted concurrently must not fail the request.
func (s *PostgresStore) SetPinned(ctx context.Context, ref artifact.ArtifactRef, pinned bool) error {
	const q = `UPDATE cache_entries SET pinned = $1
	           WHERE protocol = $2 AND name = $3 AND version = $4`
	if _, err := s.pool.Exec(ctx, q, pinned, ref.Protocol, ref.Name, ref.Version); err != nil {
		return fmt.Errorf("postgres: set pinned(%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}
	return nil
}
