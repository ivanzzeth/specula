package config_test

// validate_extra_test.go covers the Validate branches not reached by the
// table-driven suite in config_test.go.  Each subtest modifies one field of
// minimalCfg to trigger a specific error message, confirming that the
// validation rule fires and produces an intelligible diagnostic.

import (
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
)

// validCfg returns a minimal, fully-valid *Config built from struct literals
// (no YAML or koanf involved) so the tests are explicit about what is being
// changed and why.
func validCfg() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			DataPlaneAddr:    ":5000",
			ControlPlaneAddr: ":8080",
		},
		Storage: config.StorageConfig{
			Blob: config.BlobStorageConfig{
				Driver: "local",
				Local:  config.LocalBlobConfig{Root: "/tmp/blobs"},
			},
			Meta: config.MetaStorageConfig{
				Driver: "sqlite",
				DSN:    "/tmp/meta.db",
			},
		},
		Protocols: map[string]config.ProtocolConfig{
			"oci": {
				Upstreams: []config.UpstreamConfig{{
					Name:    "docker-hub",
					BaseURL: "https://registry-1.docker.io",
				}},
			},
		},
	}
}

// TestValidate_Valid confirms validCfg itself passes validation, so test
// failures below are actually caused by the deliberate mutation, not the base.
func TestValidate_Valid(t *testing.T) {
	if err := config.Validate(validCfg()); err != nil {
		t.Fatalf("validCfg: expected nil error, got %v", err)
	}
}

// TestValidate_UpstreamNameEmpty covers the
// "protocols.<name>.upstreams[N].name: must not be empty" rule.
func TestValidate_UpstreamNameEmpty(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Upstreams[0].Name = ""
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "upstreams[0].name: must not be empty")
}

// TestValidate_MetaDSNEmpty covers the
// "storage.meta.dsn: must not be empty" rule for an otherwise-valid driver.
func TestValidate_MetaDSNEmpty(t *testing.T) {
	cfg := validCfg()
	cfg.Storage.Meta.DSN = ""

	err := config.Validate(cfg)
	assertValidationErr(t, err, "storage.meta.dsn: must not be empty")
}

// TestValidate_MetaDSNEmptyPostgres is the same rule but for "postgres" driver.
func TestValidate_MetaDSNEmptyPostgres(t *testing.T) {
	cfg := validCfg()
	cfg.Storage.Meta.Driver = "postgres"
	cfg.Storage.Meta.DSN = ""

	err := config.Validate(cfg)
	assertValidationErr(t, err, "storage.meta.dsn: must not be empty")
}

// TestValidate_CosignTlogTrue covers the
// "protocols.<name>.verification.cosign.tlog: must be false" rule.
// DESIGN-REVIEW §1.1: keyless/tlog verification is unsupported (CN-offline).
func TestValidate_CosignTlogTrue(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.Cosign = &config.CosignConfig{
		Tlog: true,
		Keys: []string{"/etc/specula/cosign.pub"},
	}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "cosign.tlog: must be false")
}

// TestValidate_CosignKeysEmpty covers the
// "protocols.<name>.verification.cosign.keys: at least one public key" rule.
func TestValidate_CosignKeysEmpty(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.Cosign = &config.CosignConfig{
		Tlog: false,
		Keys: nil, // no keys
	}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "cosign.keys: at least one public key")
}

// TestValidate_MutableTTLBelowMinusOne covers the
// "protocols.<name>.mutable_ttl_seconds: must be >= -1" rule.
// PRD §6: -1 = TTLNeverRevalidate is the lower bound sentinel.
func TestValidate_MutableTTLBelowMinusOne(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.MutableTTLSeconds = -2
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "mutable_ttl_seconds: must be >= -1")
}

// TestValidate_SumDBPolicyOff covers the
// "protocols.<name>.sumdb.policy: … GOSUMDB=off is forbidden" rule.
// DESIGN-REVIEW H5: GOSUMDB must never be disabled.
func TestValidate_SumDBPolicyOff(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.SumDB = &config.SumDBConfig{Policy: "off"}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "GOSUMDB=off is forbidden")
}

// TestValidate_GPGPolicyInvalid covers the
// "protocols.<name>.verification.gpg.policy: must be "enforce" or "warn"" rule.
func TestValidate_GPGPolicyInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.GPG = &config.GPGConfig{Policy: "ignore"}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "verification.gpg.policy: must be")
}

// TestValidate_ProvenancePolicyInvalid covers the
// "protocols.<name>.verification.provenance.policy: must be …" rule.
func TestValidate_ProvenancePolicyInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.Provenance = &config.ProvenanceConfig{Policy: "skip"}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "verification.provenance.policy: must be")
}

// TestValidate_SignedRefsPolicyInvalid covers the
// "protocols.<name>.verification.signed_refs.policy: must be …" rule.
func TestValidate_SignedRefsPolicyInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.SignedRefs = &config.SignedRefsConfig{Policy: "none"}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "verification.signed_refs.policy: must be")
}

// TestValidate_TofuPolicyInvalid covers the
// "protocols.<name>.verification.tofu: must be "enforce" or "warn"" rule.
func TestValidate_TofuPolicyInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.Tofu = "allow"
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "verification.tofu: must be")
}

// TestValidate_OnPrivateDownInvalid covers the
// "protocols.<name>.verification.dependency_confusion.on_private_down:
//
//	must be "fail_closed" or "serve_stale"" rule.
func TestValidate_OnPrivateDownInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.DependencyConfusion = &config.DependencyConfusionConfig{
		OnPrivateDown: "fallback",
	}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "on_private_down: must be")
}

// TestValidate_GitSyncStaleAfterInvalid covers the
// "protocols.<name>.git.sync_stale_after: invalid duration" rule.
func TestValidate_GitSyncStaleAfterInvalid(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Git = &config.GitConfig{SyncStaleAfter: "not-a-duration"}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "sync_stale_after: invalid duration")
}

// TestValidate_ConsensusBlockQuorumZero covers the structured ConsensusConfig
// check: "protocols.<name>.verification.consensus.quorum: must be >= 1".
// This fires independently of whether "consensus" appears in Tiers (the flat
// Quorum check is a separate guard for the Tiers-based path).
func TestValidate_ConsensusBlockQuorumZero(t *testing.T) {
	cfg := validCfg()
	proto := cfg.Protocols["oci"]
	proto.Verification.Consensus = &config.ConsensusConfig{Quorum: 0}
	cfg.Protocols["oci"] = proto

	err := config.Validate(cfg)
	assertValidationErr(t, err, "verification.consensus.quorum: must be >= 1")
}

// assertValidationErr fails t if err is nil or if its message does not contain
// the expected substring. All Validate errors are multi-line diagnostic strings;
// substring matching is intentional.
func assertValidationErr(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation error containing %q, got nil", contains)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q\ndoes not contain expected substring %q", err.Error(), contains)
	}
}
