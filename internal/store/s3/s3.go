// Package s3 provides a content-addressed BlobStore backed by S3-compatible
// object storage (aws-sdk-go-v2, path-style; works with R2/MinIO/OSS).
//
// # Key layout
//
//	blobs/<algo>/<hex>  — canonical CAS objects (immutable, permanent)
//	.tmp/<random-hex>   — ephemeral upload staging area; promoted to the
//	                      canonical key via server-side CopyObject so the
//	                      canonical key is only visible after a fully
//	                      committed upload (atomic, similar to rename(2)).
//
// # Put semantics
//
// Put streams the body into a staging (.tmp) key, verifies the uploaded size
// via HeadObject, promotes via CopyObject to the canonical CAS key, then
// deletes the staging object.  If any step fails the staging object is removed
// and no canonical key is written.
//
// # UsageBytes
//
// Iterates all blobs/ objects via ListObjectsV2 and sums their sizes.  The
// result is cached for 60 seconds to amortise list calls on busy paths.
package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ivanzzeth/specula/internal/store/blob"
)

// s3API is the subset of *s3.Client methods used by S3Driver.
// Defining it as an interface enables in-process fakes in unit tests with no
// network access.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CopyObject(ctx context.Context, params *s3.CopyObjectInput, optFns ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Verify that *s3.Client satisfies s3API at compile time.
var _ s3API = (*s3.Client)(nil)

const usageCacheTTL = 60 * time.Second

// S3Driver stores blobs in an S3-compatible bucket using content-addressed
// keys.  Path-style access is always enabled for MinIO/R2/OSS compatibility.
type S3Driver struct {
	client        s3API
	bucket        string
	mu            sync.Mutex
	usageCached   int64
	usageCachedAt time.Time
}

// Compile-time assertion that S3Driver satisfies blob.BlobStore.
var _ blob.BlobStore = (*S3Driver)(nil)

// NewS3Driver builds an S3Driver from the ambient AWS configuration (env vars,
// shared config file, IRSA/EC2 metadata, etc.) targeting bucket.
func NewS3Driver(ctx context.Context, bucket string) (*S3Driver, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}
	c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	return &S3Driver{client: c, bucket: bucket}, nil
}

// newWithClient constructs an S3Driver backed by the given client.
// Intended for unit tests; not exported.
func newWithClient(client s3API, bucket string) *S3Driver {
	return &S3Driver{client: client, bucket: bucket}
}

// StaticCredentials builds a static credentials provider suitable for
// MinIO/OSS/R2 deployments that use fixed access keys.
func StaticCredentials(accessKeyID, secretAccessKey, sessionToken string) aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
}

// blobKey converts a CAS digest (e.g. "sha256:abcdef…") to an S3 object key
// ("blobs/sha256/abcdef…").  Unknown or bare digests land under "blobs/unknown/".
func blobKey(digest string) string {
	if algo, h, ok := strings.Cut(digest, ":"); ok {
		return "blobs/" + algo + "/" + h
	}
	return "blobs/unknown/" + digest
}

// tmpKey returns a unique staging key under ".tmp/".
func (d *S3Driver) tmpKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("s3: generate staging key: %w", err)
	}
	return ".tmp/" + hex.EncodeToString(b[:]), nil
}

// isNotFound reports whether err is an S3 HTTP-404 response.
func isNotFound(err error) bool {
	var nf *s3types.NotFound
	var nsk *s3types.NoSuchKey
	return errors.As(err, &nf) || errors.As(err, &nsk)
}

// parseTotalFromContentRange extracts the full object size from a
// Content-Range header value, e.g. "bytes 0-999/12345" → 12345.
func parseTotalFromContentRange(cr string) (int64, error) {
	idx := strings.LastIndex(cr, "/")
	if idx < 0 {
		return 0, fmt.Errorf("s3: invalid Content-Range %q: missing /", cr)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[idx+1:]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("s3: parse Content-Range total %q: %w", cr, err)
	}
	return n, nil
}

// Get returns a reader for the byte range [offset, offset+length) of the blob
// plus the full object size.  Pass length < 0 to read from offset to EOF.
func (d *S3Driver) Get(ctx context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error) {
	key := blobKey(digest)

	input := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}

	// Build an HTTP Range header when a sub-range is requested.
	needsRange := offset > 0 || length >= 0
	if needsRange {
		var rng string
		if length < 0 {
			rng = fmt.Sprintf("bytes=%d-", offset)
		} else {
			rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		}
		input.Range = aws.String(rng)
	}

	out, err := d.client.GetObject(ctx, input)
	if err != nil {
		return nil, 0, fmt.Errorf("s3 GetObject %s: %w", key, err)
	}

	// Determine total (full) object size.
	var totalSize int64
	if needsRange && out.ContentRange != nil {
		totalSize, err = parseTotalFromContentRange(aws.ToString(out.ContentRange))
		if err != nil {
			_ = out.Body.Close()
			return nil, 0, err
		}
	} else if out.ContentLength != nil {
		totalSize = aws.ToInt64(out.ContentLength)
	}

	return out.Body, totalSize, nil
}

