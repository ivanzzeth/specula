package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/verify"
)

// --------------------------------------------------------------------------
// callLog — records ordered operations for write-order assertions
// --------------------------------------------------------------------------

type callLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *callLog) record(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *callLog) Ops() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

// --------------------------------------------------------------------------
// fakeBlobStore — in-memory BlobStore
// --------------------------------------------------------------------------

type fakeBlobStore struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	log    *callLog
	putErr error
	getErr error
}

func newFakeBlob(log *callLog) *fakeBlobStore {
	return &fakeBlobStore{blobs: make(map[string][]byte), log: log}
}

var _ blob.BlobStore = (*fakeBlobStore)(nil)

func (f *fakeBlobStore) Get(_ context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error) {
	if f.getErr != nil {
		return nil, 0, f.getErr
	}
	f.mu.Lock()
	data, ok := f.blobs[digest]
	f.mu.Unlock()
	if !ok {
		return nil, 0, errors.New("fake blob: not found: " + digest)
	}
	total := int64(len(data))
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	slice := data[offset:]
	if length >= 0 && length < int64(len(slice)) {
		slice = slice[:length]
	}
	return io.NopCloser(bytes.NewReader(slice)), total, nil
}

func (f *fakeBlobStore) Put(_ context.Context, digest string, r io.Reader, _ int64) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.log != nil {
		f.log.record("blob.Put")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.blobs[digest] = data
	f.mu.Unlock()
	return nil
}

func (f *fakeBlobStore) Exists(_ context.Context, digest string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.blobs[digest]
	return ok, nil
}

func (f *fakeBlobStore) Delete(_ context.Context, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blobs, digest)
	return nil
}

func (f *fakeBlobStore) UsageBytes(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, v := range f.blobs {
		n += int64(len(v))
	}
	return n, nil
}

// --------------------------------------------------------------------------
// fakeMetaStore — in-memory MetadataStore
// --------------------------------------------------------------------------

// entryKey mirrors the REAL immutable keying of the production MetadataStore
// implementations, which is (protocol, name, version) and deliberately does NOT
// include the digest:
//
//	sqlite: WHERE protocol = ? AND name = ? AND version = ?
//
// This double previously keyed on ref.Digest alone — the exact inverse of
// production. That made a wrong caller-supplied pin look like a clean cache
// miss in every test, hiding the warm-path pin fail-open that a real client
// later caught. A double must key the way the real store keys, or the tests
// above it prove nothing.
func entryKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

type fakeMetaStore struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry   // keyed by entryKey (protocol:name:version)
	mutable map[string]*artifact.MutableEntry // keyed by mutableKey
	pinned  map[string]bool                   // keyed by entryKey
	log     *callLog
	putErr  error
}

func newFakeMeta(log *callLog) *fakeMetaStore {
	return &fakeMetaStore{
		entries: make(map[string]*artifact.CacheEntry),
		mutable: make(map[string]*artifact.MutableEntry),
		pinned:  make(map[string]bool),
		log:     log,
	}
}

var _ meta.MetadataStore = (*fakeMetaStore)(nil)

