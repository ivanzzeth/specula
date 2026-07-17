package config

import (
	"fmt"
	"strings"
	"time"
)

// validTiers is the set of recognised verification tier names.
var validTiers = map[string]bool{
	"checksum":  true,
	"tofu":      true,
	"consensus": true,
	"signed":    true,
}

// validSumDBPolicies is the set of allowed Go sumdb policies. Note the
// deliberate ABSENCE of "off": GOSUMDB must never be disabled (DESIGN-REVIEW H5).
var validSumDBPolicies = map[string]bool{
	"":        true, // empty defaults to "enforce"
	"enforce": true,
	"warn":    true,
}

// validVerifyPolicies is the enforce/warn set shared by the gpg / provenance /
// signed_refs / tofu verification blocks. Empty is allowed (per-block default).
var validVerifyPolicies = map[string]bool{
	"":        true,
	"enforce": true,
	"warn":    true,
}

// validOnPrivateDown is the allowed set for dependency-confusion fail behaviour.
var validOnPrivateDown = map[string]bool{
	"":            true, // empty defaults to "fail_closed"
	"fail_closed": true,
	"serve_stale": true,
}

// MetadataConsensusProtocols is the set of protocols for which cross-source
// consensus is actually BUILDABLE metadata-only: their mirror metadata advertises
// a per-file sha256 that independent mirrors can be polled for without fetching
// the blob (pypi's PEP 503 `#sha256=`, oci's manifest digest). npm advertises
// sha512 integrity and tarball advertises nothing, so their consensus tier is a
// documented no-op that downgrades to tofu at boot — those protocols must NOT be
// subjected to the quorum-vs-mirrors check below, since the tier never runs.
//
// This is the single source of truth for that capability; cmd/specula's verifier
// build reads the same fact (kept in sync deliberately — see buildConsensusVerifier).
var MetadataConsensusProtocols = map[string]bool{
	"oci":  true,
	"pypi": true,
}

// countConsensusMirrors returns the number of independent mirrors a consensus
// verifier would poll for this protocol, applying the SAME derivation cmd/specula
// uses (a5080cb): an explicit verification.consensus.mirrors list wins; otherwise
// non-official upstreams vote and the FIRST official upstream becomes the origin
// WITNESS (not a mirror). The witness never counts toward quorum.
func countConsensusMirrors(proto ProtocolConfig) int {
	if cc := proto.Verification.Consensus; cc != nil && len(cc.Mirrors) > 0 {
		return len(cc.Mirrors)
	}
	mirrors, witnessTaken := 0, false
	for _, u := range proto.Upstreams {
		if u.Official && !witnessTaken {
			witnessTaken = true // first official is the origin witness, not a mirror
			continue
		}
		mirrors++
	}
	return mirrors
}

