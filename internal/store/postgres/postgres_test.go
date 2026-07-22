package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// Unit tests — no live database required
// ─────────────────────────────────────────────────────────────────────────────

// TestHashKey verifies that hashKey is deterministic and that typical
// protocol strings produce distinct int64 values.
func TestHashKey(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		keys := []string{
			"",
			"oci",
			"npm",
			"pypi",
			"gomod",
			"helm",
			"apt",
			"git",
			"tarball",
			"oci:library/nginx:latest",
			"github.com/ivanzzeth/specula:v1.0.0",
		}
		for _, key := range keys {
			h1 := hashKey(key)
			h2 := hashKey(key)
			assert.Equal(t, h1, h2, "hashKey(%q) must be deterministic", key)
		}
	})

	t.Run("collision_free_for_protocols", func(t *testing.T) {
		// The eight Specula protocol strings should all hash to distinct values.
		// Collisions would cause false lock serialisation; verify there are none.
		protocols := []string{"oci", "npm", "pypi", "gomod", "helm", "apt", "git", "tarball"}
		seen := make(map[int64]string, len(protocols))
		for _, p := range protocols {
			h := hashKey(p)
			if prev, exists := seen[h]; exists {
				t.Errorf("hashKey collision: %q and %q both hash to %d", p, prev, h)
			}
			seen[h] = p
		}
	})

	t.Run("distinct_keys_distinct_hashes", func(t *testing.T) {
		table := []struct{ a, b string }{
			{"", "x"},
			{"oci:nginx:latest", "oci:nginx:1.25"},
			{"npm:lodash:4.17.21", "npm:lodash:4.17.20"},
		}
		for _, tc := range table {
			ha, hb := hashKey(tc.a), hashKey(tc.b)
			assert.NotEqual(t, ha, hb,
				"hashKey(%q) and hashKey(%q) should differ", tc.a, tc.b)
		}
	})
}

// TestRandomToken verifies basic properties of the fencing token.
func TestRandomToken(t *testing.T) {
	tok1 := randomToken()
	tok2 := randomToken()

	assert.Len(t, tok1, 32, "token should be 32 hex chars (16 random bytes)")
	assert.Len(t, tok2, 32)
	assert.NotEqual(t, tok1, tok2, "successive tokens should differ")
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration test helpers — gated behind SPECULA_TEST_POSTGRES_DSN
// ─────────────────────────────────────────────────────────────────────────────

const envTestDSN = "SPECULA_TEST_POSTGRES_DSN"

// newTestStore opens a PostgresStore against the DSN in SPECULA_TEST_POSTGRES_DSN.
// Skips the test if the env var is not set.
// The store is closed and all rows deleted via t.Cleanup.
//
// Isolation contract: every table is truncated BEFORE the test body runs and
// AGAIN in t.Cleanup.  This means nine tests that each create users cannot
// accumulate rows visible to TestPostgresUserStore_ListUsers — the isolation
// is package-wide, not a per-assertion band-aid.
func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := os.Getenv(envTestDSN)
	if dsn == "" {
		t.Skipf("skipping live-DB test: set %s to a PostgreSQL DSN to enable", envTestDSN)
	}

	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn)
	require.NoError(t, err, "open postgres store")

	require.NoError(t, ApplySchema(ctx, store.pool), "apply schema")

	// truncateAll wipes every table the integration tests touch.  Called once
	// before the test body (pre-test isolation) and once in t.Cleanup
	// (post-test tidiness even on failure / panic).
	truncateAll := func(c context.Context) {
		store.pool.Exec(c, "DELETE FROM users")                  //nolint:errcheck
		store.pool.Exec(c, "DELETE FROM mutable_entries")        //nolint:errcheck
		store.pool.Exec(c, "DELETE FROM cache_entries")          //nolint:errcheck
		store.pool.Exec(c, "DELETE FROM stats_series_samples")   //nolint:errcheck
		store.pool.Exec(c, "DELETE FROM upstream_blocks")        //nolint:errcheck
	}

	// Pre-test isolation: start with a clean slate regardless of leftover rows
	// from a prior run that crashed before its own t.Cleanup executed.
	truncateAll(ctx)

	t.Cleanup(func() {
		c := context.Background()
		truncateAll(c)
		store.Close()
	})

	return store
}

