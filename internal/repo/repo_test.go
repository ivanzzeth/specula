package repo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/repo"
	pgstore "github.com/ivanzzeth/specula/internal/store/postgres"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ── store factory helpers ─────────────────────────────────────────────────────

// storeCase bundles a name and a ready-to-use SQLStore.
type storeCase struct {
	name  string
	store *repo.SQLStore
}

// newSQLiteStore opens a temp SQLite DB (all migrations applied) and returns an
// repo.SQLStore backed by the same handle. Cleanup is registered automatically.
func newSQLiteStore(t *testing.T) *repo.SQLStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "repo_test.db")
	s, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "NewSQLiteStore must succeed")
	t.Cleanup(func() { _ = s.Close() })
	return repo.NewSQLStore(s.DB())
}

// newPostgresStore opens a repo.SQLStore against the DSN in
// SPECULA_TEST_POSTGRES_DSN. Skips the test if the env var is not set.
// A cleanup that deletes all repo_tags and repos rows is registered.
func newPostgresStore(t *testing.T) *repo.SQLStore {
	t.Helper()
	const envVar = "SPECULA_TEST_POSTGRES_DSN"
	dsn := os.Getenv(envVar)
	if dsn == "" {
		t.Skipf("skipping postgres test: set %s to enable", envVar)
	}

	db, err := pgstore.OpenSQLDB(dsn)
	require.NoError(t, err, "OpenSQLDB")

	// Apply migrations so that repos + repo_tags tables exist.
	require.NoError(t, pgstore.Migrate(db), "Migrate")

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM repo_tags")
		_, _ = db.Exec("DELETE FROM repos")
		_ = db.Close()
	})
	return repo.NewSQLStorePostgres(db)
}

// stores returns one SQLiteStore and (if the env is set) a Postgres store.
func stores(t *testing.T) []storeCase {
	t.Helper()
	cases := []storeCase{
		{"sqlite", newSQLiteStore(t)},
	}
	const envVar = "SPECULA_TEST_POSTGRES_DSN"
	if os.Getenv(envVar) != "" {
		cases = append(cases, storeCase{"postgres", newPostgresStore(t)})
	}
	return cases
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustCreateRepo(t *testing.T, rs repo.RepoStore, orgID, name, vis, owner string) *repo.Repo {
	t.Helper()
	r, err := rs.CreateRepo(context.Background(), orgID, name, vis, owner)
	require.NoError(t, err, "CreateRepo(%q, %q)", orgID, name)
	return r
}

// ── NormalizeVisibility ───────────────────────────────────────────────────────

func TestNormalizeVisibility(t *testing.T) {
	assert.Equal(t, repo.VisibilityPrivate, repo.NormalizeVisibility(""))
	assert.Equal(t, repo.VisibilityPrivate, repo.NormalizeVisibility("unknown"))
	assert.Equal(t, repo.VisibilityPrivate, repo.NormalizeVisibility(repo.VisibilityPrivate))
	assert.Equal(t, repo.VisibilityPublic, repo.NormalizeVisibility(repo.VisibilityPublic))
}

// ── ToACLResource ─────────────────────────────────────────────────────────────

func TestToACLResource(t *testing.T) {
	r := &repo.Repo{
		ID:          "repo_abc",
		OrgID:       "org1",
		Name:        "org1/myrepo",
		Visibility:  repo.VisibilityPublic,
		OwnerUserID: "user:42",
	}
	res := r.ToACLResource()
	assert.Equal(t, "user:42", res.OwnerUserID)
	assert.Equal(t, "org1", res.OrgID)

	// private visibility
	r.Visibility = repo.VisibilityPrivate
	res = r.ToACLResource()
	assert.Equal(t, "user:42", res.OwnerUserID)
}

// ── Repo CRUD ─────────────────────────────────────────────────────────────────

func TestRepoCRUD(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// ── CreateRepo ──
			r, err := tc.store.CreateRepo(ctx, "org1", "org1/myrepo", repo.VisibilityPrivate, "user:1")
			require.NoError(t, err)
			assert.NotEmpty(t, r.ID, "ID must be generated")
			assert.True(t, len(r.ID) > 5, "ID should have a prefix")
			assert.Equal(t, "org1", r.OrgID)
			assert.Equal(t, "org1/myrepo", r.Name)
			assert.Equal(t, repo.VisibilityPrivate, r.Visibility)
			assert.Equal(t, "user:1", r.OwnerUserID)
			assert.WithinDuration(t, time.Now().UTC(), r.CreatedAt, 5*time.Second)

			// ── GetRepo ──
			got, err := tc.store.GetRepo(ctx, "org1", "org1/myrepo")
			require.NoError(t, err)
			assert.Equal(t, r.ID, got.ID)
			assert.Equal(t, r.OrgID, got.OrgID)
			assert.Equal(t, r.Name, got.Name)
			assert.Equal(t, r.Visibility, got.Visibility)
			assert.Equal(t, r.OwnerUserID, got.OwnerUserID)

			// ── GetRepo – not found ──
			_, err = tc.store.GetRepo(ctx, "org1", "org1/missing")
			require.Error(t, err)
			assert.True(t, errors.Is(err, repo.ErrNotFound), "expected ErrNotFound, got %v", err)

			// ── GetRepo – wrong org ──
			_, err = tc.store.GetRepo(ctx, "orgX", "org1/myrepo")
			require.Error(t, err)
			assert.True(t, errors.Is(err, repo.ErrNotFound))

			// ── Duplicate name in same org is rejected ──
			_, err = tc.store.CreateRepo(ctx, "org1", "org1/myrepo", repo.VisibilityPublic, "user:2")
			require.Error(t, err, "duplicate (org_id, name) must be rejected")

			// ── Same name in different org is allowed ──
			r2, err := tc.store.CreateRepo(ctx, "org2", "org1/myrepo", repo.VisibilityPublic, "user:2")
			require.NoError(t, err, "same name in different org must succeed")
			assert.NotEqual(t, r.ID, r2.ID)

			// ── DeleteRepo ──
			require.NoError(t, tc.store.DeleteRepo(ctx, "org1", "org1/myrepo"))
			_, err = tc.store.GetRepo(ctx, "org1", "org1/myrepo")
			assert.True(t, errors.Is(err, repo.ErrNotFound), "deleted repo must not be found")

			// ── DeleteRepo – not found returns ErrNotFound ──
			err = tc.store.DeleteRepo(ctx, "org1", "org1/myrepo")
			assert.True(t, errors.Is(err, repo.ErrNotFound), "delete of absent repo must return ErrNotFound")
		})
	}
}

