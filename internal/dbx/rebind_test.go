package dbx

import "testing"

func TestRebindSQLiteUnchanged(t *testing.T) {
	q := `INSERT INTO orgs (id, name) VALUES (?, ?)`
	if got := Rebind(SQLite, q); got != q {
		t.Fatalf("SQLite Rebind changed the query:\n got: %q\nwant: %q", got, q)
	}
}

func TestRebindPostgresOrdinals(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`WHERE id = ?`, `WHERE id = $1`},
		{`VALUES (?, ?, ?)`, `VALUES ($1, $2, $3)`},
		{
			`INSERT INTO t (a,b,c,d,e,f,g,h,i,j,k) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			`INSERT INTO t (a,b,c,d,e,f,g,h,i,j,k) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		},
		{
			`WHERE org_id = ? AND role IN (?, 'org_admin')`,
			`WHERE org_id = $1 AND role IN ($2, 'org_admin')`,
		},
	}
	for _, c := range cases {
		if got := Rebind(Postgres, c.in); got != c.want {
			t.Errorf("Rebind(Postgres, %q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

func TestRebindPostgresCountsMatchArgs(t *testing.T) {
	// Every "?" must map to exactly one "$N", N running 1..count in order, so a
	// mis-count (which would desync database/sql positional args) is caught.
	q := `INSERT INTO repo_tags (repo_id, tag, digest, updated_at) VALUES (?, ?, ?, ?)
	 ON CONFLICT(repo_id, tag) DO UPDATE SET digest = excluded.digest, updated_at = excluded.updated_at`
	got := Rebind(Postgres, q)
	for i := 1; i <= 4; i++ {
		marker := "$" + itoa(i)
		if !contains(got, marker) {
			t.Errorf("expected placeholder %s in rebound query: %q", marker, got)
		}
	}
	if contains(got, "?") {
		t.Errorf("rebound query still contains '?': %q", got)
	}
	if contains(got, "$5") {
		t.Errorf("rebound query has too many placeholders: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