// mustTime parses an RFC3339 string or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts.UTC()
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: immutable tier (cache_entries)
// ─────────────────────────────────────────────────────────────────────────────

func TestPostgresStore_Get_Miss(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "missing/image", Version: "latest"}
	got, err := store.Get(ctx, ref)
	require.NoError(t, err)
	assert.Nil(t, got, "Get on absent entry should return nil")
}

func TestPostgresStore_Put_Get(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)

	tests := []struct {
		name  string
		entry artifact.CacheEntry
	}{
		{
			name: "oci_checksum",
			entry: artifact.CacheEntry{
				Ref:        artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "1.25.0"},
				Digest:     "sha256:aaaa1111",
				Size:       102400,
				Protocol:   "oci",
				Tier:       artifact.TierChecksum,
				Upstream:   "registry-1.docker.io",
				ETag:       `"etag-nginx-1"`,
				VerifiedAt: now,
				CreatedAt:  now,
			},
		},
		{
			name: "npm_tofu",
			entry: artifact.CacheEntry{
				Ref:        artifact.ArtifactRef{Protocol: "npm", Name: "lodash", Version: "4.17.21"},
				Digest:     "sha256:bbbb2222",
				Size:       71680,
				Protocol:   "npm",
				Tier:       artifact.TierTofu,
				Upstream:   "registry.npmjs.org",
				ETag:       `"etag-lodash"`,
				VerifiedAt: now,
				CreatedAt:  now,
			},
		},
		{
			name: "gomod_signed",
			entry: artifact.CacheEntry{
				Ref:        artifact.ArtifactRef{Protocol: "gomod", Name: "github.com/foo/bar", Version: "v1.2.3"},
				Digest:     "sha256:cccc3333",
				Size:       8192,
				Protocol:   "gomod",
				Tier:       artifact.TierSigned,
				Upstream:   "goproxy.cn",
				ETag:       "",
				VerifiedAt: now,
				CreatedAt:  now,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, store.Put(ctx, tc.entry))

			got, err := store.Get(ctx, tc.entry.Ref)
			require.NoError(t, err)
			require.NotNil(t, got)

			assert.Equal(t, tc.entry.Digest, got.Digest)
			assert.Equal(t, tc.entry.Size, got.Size)
			assert.Equal(t, tc.entry.Tier, got.Tier)
			assert.Equal(t, tc.entry.Upstream, got.Upstream)
			assert.Equal(t, tc.entry.ETag, got.ETag)
			assert.Equal(t, tc.entry.Protocol, got.Protocol)
			assert.Equal(t, tc.entry.Ref.Protocol, got.Ref.Protocol)
			assert.Equal(t, tc.entry.Ref.Name, got.Ref.Name)
			assert.Equal(t, tc.entry.Ref.Version, got.Ref.Version)
			assert.WithinDuration(t, tc.entry.VerifiedAt, got.VerifiedAt, time.Millisecond)
			assert.WithinDuration(t, tc.entry.CreatedAt, got.CreatedAt, time.Millisecond)
		})
	}
}

func TestPostgresStore_Put_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "library/alpine", Version: "3.19"}

	original := artifact.CacheEntry{
		Ref: ref, Digest: "sha256:orig", Size: 100,
		Protocol: "oci", Tier: artifact.TierChecksum, Upstream: "up1",
		VerifiedAt: now, CreatedAt: now,
	}
	require.NoError(t, store.Put(ctx, original))

	updated := artifact.CacheEntry{
		Ref: ref, Digest: "sha256:updated", Size: 200,
		Protocol: "oci", Tier: artifact.TierTofu, Upstream: "up2",
		VerifiedAt: now.Add(time.Minute), CreatedAt: now.Add(time.Minute),
	}
	require.NoError(t, store.Put(ctx, updated))

	got, err := store.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	// ON CONFLICT updates digest, size, tier, upstream, verified_at.
	assert.Equal(t, "sha256:updated", got.Digest, "digest should be updated")
	assert.Equal(t, int64(200), got.Size, "size should be updated")
	assert.Equal(t, artifact.TierTofu, got.Tier, "tier should be updated")
	assert.Equal(t, "up2", got.Upstream, "upstream should be updated")

	// created_at preserves the first-write value (not overwritten by ON CONFLICT).
	assert.WithinDuration(t, now, got.CreatedAt, time.Millisecond,
		"created_at should not change on upsert")
}