// ── Empty visibility defaults to private ──────────────────────────────────────

func TestCreateRepo_DefaultVisibility(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreateRepo(t, tc.store, "orgV", "orgV/repo", "", "user:1")
			assert.Equal(t, repo.VisibilityPrivate, r.Visibility,
				"empty visibility must default to private")
		})
	}
}

// ── Visibility get / set ──────────────────────────────────────────────────────

func TestVisibility(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			r := mustCreateRepo(t, tc.store, "orgVis", "orgVis/repo", repo.VisibilityPrivate, "user:1")
			assert.Equal(t, repo.VisibilityPrivate, r.Visibility)

			// Change to public.
			require.NoError(t, tc.store.SetVisibility(ctx, "orgVis", "orgVis/repo", repo.VisibilityPublic))
			got, err := tc.store.GetRepo(ctx, "orgVis", "orgVis/repo")
			require.NoError(t, err)
			assert.Equal(t, repo.VisibilityPublic, got.Visibility)

			// Unknown visibility is normalised to private.
			require.NoError(t, tc.store.SetVisibility(ctx, "orgVis", "orgVis/repo", "unknown"))
			got, err = tc.store.GetRepo(ctx, "orgVis", "orgVis/repo")
			require.NoError(t, err)
			assert.Equal(t, repo.VisibilityPrivate, got.Visibility)

			// Change back to private explicitly.
			require.NoError(t, tc.store.SetVisibility(ctx, "orgVis", "orgVis/repo", repo.VisibilityPrivate))
			got, err = tc.store.GetRepo(ctx, "orgVis", "orgVis/repo")
			require.NoError(t, err)
			assert.Equal(t, repo.VisibilityPrivate, got.Visibility)
		})
	}
}

// ── ListRepos (org-scoped) ────────────────────────────────────────────────────

func TestListRepos(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Empty org returns empty list (not nil or error).
			list, err := tc.store.ListRepos(ctx, "orgEmpty")
			require.NoError(t, err)
			assert.Empty(t, list)

			// Create repos in two orgs.
			mustCreateRepo(t, tc.store, "orgA", "orgA/alpha", repo.VisibilityPrivate, "user:1")
			mustCreateRepo(t, tc.store, "orgA", "orgA/beta", repo.VisibilityPublic, "user:1")
			mustCreateRepo(t, tc.store, "orgB", "orgB/gamma", repo.VisibilityPrivate, "user:2")

			// ListRepos for orgA must return only orgA's repos.
			listA, err := tc.store.ListRepos(ctx, "orgA")
			require.NoError(t, err)
			assert.Len(t, listA, 2, "orgA must have exactly two repos")
			for _, r := range listA {
				assert.Equal(t, "orgA", r.OrgID, "all listed repos must belong to orgA")
			}

			// ListRepos for orgB must return only orgB's repo.
			listB, err := tc.store.ListRepos(ctx, "orgB")
			require.NoError(t, err)
			assert.Len(t, listB, 1)
			assert.Equal(t, "orgB/gamma", listB[0].Name)

			// Newest-first ordering: alpha and beta were created in order; beta
			// should be first (higher created_at).
			names := make([]string, len(listA))
			for i, r := range listA {
				names[i] = r.Name
			}
			// The two repos must be present (order may vary by sub-second timing,
			// but both must appear).
			assert.ElementsMatch(t, []string{"orgA/alpha", "orgA/beta"}, names)
		})
	}
}

// ── Tag lifecycle ─────────────────────────────────────────────────────────────