func (f *fakeMetaStore) Get(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[entryKey(ref)]
	if e == nil {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (f *fakeMetaStore) Put(_ context.Context, entry artifact.CacheEntry) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.log != nil {
		f.log.record("meta.Put")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := entry
	f.entries[entryKey(entry.Ref)] = &cp
	return nil
}

func (f *fakeMetaStore) Delete(_ context.Context, ref artifact.ArtifactRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := entryKey(ref)
	delete(f.entries, k)
	delete(f.pinned, k)
	return nil
}

func (f *fakeMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.mutable[key]
	if e == nil {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (f *fakeMetaStore) PutMutable(_ context.Context, entry artifact.MutableEntry) error {
	if f.log != nil {
		f.log.record("meta.PutMutable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := entry
	f.mutable[entry.Key] = &cp
	return nil
}

func (f *fakeMetaStore) DeleteMutable(_ context.Context, key string) error {
	if f.log != nil {
		f.log.record("meta.DeleteMutable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mutable, key)
	return nil
}

// ListEntries / SetPinned implement enough of meta.MetadataStore for capacity
// eviction tests (oldest-unpinned ordering + pin filter).
func (f *fakeMetaStore) ListEntries(_ context.Context, protocol string, filter meta.EntryFilter, page meta.Page) (meta.EntryPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	page = page.Normalize()

	var all []meta.Entry
	for k, e := range f.entries {
		if protocol != "" && e.Protocol != protocol {
			continue
		}
		if filter.Pinned != nil && f.pinned[k] != *filter.Pinned {
			continue
		}
		if filter.NameContains != "" && !strings.Contains(e.Ref.Name, filter.NameContains) {
			continue
		}
		if filter.Tier != nil && e.Tier != *filter.Tier {
			continue
		}
		if filter.Upstream != "" && e.Upstream != filter.Upstream {
			continue
		}
		cp := *e
		all = append(all, meta.Entry{CacheEntry: cp, Pinned: f.pinned[k]})
	}

	switch page.Sort {
	case meta.SortSize:
		sort.Slice(all, func(i, j int) bool {
			if page.Desc {
				return all[i].Size > all[j].Size
			}
			return all[i].Size < all[j].Size
		})
	case meta.SortName:
		sort.Slice(all, func(i, j int) bool {
			if page.Desc {
				return all[i].Ref.Name > all[j].Ref.Name
			}
			return all[i].Ref.Name < all[j].Ref.Name
		})
	default: // created_at / verified_at
		sort.Slice(all, func(i, j int) bool {
			ai, aj := all[i].CreatedAt, all[j].CreatedAt
			if page.Sort == meta.SortVerifiedAt {
				ai, aj = all[i].VerifiedAt, all[j].VerifiedAt
			}
			if page.Desc {
				return ai.After(aj)
			}
			return ai.Before(aj)
		})
	}

	total := int64(len(all))
	if page.Offset >= len(all) {
		return meta.EntryPage{Entries: nil, Total: total, Limit: page.Limit, Offset: page.Offset}, nil
	}
	end := page.Offset + page.Limit
	if end > len(all) {
		end = len(all)
	}
	return meta.EntryPage{
		Entries: all[page.Offset:end],
		Total:   total,
		Limit:   page.Limit,
		Offset:  page.Offset,
	}, nil
}

func (f *fakeMetaStore) SetPinned(_ context.Context, ref artifact.ArtifactRef, pinned bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := entryKey(ref)
	if _, ok := f.entries[k]; !ok {
		return nil
	}
	f.pinned[k] = pinned
	return nil
}

func (f *fakeMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := make(map[string]artifact.SizeStat)
	for _, e := range f.entries {
		s := stats[e.Protocol]
		s.Bytes += e.Size
		s.Objects++
		if s.Oldest.IsZero() || e.CreatedAt.Before(s.Oldest) {
			s.Oldest = e.CreatedAt
		}
		if e.CreatedAt.After(s.Newest) {
			s.Newest = e.CreatedAt
		}
		stats[e.Protocol] = s
	}
	return stats, nil
}

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

// newTestManager constructs a *manager wired to fake stores. The returned
// concrete *manager lets tests inject verifyFn.
func newTestManager(t *testing.T, log *callLog) (*manager, *fakeBlobStore, *fakeMetaStore) {
	t.Helper()
	fb := newFakeBlob(log)
	fm := newFakeMeta(log)
	m := New(fb, fm, verify.NewChain()).(*manager)
	return m, fb, fm
}

// makeQuarantine writes content to a real temp file, simulating what
// Quarantine() would produce from an upstream response.
func makeQuarantine(t *testing.T, content []byte) *artifact.Artifact {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-quar-*")
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return &artifact.Artifact{
		Path:   f.Name(),
		Digest: "sha256:deadbeef",
		Size:   int64(len(content)),
		Meta:   artifact.UpstreamMeta{Upstream: "test-mirror", ETag: `"etag1"`},
	}
}

func passVerify(tier artifact.Tier) func(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error) {
	return func(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
		return artifact.Result{Status: artifact.StatusPass, Tier: tier}, nil
	}
}

func warnVerify(tier artifact.Tier) func(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error) {
	return func(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
		return artifact.Result{Status: artifact.StatusWarn, Tier: tier, Message: "first seen"}, nil
	}
}

func failVerify() func(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error) {
	return func(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
		return artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierChecksum, Message: "digest mismatch"}, nil
	}
}

// --------------------------------------------------------------------------
// Tests: isMutableFresh
// --------------------------------------------------------------------------

func TestIsMutableFresh(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		entry artifact.MutableEntry
		want  bool
	}{
		{
			name:  "never revalidate sentinel (-1) always fresh regardless of age",
			entry: artifact.MutableEntry{TTLSeconds: -1, FetchedAt: now.Add(-100 * 24 * time.Hour)},
			want:  true,
		},
		{
			name:  "always revalidate sentinel (0) always stale",
			entry: artifact.MutableEntry{TTLSeconds: 0, FetchedAt: now},
			want:  false,
		},
		{
			name:  "within TTL window",
			entry: artifact.MutableEntry{TTLSeconds: 3600, FetchedAt: now.Add(-30 * time.Minute)},
			want:  true,
		},
		{
			name:  "past TTL window",
			entry: artifact.MutableEntry{TTLSeconds: 60, FetchedAt: now.Add(-2 * time.Minute)},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.entry
			assert.Equal(t, tc.want, isMutableFresh(&e))
		})
	}
}

// --------------------------------------------------------------------------
// Tests: Lookup — immutable tier
// --------------------------------------------------------------------------

func TestLookupImmutable(t *testing.T) {
	const digest = "sha256:abc123"
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "1.25", Digest: digest}

	tests := []struct {
		name      string
		setupMeta func(*fakeMetaStore)
		setupBlob func(*fakeBlobStore)
		wantNil   bool
	}{
		{
			name:    "meta miss returns nil",
			wantNil: true,
		},
		{
			name: "meta hit + blob present returns entry",
			setupMeta: func(fm *fakeMetaStore) {
				fm.entries[entryKey(ref)] = &artifact.CacheEntry{Ref: ref, Digest: digest, Protocol: "oci"}
			},
			setupBlob: func(fb *fakeBlobStore) {
				fb.blobs[digest] = []byte("layer data")
			},
			wantNil: false,
		},
		{
			name: "M1: meta hit + blob missing returns nil",
			setupMeta: func(fm *fakeMetaStore) {
				fm.entries[entryKey(ref)] = &artifact.CacheEntry{Ref: ref, Digest: digest, Protocol: "oci"}
			},
			// no blob
			wantNil: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, fb, fm := newTestManager(t, nil)
			if tc.setupMeta != nil {
				tc.setupMeta(fm)
			}
			if tc.setupBlob != nil {
				tc.setupBlob(fb)
			}
			got, err := m.Lookup(context.Background(), ref)
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, digest, got.Digest)
			}
		})
	}
}

// TestLookupImmutableRejectsPinMismatch pins the invariant that was silently
// violated on every warm hit: if a ref carries a caller-supplied digest pin,
// Lookup must never hand back an entry for DIFFERENT content.
//
// The real MetadataStore keys on (protocol, name, version) and ignores
// ref.Digest, so an entry found by name can carry any digest at all. Enforcing
// the pin is one string comparison against the entry we already hold — no
// re-hash of the blob, and no weakening of §3's "CAS is never re-verified".
func TestLookupImmutableRejectsPinMismatch(t *testing.T) {
	const storedDigest = "sha256:aaa111"
	const wrongPin = "sha256:bbb222"

	// The cached entry, keyed by name — this is what the store will return.
	baseRef := artifact.ArtifactRef{Protocol: "tarball", Name: "example.com/rel", Version: "pkg.tgz"}
	m, fb, fm := newTestManager(t, nil)
	fm.entries[entryKey(baseRef)] = &artifact.CacheEntry{
		Ref: baseRef, Digest: storedDigest, Protocol: "tarball",
	}
	fb.blobs[storedDigest] = []byte("the legitimately cached bytes")

	t.Run("no pin serves the cached entry", func(t *testing.T) {
		got, err := m.Lookup(context.Background(), baseRef)
		require.NoError(t, err)
		require.NotNil(t, got, "an unpinned lookup must still hit")
		assert.Equal(t, storedDigest, got.Digest)
	})

	t.Run("matching pin serves the cached entry", func(t *testing.T) {
		ref := baseRef
		ref.Digest = storedDigest
		got, err := m.Lookup(context.Background(), ref)
		require.NoError(t, err)
		require.NotNil(t, got, "a matching pin must hit")
		assert.Equal(t, storedDigest, got.Digest)
	})

	t.Run("mismatched pin must not return the entry", func(t *testing.T) {
		ref := baseRef
		ref.Digest = wrongPin
		got, err := m.Lookup(context.Background(), ref)

		require.Error(t, err, "a mismatched pin must be an explicit error, not a silent hit")
		assert.Nil(t, got, "Lookup must never return an entry whose digest contradicts the pin")

		pe, ok := AsPinMismatchError(err)
		require.True(t, ok, "error must be a typed *PinMismatchError so handlers can map it")
		assert.Equal(t, wrongPin, pe.Want)
		assert.Equal(t, storedDigest, pe.Got)

		// A bad-faith pin must not be a cache-denial lever.
		assert.Contains(t, fb.blobs, storedDigest, "mismatched pin must not evict the blob")
		assert.Contains(t, fm.entries, entryKey(baseRef), "mismatched pin must not evict the metadata")
	})

	t.Run("Serve is gated by the same pin check", func(t *testing.T) {
		ref := baseRef
		ref.Digest = wrongPin
		rc, _, err := m.Serve(context.Background(), ref, 0, -1)
		require.Error(t, err, "Serve must not stream bytes that contradict the pin")
		assert.Nil(t, rc)
	})
}

// --------------------------------------------------------------------------
// Tests: Lookup — mutable tier
// --------------------------------------------------------------------------

func TestLookupMutable(t *testing.T) {
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "latest", Mutable: true}
	key := mutableKey(ref)
	const blobDigest = "sha256:aabbcc"

	tests := []struct {
		name    string
		entry   *artifact.MutableEntry
		hasBlob bool
		wantNil bool
	}{
		{
			name:    "no mutable entry → miss",
			wantNil: true,
		},
		{
			name: "fresh entry + blob exists → hit",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "oci", Digest: blobDigest,
				TTLSeconds: 3600, FetchedAt: time.Now(),
			},
			hasBlob: true,
			wantNil: false,
		},
		{
			name: "stale entry → miss",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "oci", Digest: blobDigest,
				TTLSeconds: 1, FetchedAt: time.Now().Add(-5 * time.Second),
			},
			hasBlob: true,
			wantNil: true,
		},
		{
			name: "fresh entry blob missing → miss",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "oci", Digest: blobDigest,
				TTLSeconds: 3600, FetchedAt: time.Now(),
			},
			hasBlob: false,
			wantNil: true,
		},
		{
			name: "ttlNeverRevalidate = always fresh",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "oci", Digest: blobDigest,
				TTLSeconds: ttlNeverRevalidate,
				FetchedAt:  time.Now().Add(-72 * time.Hour),
			},
			hasBlob: true,
			wantNil: false,
		},
		{
			name: "ttlAlwaysRevalidate = always stale",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "oci", Digest: blobDigest,
				TTLSeconds: ttlAlwaysRevalidate, FetchedAt: time.Now(),
			},
			hasBlob: true,
			wantNil: true,
		},
		{
			name: "payload-backed (no CAS blob, no Digest) → hit on payload",
			entry: &artifact.MutableEntry{
				Key: key, Protocol: "pypi", Digest: "",
				Payload:    []byte("<html>index</html>"),
				TTLSeconds: 3600, FetchedAt: time.Now(),
			},
			hasBlob: false,
			wantNil: false, // Digest="" means skip blob.Exists; payload serve
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, fb, fm := newTestManager(t, nil)
			if tc.entry != nil {
				fm.mutable[key] = tc.entry
			}
			if tc.hasBlob {
				fb.blobs[blobDigest] = []byte("blob bytes")
			}
			got, err := m.Lookup(context.Background(), ref)
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Tests: LookupStale — serve-stale-on-upstream-failure
// --------------------------------------------------------------------------

func TestLookupStaleReturnsStaleMutableEntry(t *testing.T) {
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "latest", Mutable: true}
	key := mutableKey(ref)
	const blobDigest = "sha256:stale1"

	m, fb, fm := newTestManager(t, nil)
	// Insert an expired mutable entry (1s TTL, fetched 10s ago).
	fm.mutable[key] = &artifact.MutableEntry{
		Key:        key,
		Protocol:   "oci",
		Digest:     blobDigest,
		TTLSeconds: 1,
		FetchedAt:  time.Now().Add(-10 * time.Second),
	}
	fb.blobs[blobDigest] = []byte("stale bytes")

	// Normal Lookup returns miss because TTL expired.
	entry, err := m.Lookup(context.Background(), ref)
	require.NoError(t, err)
	assert.Nil(t, entry, "Lookup must return nil for stale entry")

	// LookupStale bypasses TTL and returns the entry.
	entry, err = m.LookupStale(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, entry, "LookupStale must return stale entry for serve-stale path")
	assert.Equal(t, blobDigest, entry.Digest)
}

