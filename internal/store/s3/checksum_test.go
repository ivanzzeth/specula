package s3

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// aws-sdk-go-v2 defaults RequestChecksumCalculation to WhenSupported, which makes
// PutObject send `aws-chunked` with a trailing checksum and an unsigned payload.
// Alibaba Cloud OSS rejects that outright:
//
//	PutObject: 400 NotImplemented:
//	  Aws MultiChunkedEncoding STREAMING-UNSIGNED-PAYLOAD-TRAILER is not supported.
//
// Observed on a real OSS bucket after auth was already working, so EVERY cache write
// failed while reads and listing were fine. Cloudflare R2 and older MinIO builds reject
// it too, which would make the s3 driver unusable on essentially every non-AWS store —
// i.e. on most of the deployments this driver exists for.
//
// WhenRequired keeps checksums for the operations that mandate them and drops the
// streaming-trailer encoding everywhere else.
func TestLoadOptionsDisableOpportunisticChecksums(t *testing.T) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		buildLoadOptions(S3Config{Region: "cn-chengdu", AccessKeyID: "ak", SecretAccessKey: "sk"})...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatalf("RequestChecksumCalculation = %v, want WhenRequired — WhenSupported makes "+
			"PutObject use a trailer encoding that OSS/R2 reject with NotImplemented",
			cfg.RequestChecksumCalculation)
	}
	if cfg.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Fatalf("ResponseChecksumValidation = %v, want WhenRequired",
			cfg.ResponseChecksumValidation)
	}
}

// The setting must not depend on region or credentials being present — an
// instance-role deployment needs it just as much.
func TestLoadOptionsChecksumSettingIsUnconditional(t *testing.T) {
	for _, c := range []S3Config{
		{},
		{Region: "us-east-1"},
		{AccessKeyID: "ak", SecretAccessKey: "sk"},
	} {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), buildLoadOptions(c)...)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
			t.Fatalf("config %+v: checksum calculation not pinned", c)
		}
	}
}