func TestTagLifecycle(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			r := mustCreateRepo(t, tc.store, "orgT", "orgT/testrepo", repo.VisibilityPrivate, "user:1")

			// ── GetTag – not found ──
			_, err := tc.store.GetTag(ctx, r.ID, "latest")
			require.Error(t, err)
			assert.True(t, errors.Is(err, repo.ErrNotFound))

			// ── PutTag (insert) ──
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "latest", "sha256:aabbcc"))
			tag, err := tc.store.GetTag(ctx, r.ID, "latest")
			require.NoError(t, err)
			assert.Equal(t, r.ID, tag.RepoID)
			assert.Equal(t, "latest", tag.Tag)
			assert.Equal(t, "sha256:aabbcc", tag.Digest)
			assert.WithinDuration(t, time.Now().UTC(), tag.UpdatedAt, 5*time.Second)

			// ── PutTag (upsert – same tag, new digest) ──
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "latest", "sha256:ddeeff"))
			tag2, err := tc.store.GetTag(ctx, r.ID, "latest")
			require.NoError(t, err)
			assert.Equal(t, "sha256:ddeeff", tag2.Digest, "digest must be updated on upsert")
			// UpdatedAt must be >= the first write.
			assert.False(t, tag2.UpdatedAt.Before(tag.UpdatedAt), "UpdatedAt must be >= previous")

			// ── Multiple tags ──
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "v1.0", "sha256:111111"))
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "v2.0", "sha256:222222"))

			// ── ListTags – tag-name ascending ──
			tags, err := tc.store.ListTags(ctx, r.ID)
			require.NoError(t, err)
			require.Len(t, tags, 3, "must have latest, v1.0, v2.0")

			tagNames := make([]string, len(tags))
			for i, tg := range tags {
				tagNames[i] = tg.Tag
			}
			// Ascending order: "latest" < "v1.0" < "v2.0" lexicographically.
			assert.Equal(t, []string{"latest", "v1.0", "v2.0"}, tagNames)

			// ── Tags are scoped per repo ──
			r2 := mustCreateRepo(t, tc.store, "orgT", "orgT/otherrepo", repo.VisibilityPublic, "user:1")
			require.NoError(t, tc.store.PutTag(ctx, r2.ID, "latest", "sha256:999999"))
			tagsR2, err := tc.store.ListTags(ctx, r2.ID)
			require.NoError(t, err)
			assert.Len(t, tagsR2, 1, "tags for r2 must not bleed into r")

			// ── DeleteTag ──
			require.NoError(t, tc.store.DeleteTag(ctx, r.ID, "v1.0"))
			_, err = tc.store.GetTag(ctx, r.ID, "v1.0")
			assert.True(t, errors.Is(err, repo.ErrNotFound), "deleted tag must not be found")

			// DeleteTag is a no-op for absent tags (must not error).
			require.NoError(t, tc.store.DeleteTag(ctx, r.ID, "v1.0"), "delete absent tag must be no-op")

			// ── ListTags after delete ──
			tagsAfter, err := tc.store.ListTags(ctx, r.ID)
			require.NoError(t, err)
			assert.Len(t, tagsAfter, 2, "must have latest and v2.0 remaining")
		})
	}
}

// ── DeleteRepo cascades to tags ───────────────────────────────────────────────

func TestDeleteRepoCascadesTags(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			r := mustCreateRepo(t, tc.store, "orgCasc", "orgCasc/repo", repo.VisibilityPrivate, "user:1")
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "v1", "sha256:aaa"))
			require.NoError(t, tc.store.PutTag(ctx, r.ID, "v2", "sha256:bbb"))

			// Verify tags exist before deletion.
			tags, err := tc.store.ListTags(ctx, r.ID)
			require.NoError(t, err)
			assert.Len(t, tags, 2)

			// DeleteRepo must also remove tag rows.
			require.NoError(t, tc.store.DeleteRepo(ctx, "orgCasc", "orgCasc/repo"))

			// Tags for the deleted repo must be gone.
			tagsAfter, err := tc.store.ListTags(ctx, r.ID)
			require.NoError(t, err)
			assert.Empty(t, tagsAfter, "tags must be removed when the repo is deleted")
		})
	}
}

// ── ListTags empty repo ───────────────────────────────────────────────────────

func TestListTags_EmptyRepo(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreateRepo(t, tc.store, "orgLT", "orgLT/empty", repo.VisibilityPrivate, "user:1")

			tags, err := tc.store.ListTags(context.Background(), r.ID)
			require.NoError(t, err)
			assert.Empty(t, tags, "fresh repo must have no tags")
		})
	}
}

// ── Name trimming ─────────────────────────────────────────────────────────────

func TestCreateRepo_TrimName(t *testing.T) {
	for _, tc := range stores(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreateRepo(t, tc.store, "orgTrim", "  orgTrim/repo  ", repo.VisibilityPublic, "user:1")
			assert.Equal(t, "orgTrim/repo", r.Name, "leading/trailing spaces must be trimmed")
		})
	}
}
