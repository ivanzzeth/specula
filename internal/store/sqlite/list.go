package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// listWhere builds the shared WHERE clause (and its args) for ListEntries and
// its companion COUNT. Both queries must be filtered identically or the
// reported Total would not describe the returned page, so the clause is built
// exactly once here and used by both.
//
// Every value is bound as a placeholder argument; no caller-supplied string is
// ever concatenated into the SQL text.
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
		args = append(args, boolToInt(*f.Pinned))
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
func (s *SQLiteStore) ListEntries(
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
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cache_entries`+where, args...,
	).Scan(&total); err != nil {
		return meta.EntryPage{}, fmt.Errorf("sqlite: count entries(%s): %w", protocol, err)
	}

	q := `
		SELECT protocol, name, version, ref_digest, ref_upstream, mutable,
		       digest, size, tier, upstream, etag, verified_at, created_at, pinned
		FROM   cache_entries` + where + orderClause(page) + ` LIMIT ? OFFSET ?`

	rows, err := s.db.QueryContext(ctx, q, append(args, page.Limit, page.Offset)...)
	if err != nil {
		return meta.EntryPage{}, fmt.Errorf("sqlite: list entries(%s): %w", protocol, err)
	}
	defer rows.Close()

	entries := make([]meta.Entry, 0, page.Limit)
	for rows.Next() {
		var (
			e                     meta.Entry
			tier                  int
			mutable, pinned       int
			verifiedAt, createdAt int64
		)
		if err := rows.Scan(
			&e.Ref.Protocol, &e.Ref.Name, &e.Ref.Version,
			&e.Ref.Digest, &e.Ref.Upstream, &mutable,
			&e.Digest, &e.Size, &tier, &e.Upstream, &e.ETag,
			&verifiedAt, &createdAt, &pinned,
		); err != nil {
			return meta.EntryPage{}, fmt.Errorf("sqlite: scan entry: %w", err)
		}
		e.Ref.Mutable = mutable != 0
		e.Protocol = e.Ref.Protocol
		e.Tier = artifact.Tier(tier)
		e.Pinned = pinned != 0
		if verifiedAt != 0 {
			e.VerifiedAt = time.Unix(verifiedAt, 0).UTC()
		}
		if createdAt != 0 {
			e.CreatedAt = time.Unix(createdAt, 0).UTC()
		}
		e.ID = meta.EncodeEntryID(e.Ref)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return meta.EntryPage{}, fmt.Errorf("sqlite: iterate entries: %w", err)
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
func (s *SQLiteStore) SetPinned(ctx context.Context, ref artifact.ArtifactRef, pinned bool) error {
	const q = `UPDATE cache_entries SET pinned = ?
	           WHERE protocol = ? AND name = ? AND version = ?`
	if _, err := s.db.ExecContext(ctx, q,
		boolToInt(pinned), ref.Protocol, ref.Name, ref.Version); err != nil {
		return fmt.Errorf("sqlite: set pinned(%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}
	return nil
}
