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

func TestValidate_MaxBytesNegative(t *testing.T) {
	cfg := validCfg()
	cfg.Cache.MaxBytes = -1
	err := config.Validate(cfg)
	assertValidationErr(t, err, "cache.max_bytes")
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
	proto.MutableTTLSeconds = config.TTLPtr(-2)
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

// pypiLikeConsensusCfg builds a config with a single pypi protocol whose
// verification asks for the consensus tier. mirrors is the number of non-official
// (independent-mirror) upstreams; one official (origin-witness) upstream is always
// appended. quorum is the requested agreement threshold.
func pypiLikeConsensusCfg(mirrors, quorum int) *config.Config {
	cfg := validCfg()
	ups := make([]config.UpstreamConfig, 0, mirrors+1)
	for i := 0; i < mirrors; i++ {
		ups = append(ups, config.UpstreamConfig{
			Name:     "mirror" + string(rune('a'+i)),
			BaseURL:  "https://mirror" + string(rune('a'+i)) + ".example.cn",
			Official: false,
		})
	}
	ups = append(ups, config.UpstreamConfig{
		Name: "origin", BaseURL: "https://pypi.org", Official: true,
	})
	cfg.Protocols = map[string]config.ProtocolConfig{
		"pypi": {
			Upstreams: ups,
			Verification: config.VerificationConfig{
				Tiers:  []string{"consensus", "tofu", "checksum"},
				Quorum: quorum,
			},
		},
	}
	return cfg
}

// TestValidate_ConsensusQuorumExceedsMirrors is the Bug-2 guard: an
// unsatisfiable consensus quorum (quorum > available independent mirrors) must be
// rejected at config validation, not silently at boot. PRD §6's own example
// shipped exactly this — pypi quorum:2 with a single non-official upstream (the
// other being the official origin WITNESS) — and it could not start a server, yet
// a parse-only test named for §6 passed it. This is the a5080cb rule, moved to the
// layer that config.Load already gates on so the doc test actually catches drift.
func TestValidate_ConsensusQuorumExceedsMirrors(t *testing.T) {
	// 1 non-official mirror + 1 official witness, quorum 2 → unsatisfiable.
	err := config.Validate(pypiLikeConsensusCfg(1, 2))
	assertValidationErr(t, err, "consensus quorum 2 exceeds")
}

// TestValidate_ConsensusQuorumSatisfiable confirms the fix's shape: two genuine
// independent mirrors + an origin witness makes quorum:2 satisfiable, so the
// config validates. This is the configuration §6 must teach — one that both boots
// AND demonstrates cross-source consensus.
func TestValidate_ConsensusQuorumSatisfiable(t *testing.T) {
	if err := config.Validate(pypiLikeConsensusCfg(2, 2)); err != nil {
		t.Fatalf("2 mirrors + witness, quorum 2 must validate, got: %v", err)
	}
}

// TestValidate_ConsensusQuorumNonMetadataProtocol pins that the check is gated by
// consensus ACHIEVABILITY: npm advertises sha512 integrity, never a metadata-only
// sha256, so its consensus tier is a documented no-op that downgrades to tofu at
// boot (cmd/specula) and must NOT be rejected here — rejecting it would break a
// config the server actually starts.
func TestValidate_ConsensusQuorumNonMetadataProtocol(t *testing.T) {
	cfg := pypiLikeConsensusCfg(1, 2)
	// Re-key the protocol as npm (non-metadata-consensus-capable).
	proto := cfg.Protocols["pypi"]
	delete(cfg.Protocols, "pypi")
	cfg.Protocols["npm"] = proto
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("npm consensus quorum>mirrors must NOT be rejected (downgrades to tofu), got: %v", err)
	}
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