func TestPostgresStore_Delete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	ref := artifact.ArtifactRef{Protocol: "pypi", Name: "requests", Version: "2.31.0"}
	entry := artifact.CacheEntry{
		Ref: ref, Digest: "sha256:pypi1", Size: 50,
		Protocol: "pypi", Tier: artifact.TierConsensus, Upstream: "pypi.org",
		VerifiedAt: now, CreatedAt: now,
	}
	require.NoError(t, store.Put(ctx, entry))

	// Verify it exists.
	got, err := store.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Delete it.
	require.NoError(t, store.Delete(ctx, ref))

	// Now it should be absent.
	got, err = store.Get(ctx, ref)
	require.NoError(t, err)
	assert.Nil(t, got, "after Delete, Get should return nil")

	// Delete of absent entry should be a no-op (not an error).
	require.NoError(t, store.Delete(ctx, ref))
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: mutable tier (mutable_entries)
// ─────────────────────────────────────────────────────────────────────────────

func TestPostgresStore_GetMutable_Miss(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetMutable(ctx, "oci:does-not-exist:latest")
	require.NoError(t, err)
	assert.Nil(t, got, "GetMutable on absent key should return nil")
}

func TestPostgresStore_PutMutable_Get(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	tests := []struct {
		name  string
		entry artifact.MutableEntry
	}{
		{
			name: "tag_to_digest",
			entry: artifact.MutableEntry{
				Key: "oci:library/nginx:latest", Protocol: "oci",
				Digest: "sha256:abc", Payload: nil,
				ETag: `"etag-tag"`, LastModified: "Mon, 01 Jan 2024 00:00:00 GMT",
				TTLSeconds: 300, Upstream: "registry-1.docker.io",
				FetchedAt: now,
			},
		},
		{
			name: "index_page_with_body",
			entry: artifact.MutableEntry{
				Key: "pypi:simple:requests", Protocol: "pypi",
				Digest: "", Payload: []byte("<html>index</html>"),
				ETag: `"etag-idx"`, LastModified: "",
				TTLSeconds: 1800, Upstream: "pypi.org",
				FetchedAt: now,
			},
		},
		{
			name: "never_revalidate_ttl",
			entry: artifact.MutableEntry{
				Key: "gomod:list:github.com/foo/bar", Protocol: "gomod",
				Digest: "sha256:mod", Payload: []byte("v1.0.0\nv1.1.0\n"),
				TTLSeconds: -1, // config.TTLNeverRevalidate
				Upstream:   "goproxy.cn",
				FetchedAt:  now,
			},
		},
		{
			name: "always_revalidate_ttl",
			entry: artifact.MutableEntry{
				Key: "apt:InRelease:debian/bullseye", Protocol: "apt",
				Digest: "sha256:apt1", Payload: []byte("InRelease data"),
				TTLSeconds: 0, // config.TTLAlwaysRevalidate
				Upstream:   "deb.debian.org",
				FetchedAt:  now,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, store.PutMutable(ctx, tc.entry))

			got, err := store.GetMutable(ctx, tc.entry.Key)
			require.NoError(t, err)
			require.NotNil(t, got)

			assert.Equal(t, tc.entry.Key, got.Key)
			assert.Equal(t, tc.entry.Protocol, got.Protocol)
			assert.Equal(t, tc.entry.Digest, got.Digest)
			assert.Equal(t, tc.entry.ETag, got.ETag)
			assert.Equal(t, tc.entry.LastModified, got.LastModified)
			assert.Equal(t, tc.entry.TTLSeconds, got.TTLSeconds)
			assert.Equal(t, tc.entry.Upstream, got.Upstream)
			assert.WithinDuration(t, tc.entry.FetchedAt, got.FetchedAt, time.Millisecond)

			// Payload normalised to empty slice (not nil) on round-trip.
			wantPayload := tc.entry.Payload
			if wantPayload == nil {
				wantPayload = []byte{}
			}
			assert.Equal(t, wantPayload, got.Payload)
		})
	}
}