// Validate checks a loaded Config for consistency and completeness.
// All detected problems are collected and returned as a single error
// with one message per line so callers see the full picture at once.
//
// Validation rules:
//
//   - server: both addresses must be non-empty.
//   - storage.blob: driver must be "local" or "s3"; driver-specific
//     required fields must be provided.
//   - storage.meta: driver must be "sqlite" or "postgres"; dsn non-empty.
//   - cache: negative_ttl_seconds must be >= 0.
//   - protocols: each protocol must have ≥1 upstream with name + base_url;
//     verification tiers must be from the valid set; quorum must be ≥1 when
//     the "consensus" tier is enabled.
func Validate(cfg *Config) error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// ── Server ────────────────────────────────────────────────────────────
	if strings.TrimSpace(cfg.Server.DataPlaneAddr) == "" {
		add("server.data_plane_addr: must not be empty")
	}
	if strings.TrimSpace(cfg.Server.ControlPlaneAddr) == "" {
		add("server.control_plane_addr: must not be empty")
	}

	// ── Storage — Blob ────────────────────────────────────────────────────
	switch cfg.Storage.Blob.Driver {
	case "local":
		if strings.TrimSpace(cfg.Storage.Blob.Local.Root) == "" {
			add("storage.blob.local.root: must not be empty when driver is \"local\"")
		}
	case "s3":
		if strings.TrimSpace(cfg.Storage.Blob.S3.Bucket) == "" {
			add("storage.blob.s3.bucket: must not be empty when driver is \"s3\"")
		}
	default:
		add("storage.blob.driver: must be \"local\" or \"s3\", got %q", cfg.Storage.Blob.Driver)
	}

	// ── Storage — Meta ────────────────────────────────────────────────────
	switch cfg.Storage.Meta.Driver {
	case "sqlite", "postgres":
		if strings.TrimSpace(cfg.Storage.Meta.DSN) == "" {
			add("storage.meta.dsn: must not be empty")
		}
	default:
		add("storage.meta.driver: must be \"sqlite\" or \"postgres\", got %q", cfg.Storage.Meta.Driver)
	}

	// ── Cache ─────────────────────────────────────────────────────────────
	// default_mutable_ttl_seconds: -1/0/positive are all valid sentinels.
	// negative_ttl_seconds: must be >= 0 (0 = disabled, positive = cache duration).
	if cfg.Cache.NegativeTTLSeconds < 0 {
		add("cache.negative_ttl_seconds: must be >= 0 (0 = disabled); got %d",
			cfg.Cache.NegativeTTLSeconds)
	}

	// ── Protocols ─────────────────────────────────────────────────────────
	for name, proto := range cfg.Protocols {
		if len(proto.Upstreams) == 0 {
			add("protocols.%s: at least one upstream is required", name)
			// Skip further checks for this protocol — no upstreams to inspect.
			continue
		}
		for i, up := range proto.Upstreams {
			if strings.TrimSpace(up.Name) == "" {
				add("protocols.%s.upstreams[%d].name: must not be empty", name, i)
			}
			if strings.TrimSpace(up.BaseURL) == "" {
				add("protocols.%s.upstreams[%d].base_url: must not be empty", name, i)
			}
		}

		// Verification tiers.
		hasConsensus := false
		for _, tier := range proto.Verification.Tiers {
			if !validTiers[tier] {
				add("protocols.%s.verification.tiers: unknown tier %q "+
					"(valid: checksum, tofu, consensus, signed)", name, tier)
			}
			if tier == "consensus" {
				hasConsensus = true
			}
		}
		// Effective quorum comes from the structured Consensus block when
		// present, else the flat Quorum field (back-compat).
		effectiveQuorum := proto.Verification.Quorum
		if proto.Verification.Consensus != nil {
			effectiveQuorum = proto.Verification.Consensus.Quorum
		}
		if hasConsensus && effectiveQuorum < 1 {
			add("protocols.%s.verification.quorum: must be >= 1 when "+
				"\"consensus\" tier is enabled, got %d", name, effectiveQuorum)
		}
		// Structured consensus block: quorum must be >= 1 whenever it is set.
		if cc := proto.Verification.Consensus; cc != nil && cc.Quorum < 1 {
			add("protocols.%s.verification.consensus.quorum: must be >= 1, got %d",
				name, cc.Quorum)
		}
		// Unsatisfiable consensus quorum (quorum > available independent mirrors)
		// is a HARD error, not a boot-time surprise. This is a5080cb's rule lifted
		// to the config layer that config.Load already gates on: an `official: true`
		// upstream becomes the origin WITNESS, not a mirror, so two upstreams with
		// quorum:2 leaves a single voter and the tier can never pass — every fetch
		// of that protocol would then fail closed forever. Only checked for
		// protocols whose consensus is genuinely buildable metadata-only; npm/tarball
		// downgrade to tofu and must not be rejected. effectiveQuorum >= 1 is assumed
		// (the guard above already fired otherwise, but we still want a clean number).
		if hasConsensus && MetadataConsensusProtocols[name] && effectiveQuorum >= 1 {
			if mirrors := countConsensusMirrors(proto); effectiveQuorum > mirrors {
				add("protocols.%s.verification: consensus quorum %d exceeds the %d "+
					"available independent mirror(s) — the tier can never pass and every "+
					"%s fetch would fail closed. An upstream marked `official: true` becomes "+
					"the origin WITNESS, not a mirror. Either lower quorum to <= %d, or add "+
					"%d more non-official upstream(s)/consensus mirror(s)",
					name, effectiveQuorum, mirrors, name, mirrors, effectiveQuorum-mirrors)
			}
		}

		// cosign block (only meaningful for "oci"): tlog must be false
		// (keyless/transparency-log verification is unsupported) and at least
		// one public key is required.
		if cs := proto.Verification.Cosign; cs != nil {
			if cs.Tlog {
				add("protocols.%s.verification.cosign.tlog: must be false "+
					"(keyless/transparency-log verification is unsupported)", name)
			}
			if len(cs.Keys) == 0 {
				add("protocols.%s.verification.cosign.keys: at least one public "+
					"key is required when the cosign block is set", name)
			}
		}

		// Per-protocol mutable TTL: -1/0/positive are valid sentinels.
		// Values below -1 are not meaningful. nil means the protocol did not set
		// one and inherits the global default, which has nothing to validate here.
		if proto.MutableTTLSeconds != nil && *proto.MutableTTLSeconds < TTLNeverRevalidate {
			add("protocols.%s.mutable_ttl_seconds: must be >= -1, got %d",
				name, *proto.MutableTTLSeconds)
		}

		// Go sumdb block (only meaningful for the "go" protocol). Policy must be
		// enforce/warn — never "off" (DESIGN-REVIEW H5: GOSUMDB is never disabled).
		if proto.SumDB != nil && !validSumDBPolicies[proto.SumDB.Policy] {
			add("protocols.%s.sumdb.policy: must be \"enforce\" or \"warn\" "+
				"(GOSUMDB=off is forbidden), got %q", name, proto.SumDB.Policy)
		}

		// Structured verification sub-blocks (apt gpg / helm provenance / git
		// signed_refs / tofu policy). All policies are enforce|warn (or empty).
		vc := proto.Verification
		if vc.GPG != nil && !validVerifyPolicies[vc.GPG.Policy] {
			add("protocols.%s.verification.gpg.policy: must be \"enforce\" or \"warn\", got %q",
				name, vc.GPG.Policy)
		}
		if vc.Provenance != nil && !validVerifyPolicies[vc.Provenance.Policy] {
			add("protocols.%s.verification.provenance.policy: must be \"enforce\" or \"warn\", got %q",
				name, vc.Provenance.Policy)
		}
		if vc.SignedRefs != nil && !validVerifyPolicies[vc.SignedRefs.Policy] {
			add("protocols.%s.verification.signed_refs.policy: must be \"enforce\" or \"warn\", got %q",
				name, vc.SignedRefs.Policy)
		}
		if !validVerifyPolicies[vc.Tofu] {
			add("protocols.%s.verification.tofu: must be \"enforce\" or \"warn\", got %q",
				name, vc.Tofu)
		}
		if dc := vc.DependencyConfusion; dc != nil && !validOnPrivateDown[dc.OnPrivateDown] {
			add("protocols.%s.verification.dependency_confusion.on_private_down: "+
				"must be \"fail_closed\" or \"serve_stale\", got %q", name, dc.OnPrivateDown)
		}

		// git block: validate the sync_stale_after duration string when set.
		if proto.Git != nil && strings.TrimSpace(proto.Git.SyncStaleAfter) != "" {
			if _, err := time.ParseDuration(proto.Git.SyncStaleAfter); err != nil {
				add("protocols.%s.git.sync_stale_after: invalid duration %q: %v",
					name, proto.Git.SyncStaleAfter, err)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config validation failed (%d error(s)):\n  %s",
		len(errs), strings.Join(errs, "\n  "))
}
