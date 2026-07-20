// Package s3 re-exports the S3-compatible CAS blob driver.
//
// This package pulls in the AWS SDK. Import it only when you need S3:
//
//	import "github.com/ivanzzeth/specula/pkg/store/s3"
//
// Blank-import also registers the "s3" driver name with pkg/store/blob registry:
//
//	import _ "github.com/ivanzzeth/specula/pkg/store/s3"
package s3

import (
	"context"

	ints3 "github.com/ivanzzeth/specula/internal/store/s3"

	"github.com/ivanzzeth/specula/pkg/store/blob"
)

func init() {
	blob.Register("s3", func(cfg map[string]string) (blob.BlobStore, error) {
		return New(context.Background(), Config{
			Bucket:          cfg["bucket"],
			Endpoint:        cfg["endpoint"],
			Region:          cfg["region"],
			AccessKeyID:     cfg["access_key_id"],
			SecretAccessKey: cfg["secret_access_key"],
			UsePathStyle:    cfg["use_path_style"] == "true" || cfg["use_path_style"] == "1",
		})
	})
}

type (
	S3Driver = ints3.S3Driver
	Config   = ints3.S3Config
)

// New constructs an S3-backed BlobStore.
func New(ctx context.Context, cfg Config) (blob.BlobStore, error) {
	return ints3.NewS3Driver(ctx, cfg)
}

// NewS3Driver is an alias for New retained for parity with internal API.
func NewS3Driver(ctx context.Context, cfg Config) (*S3Driver, error) {
	return ints3.NewS3Driver(ctx, cfg)
}
