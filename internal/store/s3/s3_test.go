package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// In-memory fake s3API — no network access required.
// ---------------------------------------------------------------------------

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // key → content
	bucket  string
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{objects: make(map[string][]byte), bucket: bucket}
}

func (f *fakeS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.objects[aws.ToString(params.Key)] = data
	f.mu.Unlock()
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(params.Key)
	f.mu.Lock()
	data, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}

	total := int64(len(data))
	content := data
	var contentRange *string

	if rng := aws.ToString(params.Range); rng != "" {
		// Parse "bytes=start-end" or "bytes=start-"
		rng = strings.TrimPrefix(rng, "bytes=")
		parts := strings.SplitN(rng, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("fake: bad Range header: %s", rng)
		}
		start := int64(0)
		if parts[0] != "" {
			fmt.Sscanf(parts[0], "%d", &start)
		}
		end := total - 1
		if parts[1] != "" {
			fmt.Sscanf(parts[1], "%d", &end)
		}
		if start < 0 || end >= total || start > end {
			return nil, fmt.Errorf("fake: Range out of bounds: %d-%d/%d", start, end, total)
		}
		content = data[start : end+1]
		cr := fmt.Sprintf("bytes %d-%d/%d", start, end, total)
		contentRange = aws.String(cr)
	}

	cl := int64(len(content))
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(content)),
		ContentLength: aws.Int64(cl),
		ContentRange:  contentRange,
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	key := aws.ToString(params.Key)
	f.mu.Lock()
	data, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NotFound{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data)))}, nil
}

func (f *fakeS3) CopyObject(_ context.Context, params *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	// CopySource is "{bucket}/{key}"; strip the bucket prefix to get the key.
	src := aws.ToString(params.CopySource)
	prefix := f.bucket + "/"
	if strings.HasPrefix(src, prefix) {
		src = src[len(prefix):]
	} else if _, after, ok := strings.Cut(src, "/"); ok {
		src = after
	}

	f.mu.Lock()
	data, ok := f.objects[src]
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}

	dst := aws.ToString(params.Key)
	f.mu.Lock()
	f.objects[dst] = data
	f.mu.Unlock()

	return &s3.CopyObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(params.Key)
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(params.Prefix)
	f.mu.Lock()
	defer f.mu.Unlock()

	var contents []s3types.Object
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			size := int64(len(v))
			contents = append(contents, s3types.Object{
				Key:  aws.String(k),
				Size: aws.Int64(size),
			})
		}
	}

	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: aws.Bool(false),
	}, nil
}

// helper: count keys in fake that start with prefix (used to verify .tmp cleanup)
func (f *fakeS3) countKeysWithPrefix(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// Helper: new driver backed by the in-memory fake.
// ---------------------------------------------------------------------------

const testBucket = "specula-test"

func newTestDriver(t *testing.T) (*S3Driver, *fakeS3) {
	t.Helper()
	fake := newFakeS3(testBucket)
	drv := newWithClient(fake, testBucket)
	return drv, fake
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBlobKey(t *testing.T) {
	cases := []struct {
		digest string
		want   string
	}{
		{"sha256:abc123", "blobs/sha256/abc123"},
		{"sha512:deadbeef", "blobs/sha512/deadbeef"},
		{"bare-no-colon", "blobs/unknown/bare-no-colon"},
		{"", "blobs/unknown/"},
	}
	for _, tc := range cases {
		got := blobKey(tc.digest)
		assert.Equal(t, tc.want, got, "blobKey(%q)", tc.digest)
	}
}

func TestParseTotalFromContentRange(t *testing.T) {
	cases := []struct {
		cr      string
		want    int64
		wantErr bool
	}{
		{"bytes 0-999/12345", 12345, false},
		{"bytes 100-199/500", 500, false},
		{"bytes 0-0/1", 1, false},
		{"no-slash-here", 0, true},
		{"bytes 0-9/notanumber", 0, true},
	}
	for _, tc := range cases {
		got, err := parseTotalFromContentRange(tc.cr)
		if tc.wantErr {
			assert.Error(t, err, "expected error for %q", tc.cr)
		} else {
			require.NoError(t, err, "unexpected error for %q", tc.cr)
			assert.Equal(t, tc.want, got, "total for %q", tc.cr)
		}
	}
}

func TestExistsNotFound(t *testing.T) {
	drv, _ := newTestDriver(t)
	ok, err := drv.Exists(context.Background(), "sha256:missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPutAndExists(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	digest := "sha256:cafebabe"
	content := []byte("hello specula")

	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	ok, err := drv.Exists(ctx, digest)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestPutIdempotent(t *testing.T) {
	drv, fake := newTestDriver(t)
	ctx := context.Background()

	digest := "sha256:idempotent"
	content := []byte("same bytes")

	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	// Second put with same digest should be a no-op (Exists returns true).
	// The fake PutObject would still succeed, but we verify by checking the
	// canonical key holds the original content.
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	key := blobKey(digest)
	fake.mu.Lock()
	stored := fake.objects[key]
	fake.mu.Unlock()
	assert.Equal(t, content, stored)
}

func TestPutCleansStagingKey(t *testing.T) {
	drv, fake := newTestDriver(t)
	ctx := context.Background()

	content := []byte("check tmp cleanup")
	require.NoError(t, drv.Put(ctx, "sha256:cleanup", bytes.NewReader(content), int64(len(content))))

	// No .tmp/ keys should remain after a successful Put.
	assert.Equal(t, 0, fake.countKeysWithPrefix(".tmp/"))
}

func TestGetFullObject(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	content := []byte("full object content for get")
	digest := "sha256:fullget"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	rc, size, err := drv.Get(ctx, digest, 0, -1)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, int64(len(content)), size)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestGetWithOffset(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	content := []byte("0123456789abcdef")
	digest := "sha256:rangeoffset"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	// Read from offset 4 to end.
	rc, totalSize, err := drv.Get(ctx, digest, 4, -1)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, int64(len(content)), totalSize, "should return full object size")

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content[4:], got)
}

func TestGetWithOffsetAndLength(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	content := []byte("abcdefghijklmnop")
	digest := "sha256:rangeboth"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	// Read bytes [2, 7) → "cdefg".
	rc, totalSize, err := drv.Get(ctx, digest, 2, 5)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, int64(len(content)), totalSize, "should return full object size")

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content[2:7], got)
}