// --------------------------------------------------------------------------
// Tests: Store — verify-on-write promotion
// --------------------------------------------------------------------------

func TestStoreVerifyPassPromotesBlobThenMeta(t *testing.T) {
	log := &callLog{}
	m, _, _ := newTestManager(t, log)
	m.verifyFn = passVerify(artifact.TierChecksum)

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "1.25"}
	art := makeQuarantine(t, []byte("image layer bytes"))
	quarPath := art.Path

	entry, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, art.Digest, entry.Digest)
	assert.Equal(t, artifact.TierChecksum, entry.Tier)

	// Write order must be blob FIRST, metadata AFTER (fix M1).
	ops := log.Ops()
	require.Len(t, ops, 2, "expected exactly blob.Put + meta.Put")
	assert.Equal(t, "blob.Put", ops[0], "blob must precede meta")
	assert.Equal(t, "meta.Put", ops[1], "meta must follow blob")

	// Quarantine file must be removed after successful promotion.
	_, statErr := os.Stat(quarPath)
	assert.True(t, os.IsNotExist(statErr), "quarantine file should be gone after Store success")
}

func TestStoreVerifyFailCleansUpAndReturnsVerifyError(t *testing.T) {
	m, fb, _ := newTestManager(t, nil)
	m.verifyFn = failVerify()

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "evil", Version: "1.0"}
	art := makeQuarantine(t, []byte("tampered bytes"))
	quarPath := art.Path

	entry, err := m.Store(context.Background(), ref, art)
	require.Error(t, err)
	assert.Nil(t, entry)

	// Error must be typed VerifyError.
	var ve *VerifyError
	require.True(t, errors.As(err, &ve), "expected *VerifyError, got %T: %v", err, err)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status)
	assert.Contains(t, ve.Error(), "failed")

	// Quarantine file must be removed on FAIL.
	_, statErr := os.Stat(quarPath)
	assert.True(t, os.IsNotExist(statErr), "quarantine file must be removed on verify FAIL")

	// Blob must NOT have been written.
	ok, _ := fb.Exists(context.Background(), art.Digest)
	assert.False(t, ok, "blob must not be stored when verification fails")
}

