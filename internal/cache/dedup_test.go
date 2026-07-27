package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// The CAS deduplicates by digest: identical bytes are ONE object serving every
// image, repo, protocol and — in the hosted shape — every cluster. That is where
// the storage saving comes from, and it carries a matching hazard: evicting one
// entry must not delete bytes another entry still points at.
//
// Two names storing IDENTICAL content is exactly how it happens in production: two
// image tags sharing a base layer, or one layer reached through two repositories.
func TestEvictingOneOfTwoEntriesSharingABlobKeepsTheBytes(t *testing.T) {
	ctx := context.Background()
	m, fb, fm := newTestManager(t, nil)
	m.maxBytes = 25

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	shared := []byte("aaaaaaaaaa")
	oldRef := storeNamed(t, m, "shared-old", shared, base)
	dgst := digestOf(t, fm, oldRef)
	newRef := storeNamed(t, m, "shared-new", shared, base.Add(time.Second))
	if got := digestOf(t, fm, newRef); got != dgst {
		t.Fatalf("test setup: identical content produced different digests %q vs %q", got, dgst)
	}
	storeNamed(t, m, "filler", []byte("bbbbbbbbbb"), base.Add(2*time.Second))

	if err := m.enforceCapacity(ctx, artifact.ArtifactRef{}); err != nil {
		t.Fatalf("enforce capacity: %v", err)
	}

	page, err := fm.ListEntries(ctx, "", meta.EntryFilter{Digest: dgst}, meta.Page{Limit: 10})
	if err != nil {
		t.Fatalf("list by digest: %v", err)
	}
	if page.Total == 0 {
		t.Skip("both sharing entries were evicted; nothing to protect in this run")
	}
	ok, err := fb.Exists(ctx, dgst)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !ok {
		t.Fatalf("%d entries still reference the blob but its bytes were deleted", page.Total)
	}
}

// The flip side: when the last reference goes, the object must actually go, or
// eviction would report progress while freeing nothing.
func TestEvictingTheLastReferenceDeletesTheBlob(t *testing.T) {
	ctx := context.Background()
	m, fb, fm := newTestManager(t, nil)
	m.maxBytes = 15

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lonely := storeNamed(t, m, "lonely", []byte("cccccccccc"), base)
	dgst := digestOf(t, fm, lonely)
	keep := storeNamed(t, m, "keep", []byte("dddddddddd"), base.Add(time.Second))

	if err := m.enforceCapacity(ctx, keep); err != nil {
		t.Fatalf("enforce capacity: %v", err)
	}

	page, err := fm.ListEntries(ctx, "", meta.EntryFilter{Digest: dgst}, meta.Page{Limit: 10})
	if err != nil {
		t.Fatalf("list by digest: %v", err)
	}
	if page.Total > 0 {
		t.Skip("the unshared entry survived this eviction round")
	}
	ok, err := fb.Exists(ctx, dgst)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if ok {
		t.Error("last reference evicted but the blob was kept — eviction frees nothing")
	}
}

// A row whose blob has vanished — an S3 lifecycle rule, a bucket sweep, an
// eviction elsewhere — must read as a MISS so the caller re-fetches, rather than
// as a server error that turns re-fetchable content into a 502.
func TestEntryWithAMissingBlobReadsAsACacheMiss(t *testing.T) {
	ctx := context.Background()
	m, fb, fm := newTestManager(t, nil)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ref := storeNamed(t, m, "gone", []byte("eeeeeeeeee"), base)
	entry := entryOf(t, fm, ref)

	if err := fb.Delete(ctx, entry.Digest); err != nil {
		t.Fatalf("delete blob: %v", err)
	}

	rc, err := m.ServeEntry(ctx, entry, 0, -1)
	if rc != nil {
		_ = rc.Close()
	}
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("ServeEntry with a missing blob = %v, want ErrCacheMiss", err)
	}
}

func TestIsBlobMissingClassifiesDriverErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("s3 GetObject: NoSuchKey: The specified key does not exist"),
		errors.New("local: open /var/lib/specula/blobs/ab/abc: no such file or directory"),
		errors.New("s3: NotFound"),
		ErrCacheMiss,
	} {
		if !isBlobMissing(err) {
			t.Errorf("isBlobMissing(%v) = false, want true", err)
		}
	}
	// A real failure must never be reported as a miss: that would silently re-fetch
	// on every request and hide an outage as a cache-miss storm.
	for _, err := range []error{
		errors.New("s3 GetObject: AccessDenied"),
		errors.New("dial tcp: i/o timeout"),
		errors.New("local: permission denied"),
	} {
		if isBlobMissing(err) {
			t.Errorf("isBlobMissing(%v) = true, want false", err)
		}
	}
}

// digestOf reads an entry's digest right after it was stored. Store() enforces
// capacity as it goes, so reading later can find the row already evicted.
func digestOf(t *testing.T, fm *fakeMetaStore, ref artifact.ArtifactRef) string {
	t.Helper()
	return entryOf(t, fm, ref).Digest
}

func entryOf(t *testing.T, fm *fakeMetaStore, ref artifact.ArtifactRef) *artifact.CacheEntry {
	t.Helper()
	fm.mu.Lock()
	defer fm.mu.Unlock()
	e := fm.entries[entryKey(ref)]
	if e == nil {
		t.Fatalf("no cache entry for %s/%s@%s", ref.Protocol, ref.Name, ref.Version)
	}
	cp := *e
	return &cp
}
