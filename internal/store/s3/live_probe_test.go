package s3

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Diagnostic for an S3-compatible store that answers 403.
//
// HeadObject has no response body by protocol, so the SDK cannot read the store's
// error CODE and reports a bare "Forbidden" — which cannot distinguish an
// authorization problem (AccessDenied) from a signing one (SignatureDoesNotMatch).
// ListObjectsV2 and GetObject do return a body, so they surface the real code.
//
//	SPECULA_LIVE_S3=1 \
//	SPECULA_LIVE_S3_BUCKET=… SPECULA_LIVE_S3_ENDPOINT=https://oss-<region>.aliyuncs.com \
//	SPECULA_LIVE_S3_REGION=… SPECULA_LIVE_S3_AK=… SPECULA_LIVE_S3_SK=… \
//	go test ./internal/store/s3/ -run LiveS3Diagnose -v
func TestLiveS3Diagnose(t *testing.T) {
	if os.Getenv("SPECULA_LIVE_S3") == "" {
		t.Skip("set SPECULA_LIVE_S3=1 plus BUCKET/ENDPOINT/REGION/AK/SK to run")
	}
	bucket := os.Getenv("SPECULA_LIVE_S3_BUCKET")
	endpoint := os.Getenv("SPECULA_LIVE_S3_ENDPOINT")
	region := os.Getenv("SPECULA_LIVE_S3_REGION")
	ak, sk := os.Getenv("SPECULA_LIVE_S3_AK"), os.Getenv("SPECULA_LIVE_S3_SK")
	if bucket == "" || endpoint == "" || ak == "" || sk == "" {
		t.Fatal("BUCKET/ENDPOINT/AK/SK are all required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(region),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")))
	if err != nil {
		t.Fatal(err)
	}
	cli := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = false
	})

	// ListObjectsV2 — returns a body, so the real error code is visible.
	_, lerr := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket), MaxKeys: aws.Int32(1),
	})
	t.Logf("ListObjectsV2: %v", lerr)

	// GetObject on a key that will not exist: NoSuchKey means auth+signing are FINE.
	_, gerr := cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("specula-probe-does-not-exist"),
	})
	t.Logf("GetObject:     %v", gerr)

	// HeadObject — the call Specula actually failed on, for comparison.
	_, herr := cli.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("specula-probe-does-not-exist"),
	})
	t.Logf("HeadObject:    %v", herr)
}