func TestStoreVerifyWarnTreatedAsPass(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	m.verifyFn = warnVerify(artifact.TierTofu)

	ref := artifact.ArtifactRef{Protocol: "npm", Name: "left-pad", Version: "1.0.0"}
	art := makeQuarantine(t, []byte("package tarball"))

	entry, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)
	require.NotNil(t, entry, "WARN must be promoted like PASS")
	assert.Equal(t, artifact.TierTofu, entry.Tier)
}

func TestStoreMutableRefWritesMutableEntry(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	m.verifyFn = passVerify(artifact.TierChecksum)

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "latest", Mutable: true}
	art := makeQuarantine(t, []byte("manifest bytes"))

	_, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)

	key := mutableKey(ref)
	me, err := fm.GetMutable(context.Background(), key)
	require.NoError(t, err)
	require.NotNil(t, me, "MutableEntry must be written for mutable ref")
	assert.Equal(t, art.Digest, me.Digest)
}

func TestStoreBlobPutErrorReturnsError(t *testing.T) {
	m, fb, _ := newTestManager(t, nil)
	m.verifyFn = passVerify(artifact.TierChecksum)
	fb.putErr = errors.New("S3 error")

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "1.25"}
	art := makeQuarantine(t, []byte("data"))

	_, err := m.Store(context.Background(), ref, art)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blob put")
}