func TestGetWithLengthFromZero(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	content := []byte("abcdefghijklmnop")
	digest := "sha256:rangefromzero"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	// offset=0, length=8 → "bytes=0-7"
	rc, totalSize, err := drv.Get(ctx, digest, 0, 8)
	require.NoError(t, err)
	defer rc.Close()

	assert.Equal(t, int64(len(content)), totalSize)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content[:8], got)
}

func TestGetMissing(t *testing.T) {
	drv, _ := newTestDriver(t)
	rc, _, err := drv.Get(context.Background(), "sha256:nothere", 0, -1)
	assert.Error(t, err)
	assert.Nil(t, rc)
}

func TestDelete(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	content := []byte("to be deleted")
	digest := "sha256:deleteme"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	ok, err := drv.Exists(ctx, digest)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, drv.Delete(ctx, digest))

	ok, err = drv.Exists(ctx, digest)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestUsageBytes(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	blobs := map[string][]byte{
		"sha256:a1": []byte("hello"),         // 5 bytes
		"sha256:b2": []byte("world!!"),       // 7 bytes
		"sha256:c3": []byte("specula cache"), // 13 bytes
	}
	for digest, content := range blobs {
		require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))
	}

	total, err := drv.UsageBytes(ctx)
	require.NoError(t, err)

	expected := int64(5 + 7 + 13)
	assert.Equal(t, expected, total)
}

func TestUsageBytesCached(t *testing.T) {
	drv, fake := newTestDriver(t)
	ctx := context.Background()

	content := []byte("initial")
	digest := "sha256:cached"
	require.NoError(t, drv.Put(ctx, digest, bytes.NewReader(content), int64(len(content))))

	// Prime the cache.
	total1, err := drv.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), total1)

	// Add another blob directly to the fake (bypass driver to avoid cache invalidation).
	fake.mu.Lock()
	fake.objects[blobKey("sha256:sneaky")] = []byte("extra bytes")
	fake.mu.Unlock()

	// Second call should return the cached (stale) value because usageCacheTTL hasn't elapsed.
	total2, err := drv.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Equal(t, total1, total2, "expected cached value to be returned within TTL")
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"NotFound", &s3types.NotFound{}, true},
		{"NoSuchKey", &s3types.NoSuchKey{}, true},
		{"other error", fmt.Errorf("connection refused"), false},
	}
	for _, tc := range cases {
		got := isNotFound(tc.err)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestRoundTripMultipleBlobs(t *testing.T) {
	drv, _ := newTestDriver(t)
	ctx := context.Background()

	type blob struct {
		digest  string
		content []byte
	}
	blobs := []blob{
		{"sha256:aa", []byte("blob one")},
		{"sha256:bb", []byte("blob two")},
		{"sha256:cc", []byte("blob three")},
	}

	// Store all.
	for _, b := range blobs {
		require.NoError(t, drv.Put(ctx, b.digest, bytes.NewReader(b.content), int64(len(b.content))))
	}

	// Read all back and verify.
	for _, b := range blobs {
		rc, size, err := drv.Get(ctx, b.digest, 0, -1)
		require.NoError(t, err, "Get %s", b.digest)
		defer rc.Close()

		assert.Equal(t, int64(len(b.content)), size, "size for %s", b.digest)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, b.content, got, "content for %s", b.digest)
	}
}