func TestPostgresStore_PutMutable_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	key := "npm:packument:express"
	first := artifact.MutableEntry{
		Key: key, Protocol: "npm", Digest: "sha256:d1",
		Payload: []byte("v1"), TTLSeconds: 120, Upstream: "up1",
		FetchedAt: now,
	}
	require.NoError(t, store.PutMutable(ctx, first))

	second := artifact.MutableEntry{
		Key: key, Protocol: "npm", Digest: "sha256:d2",
		Payload: []byte("v2"), TTLSeconds: 240, Upstream: "up2",
		FetchedAt: now.Add(time.Second),
	}
	require.NoError(t, store.PutMutable(ctx, second))

	got, err := store.GetMutable(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "sha256:d2", got.Digest)
	assert.Equal(t, []byte("v2"), got.Payload)
	assert.Equal(t, int64(240), got.TTLSeconds)
	assert.Equal(t, "up2", got.Upstream)
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: CacheSizeByProtocol (G7)
// ─────────────────────────────────────────────────────────────────────────────

func TestPostgresStore_CacheSizeByProtocol(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Empty table → empty map (no groups).
	stats, err := store.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	assert.Empty(t, stats, "empty table should yield empty stats")

	// Insert entries across two protocols.
	entries := []artifact.CacheEntry{
		{
			Ref:    artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "a"},
			Digest: "sha256:01", Size: 1000, Protocol: "oci",
			Tier: artifact.TierChecksum, VerifiedAt: now, CreatedAt: now,
		},
		{
			Ref:    artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "b"},
			Digest: "sha256:02", Size: 2000, Protocol: "oci",
			Tier: artifact.TierChecksum, VerifiedAt: now, CreatedAt: now.Add(time.Second),
		},
		{
			Ref:    artifact.ArtifactRef{Protocol: "npm", Name: "pkg", Version: "1.0.0"},
			Digest: "sha256:03", Size: 500, Protocol: "npm",
			Tier: artifact.TierTofu, VerifiedAt: now, CreatedAt: now,
		},
	}
	for i, e := range entries {
		require.NoError(t, store.Put(ctx, e), "insert entry %d", i)
	}

	stats, err = store.CacheSizeByProtocol(ctx)
	require.NoError(t, err)

	oci, ok := stats["oci"]
	require.True(t, ok, "oci should appear in stats")
	assert.Equal(t, int64(3000), oci.Bytes, "oci bytes = 1000+2000")
	assert.Equal(t, int64(2), oci.Objects, "oci objects = 2")
	assert.False(t, oci.Oldest.IsZero(), "oci oldest should be set")
	assert.False(t, oci.Newest.IsZero(), "oci newest should be set")
	assert.True(t, !oci.Oldest.After(oci.Newest), "oldest <= newest")

	npm, ok := stats["npm"]
	require.True(t, ok, "npm should appear in stats")
	assert.Equal(t, int64(500), npm.Bytes)
	assert.Equal(t, int64(1), npm.Objects)
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: PGAdvisoryLocker
// ─────────────────────────────────────────────────────────────────────────────

func TestPGAdvisoryLocker_AcquireRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	locker := NewPGAdvisoryLocker(store.pool)

	lock, err := locker.Acquire(ctx, "test-lock-key", 5*time.Second)
	require.NoError(t, err, "acquire should succeed")
	require.NotNil(t, lock)

	assert.Len(t, lock.Token(), 32, "token should be 32 hex chars")

	// A second acquire on the same key should block; verify it times out.
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err2 := locker.Acquire(shortCtx, "test-lock-key", 0)
	assert.Error(t, err2, "second acquire on held key should fail/time out")

	// Release the first lock; second acquire should now succeed.
	require.NoError(t, lock.Release(ctx), "release should succeed")

	lock2, err := locker.Acquire(ctx, "test-lock-key", 2*time.Second)
	require.NoError(t, err, "acquire after release should succeed")
	require.NoError(t, lock2.Release(ctx))
}

func TestPGAdvisoryLocker_Release_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	locker := NewPGAdvisoryLocker(store.pool)
	lock, err := locker.Acquire(ctx, "idempotent-lock", 2*time.Second)
	require.NoError(t, err)

	// Multiple Release calls must not panic or return error.
	require.NoError(t, lock.Release(ctx))
	require.NoError(t, lock.Release(ctx))
	require.NoError(t, lock.Release(ctx))
}