func TestStoreMetaPutErrorReturnsError(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	m.verifyFn = passVerify(artifact.TierChecksum)
	fm.putErr = errors.New("DB error")

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "1.25"}
	art := makeQuarantine(t, []byte("data"))

	_, err := m.Store(context.Background(), ref, art)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "meta put")
}

// --------------------------------------------------------------------------
// Tests: Serve
// --------------------------------------------------------------------------

func TestServeMissReturnsErrCacheMiss(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "nginx", Version: "1.25", Digest: "sha256:x"}

	_, _, err := m.Serve(context.Background(), ref, 0, -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCacheMiss)
}

func TestServeHitReturnsFullBlob(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	const digest = "sha256:cafe"
	content := []byte("hello cache world")
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "v1", Digest: digest}

	fb.blobs[digest] = content
	fm.entries[entryKey(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Protocol: "oci",
		Size:     int64(len(content)),
	}

	rc, entry, err := m.Serve(context.Background(), ref, 0, -1)
	require.NoError(t, err)
	require.NotNil(t, rc)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, digest, entry.Digest)
}

func TestServeRangeRead(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	const digest = "sha256:range"
	content := []byte("0123456789")
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "v2", Digest: digest}

	fb.blobs[digest] = content
	fm.entries[entryKey(ref)] = &artifact.CacheEntry{Ref: ref, Digest: digest, Protocol: "oci", Size: int64(len(content))}

	// bytes [3, 3+4) = "3456"
	rc, _, err := m.Serve(context.Background(), ref, 3, 4)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, []byte("3456"), got)
}