// Put stores r (size bytes) under digest.  It is idempotent: if the canonical
// CAS key already exists the call returns immediately without reading r.
//
// Upload sequence for atomicity:
//  1. Stream body to a random staging key under ".tmp/".
//  2. HeadObject to verify the uploaded byte count.
//  3. CopyObject to promote to the canonical CAS key.
//  4. DeleteObject the staging key (best effort; orphans are harmless).
func (d *S3Driver) Put(ctx context.Context, digest string, r io.Reader, size int64) error {
	key := blobKey(digest)

	// Fast idempotent path: canonical key already exists.
	exists, err := d.Exists(ctx, digest)
	if err != nil {
		return fmt.Errorf("s3 Put exists-check %s: %w", digest, err)
	}
	if exists {
		return nil
	}

	tmpKey, err := d.tmpKey()
	if err != nil {
		return err
	}

	// 1. Stream to staging key.
	if _, err = d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(d.bucket),
		Key:           aws.String(tmpKey),
		Body:          r,
		ContentLength: aws.Int64(size),
	}); err != nil {
		return fmt.Errorf("s3 PutObject staging %s: %w", tmpKey, err)
	}

	// 2. Verify size via HeadObject.
	head, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(tmpKey),
	})
	if err != nil {
		d.deleteIgnoreErr(ctx, tmpKey)
		return fmt.Errorf("s3 HeadObject staging %s: %w", tmpKey, err)
	}
	if head.ContentLength != nil && aws.ToInt64(head.ContentLength) != size {
		d.deleteIgnoreErr(ctx, tmpKey)
		return fmt.Errorf("s3 Put %s: size mismatch: uploaded %d, expected %d",
			digest, aws.ToInt64(head.ContentLength), size)
	}

	// 3. Atomic promotion: server-side copy to canonical CAS key.
	// CopySource format expected by S3: "{bucket}/{key}"
	copySource := d.bucket + "/" + tmpKey
	if _, err = d.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(d.bucket),
		Key:        aws.String(key),
		CopySource: aws.String(copySource),
	}); err != nil {
		d.deleteIgnoreErr(ctx, tmpKey)
		return fmt.Errorf("s3 CopyObject %s → %s: %w", tmpKey, key, err)
	}

	// 4. Remove staging key (best effort; orphaned .tmp objects are harmless).
	d.deleteIgnoreErr(ctx, tmpKey)
	return nil
}

// Exists reports whether a blob with the given digest exists in the bucket.
func (d *S3Driver) Exists(ctx context.Context, digest string) (bool, error) {
	key := blobKey(digest)
	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 HeadObject %s: %w", key, err)
	}
	return true, nil
}

// Delete removes the CAS blob for digest.
func (d *S3Driver) Delete(ctx context.Context, digest string) error {
	key := blobKey(digest)
	if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3 DeleteObject %s: %w", key, err)
	}
	return nil
}

// UsageBytes returns the total stored bytes for all blobs in the bucket.
// The result is cached for 60 seconds to avoid hammering the list API on hot
// serve paths.
func (d *S3Driver) UsageBytes(ctx context.Context) (int64, error) {
	d.mu.Lock()
	if !d.usageCachedAt.IsZero() && time.Since(d.usageCachedAt) < usageCacheTTL {
		v := d.usageCached
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()

	total, err := d.sumBlobBytes(ctx)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	d.usageCached = total
	d.usageCachedAt = time.Now()
	d.mu.Unlock()

	return total, nil
}

// sumBlobBytes paginates ListObjectsV2 under the "blobs/" prefix and sums sizes.
func (d *S3Driver) sumBlobBytes(ctx context.Context) (int64, error) {
	var total int64
	var token *string

	for {
		out, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(d.bucket),
			Prefix:            aws.String("blobs/"),
			ContinuationToken: token,
		})
		if err != nil {
			return 0, fmt.Errorf("s3 ListObjectsV2: %w", err)
		}
		for _, obj := range out.Contents {
			if obj.Size != nil {
				total += aws.ToInt64(obj.Size)
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return total, nil
}

// deleteIgnoreErr deletes key from the bucket, silently swallowing any error.
// Used for best-effort cleanup of staging objects after Put failures.
func (d *S3Driver) deleteIgnoreErr(ctx context.Context, key string) {
	_, _ = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
}