func TestPGAdvisoryLocker_DifferentKeys_Independent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	locker := NewPGAdvisoryLocker(store.pool)

	// Locks on different keys must be independent.
	lock1, err := locker.Acquire(ctx, "key-alpha", 2*time.Second)
	require.NoError(t, err)
	defer lock1.Release(ctx) //nolint:errcheck

	lock2, err := locker.Acquire(ctx, "key-beta", 2*time.Second)
	require.NoError(t, err, "different key should be acquirable while key-alpha is held")
	defer lock2.Release(ctx) //nolint:errcheck

	assert.NotEqual(t, lock1.Token(), lock2.Token(), "tokens must differ")
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: user store (auth.UserStore)
// ─────────────────────────────────────────────────────────────────────────────

func newTestPGUser(email, name string) auth.User {
	return auth.User{
		Email:        email,
		Name:         name,
		PasswordHash: "bcrypt-placeholder",
		SystemRole:   "user",
	}
}

func TestPostgresUserStore_CreateGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-alice@example.com", "Alice"))
	require.NoError(t, err)
	assert.NotZero(t, u.ID)
	assert.Equal(t, "Alice", u.Name)
	assert.Equal(t, "user", u.SystemRole)

	got, err := store.GetUserByEmail(ctx, "pg-alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)
	assert.Equal(t, "Alice", got.Name)
}

func TestPostgresUserStore_CreateUser_PersistsName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, auth.User{
		Email:        "pg-named@example.com",
		Name:         "Registered Name",
		PasswordHash: "hash",
		SystemRole:   "admin",
	})
	require.NoError(t, err)
	assert.Equal(t, "Registered Name", u.Name)

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "Registered Name", got.Name)
}

func TestPostgresUserStore_EmailTaken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.CreateUser(ctx, newTestPGUser("pg-dup@example.com", ""))
	require.NoError(t, err)

	_, err = store.CreateUser(ctx, newTestPGUser("pg-dup@example.com", ""))
	require.ErrorIs(t, err, auth.ErrEmailTaken)
}

func TestPostgresUserStore_UpdateUserRole(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-role@example.com", ""))
	require.NoError(t, err)

	require.NoError(t, store.UpdateUserRole(ctx, u.ID, "admin"))

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", got.SystemRole)
}

func TestPostgresUserStore_UpdateUserFields_Name(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-fields@example.com", "Original"))
	require.NoError(t, err)

	newName := "Updated Name"
	require.NoError(t, store.UpdateUserFields(ctx, u.ID, &newName, nil))

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", got.Name)
	assert.Equal(t, "bcrypt-placeholder", got.PasswordHash, "password unchanged")
}

func TestPostgresUserStore_UpdateUserFields_PasswordHash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-pw@example.com", ""))
	require.NoError(t, err)

	newHash := "new-bcrypt-hash"
	require.NoError(t, store.UpdateUserFields(ctx, u.ID, nil, &newHash))

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-bcrypt-hash", got.PasswordHash)
}

func TestPostgresUserStore_UpdateUserFields_Both(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-both@example.com", "Old"))
	require.NoError(t, err)

	newName := "New Name"
	newHash := "refreshed"
	require.NoError(t, store.UpdateUserFields(ctx, u.ID, &newName, &newHash))

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "New Name", got.Name)
	assert.Equal(t, "refreshed", got.PasswordHash)
}

func TestPostgresUserStore_UpdateUserFields_NotFound(t *testing.T) {
	store := newTestStore(t)
	newName := "Ghost"
	err := store.UpdateUserFields(context.Background(), 9999, &newName, nil)
	require.ErrorIs(t, err, auth.ErrUserNotFound)
}

func TestPostgresUserStore_DeleteUser(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-del@example.com", ""))
	require.NoError(t, err)

	require.NoError(t, store.DeleteUser(ctx, u.ID))

	_, err = store.GetUserByID(ctx, u.ID)
	require.ErrorIs(t, err, auth.ErrUserNotFound)
}

func TestPostgresUserStore_BumpTokenGen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, newTestPGUser("pg-bump@example.com", ""))
	require.NoError(t, err)

	newGen, err := store.BumpTokenGen(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), newGen)

	got, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.TokenGen)
}

func TestPostgresUserStore_ListUsers(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, email := range []string{"pg-a@x.com", "pg-b@x.com", "pg-c@x.com"} {
		_, err := store.CreateUser(ctx, newTestPGUser(email, ""))
		require.NoError(t, err)
	}

	users, total, err := store.ListUsers(ctx, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Len(t, users, 3)
}