func TestServeMutablePayloadEntry(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	ref := artifact.ArtifactRef{Protocol: "pypi", Name: "requests", Version: "simple", Mutable: true}
	key := mutableKey(ref)
	payload := []byte("<html>simple index for requests</html>")

	fm.mutable[key] = &artifact.MutableEntry{
		Key:        key,
		Protocol:   "pypi",
		Digest:     "", // payload-backed; no CAS blob
		Payload:    payload,
		TTLSeconds: ttlNeverRevalidate,
		FetchedAt:  time.Now(),
	}

	rc, entry, err := m.Serve(context.Background(), ref, 0, -1)
	require.NoError(t, err)
	require.NotNil(t, rc)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, payload, got)
	assert.NotNil(t, entry)
}

// --------------------------------------------------------------------------
// Tests: ServeEntry — serve bytes for an entry the caller already holds
// --------------------------------------------------------------------------

// TestServeEntryServesStaleMutableEntry is the regression test for the
// serve-stale-on-upstream-failure bug (DESIGN-REVIEW §2 H1): Serve is
// freshness-gated and MUST miss on a stale mutable ref, while ServeEntry MUST
// still render the entry LookupStale returned for that same ref. Handlers rely
// on exactly this asymmetry to keep serving an index when the upstream is down.
func TestServeEntryServesStaleMutableEntry(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	const digest = "sha256:stale"
	content := []byte("stale but usable index")
	ref := artifact.ArtifactRef{Protocol: "npm", Name: "react", Version: "packument", Mutable: true}
	key := mutableKey(ref)

	fb.blobs[digest] = content
	fm.mutable[key] = &artifact.MutableEntry{
		Key:        key,
		Protocol:   "npm",
		Digest:     digest,
		TTLSeconds: ttlAlwaysRevalidate, // 0 → always expired
		FetchedAt:  time.Now(),
	}

	// Serve is freshness-gated: it re-runs Lookup and must report a miss.
	_, _, err := m.Serve(context.Background(), ref, 0, -1)
	assert.ErrorIs(t, err, ErrCacheMiss,
		"Serve must not return stale mutable content")

	// LookupStale surfaces the entry the handler will hold...
	stale, err := m.LookupStale(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, stale)

	// ...and ServeEntry must render its bytes despite the expired TTL.
	rc, err := m.ServeEntry(context.Background(), stale, 0, -1)
	require.NoError(t, err)
	require.NotNil(t, rc)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got, "stale entry bytes must be served verbatim")
}

// TestServeEntryNilEntryIsMiss — a nil entry is a miss, never a panic.
func TestServeEntryNilEntryIsMiss(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	_, err := m.ServeEntry(context.Background(), nil, 0, -1)
	assert.ErrorIs(t, err, ErrCacheMiss)
}

// TestServeEntryRangeRead — Range semantics (fix M2) hold on the entry path.
func TestServeEntryRangeRead(t *testing.T) {
	m, fb, _ := newTestManager(t, nil)
	const digest = "sha256:entryrange"
	fb.blobs[digest] = []byte("0123456789")

	rc, err := m.ServeEntry(context.Background(),
		&artifact.CacheEntry{Digest: digest, Protocol: "oci"}, 3, 4)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, []byte("3456"), got)
}

// TestServeEntryPayloadBackedStaleEntry — payload-backed mutable entries (no CAS
// blob) are served through the entry path too, using entry.Ref for the payload
// lookup.
func TestServeEntryPayloadBackedStaleEntry(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	ref := artifact.ArtifactRef{Protocol: "pypi", Name: "flask", Version: "simple", Mutable: true}
	key := mutableKey(ref)
	payload := []byte("<html>stale simple index</html>")

	fm.mutable[key] = &artifact.MutableEntry{
		Key:        key,
		Protocol:   "pypi",
		Digest:     "", // payload-backed
		Payload:    payload,
		TTLSeconds: ttlAlwaysRevalidate, // stale
		FetchedAt:  time.Now(),
	}

	stale, err := m.LookupStale(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, stale)

	rc, err := m.ServeEntry(context.Background(), stale, 0, -1)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, payload, got)
}

// --------------------------------------------------------------------------
// Tests: Quarantine
// --------------------------------------------------------------------------

func TestQuarantineStreamsAndComputesDigest(t *testing.T) {
	dir := t.TempDir()
	content := []byte("artifact bytes for quarantine streaming test")

	art, cleanup, err := Quarantine(context.Background(), dir, bytes.NewReader(content), artifact.UpstreamMeta{Upstream: "origin"})
	require.NoError(t, err)
	require.NotNil(t, art)
	require.NotNil(t, cleanup)

	// Digest must carry sha256: prefix.
	assert.True(t, strings.HasPrefix(art.Digest, "sha256:"), "digest must have sha256: prefix")
	assert.Equal(t, int64(len(content)), art.Size)

	// File must contain the original bytes.
	got, err := os.ReadFile(art.Path)
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// cleanup must remove the file.
	cleanup()
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove quarantine file")
}

func TestQuarantineCleanupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	_, cleanup, err := Quarantine(context.Background(), dir, bytes.NewReader([]byte("x")), artifact.UpstreamMeta{})
	require.NoError(t, err)
	cleanup()
	cleanup() // second call must not panic
}

// --------------------------------------------------------------------------
// Tests: AsVerifyError / VerifyError.Error
// --------------------------------------------------------------------------

func TestVerifyErrorUnwrap(t *testing.T) {
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "v1"}
	ve := &VerifyError{Ref: ref, Result: artifact.Result{Status: artifact.StatusFail, Tier: artifact.TierChecksum, Message: "bad digest"}}

	assert.Contains(t, ve.Error(), "failed")
	assert.Contains(t, ve.Error(), "oci")

	got, ok := AsVerifyError(ve)
	assert.True(t, ok)
	assert.Equal(t, ve, got)

	_, ok = AsVerifyError(errors.New("unrelated"))
	assert.False(t, ok)
}

// --------------------------------------------------------------------------
// Tests: Store round-trip (Quarantine → Store → Lookup → Serve)
// --------------------------------------------------------------------------

func TestFullRoundTrip(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	m.verifyFn = passVerify(artifact.TierTofu)

	dir := t.TempDir()
	content := []byte("full round-trip blob content")
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "app", Version: "sha256:roundtrip"}

	// 1. Quarantine upstream response.
	art, cleanup, err := Quarantine(context.Background(), dir, bytes.NewReader(content), artifact.UpstreamMeta{Upstream: "daocloud"})
	require.NoError(t, err)
	defer cleanup()

	// 2. Fill ref.Digest from quarantine artifact.
	ref.Digest = art.Digest

	// 3. Store (promote).
	entry, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, art.Digest, entry.Digest)
	assert.Equal(t, artifact.TierTofu, entry.Tier)

	// 4. Lookup — must be a hit now.
	got, err := m.Lookup(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	// 5. Serve — must return original content.
	rc, _, err := m.Serve(context.Background(), ref, 0, -1)
	require.NoError(t, err)
	defer rc.Close()
	served, _ := io.ReadAll(rc)
	assert.Equal(t, content, served)
}
