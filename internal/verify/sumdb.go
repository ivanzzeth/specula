package verify

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	xsumdb "golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// protocolGo is the ArtifactRef.Protocol value for Go modules. It is "gomod"
// (matching the upstream buildPath, the checksum/tofu keys, and the store rows)
// even though the CONFIG protocol map keys this block "go".
const protocolGo = "gomod"

// sumdbExtInfo / Mod / Zip are the recognised @v file extensions.
const (
	sumdbExtInfo = ".info"
	sumdbExtMod  = ".mod"
	sumdbExtZip  = ".zip"
)

// Policy controls how a sumdb verification failure is treated. GOSUMDB=off is
// intentionally not representable here — sumdb is never globally disabled
// (DESIGN-REVIEW H5).
type Policy string

const (
	// PolicyEnforce fails closed: a verification failure is StatusFail.
	PolicyEnforce Policy = "enforce"
	// PolicyWarn logs and serves at a degraded tier: a failure becomes
	// StatusWarn rather than StatusFail. Use sparingly.
	PolicyWarn Policy = "warn"
)

// TreeSizeStore persists the monotonic high-water signed tree size observed for
// a given sumdb, enabling anti-rollback / fork detection across restarts: the
// verifier rejects a signed tree head whose size regresses below the last
// persisted value (research.swtch.com/tlog consistency). Implementations back
// this with the metadata store.
type TreeSizeStore interface {
	// GetTreeSize returns the largest tree size previously persisted for
	// sumdbName, or (0, nil) if none has been recorded yet.
	GetTreeSize(ctx context.Context, sumdbName string) (int64, error)
	// SetTreeSize persists size as the new high-water mark for sumdbName.
	// Implementations must ignore a size that is not greater than the stored
	// value (monotonic; never regress).
	SetTreeSize(ctx context.Context, sumdbName string, size int64) error
}

// PrivateMatcher tests module paths against GONOSUMDB-style globs (Athens
// NoSumPatterns). Matching modules are private: their names must never be
// forwarded to the public sumdb, and /sumdb/ lookups for them return 403.
type PrivateMatcher struct {
	// globs is the comma-separated pattern list consumed by
	// module.MatchPrefixPatterns (GONOSUMDB / GOPRIVATE semantics).
	globs string
}

// NewPrivateMatcher builds a PrivateMatcher from GONOSUMDB glob patterns
// (e.g. ["git.internal.corp/*", "*.corp.example.com/*"]).
func NewPrivateMatcher(patterns []string) PrivateMatcher {
	return PrivateMatcher{globs: strings.Join(patterns, ",")}
}

// IsPrivate reports whether the CANONICAL (unescaped) module path matches any
// configured private glob. Callers must pass the unescaped module path.
func (m PrivateMatcher) IsPrivate(modulePath string) bool {
	if m.globs == "" {
		return false
	}
	return module.MatchPrefixPatterns(m.globs, modulePath)
}

// SumDBConfig is the runtime configuration for SumDBVerifier (mapped from
// config.SumDBConfig by the wiring layer).
type SumDBConfig struct {
	// URL is the sumdb access endpoint (sum.golang.google.cn or a GOPROXY
	// "/sumdb/" passthrough base).
	URL string
	// VerifierKey is the note verifier key ("<name>+<hash>+<base64key>").
	// Empty uses the compiled x/mod default (sum.golang.org).
	VerifierKey string
	// Policy is enforce (default) or warn. Never "off".
	Policy Policy
	// PrivatePatterns are GONOSUMDB globs; matching modules are never looked up
	// against the public sumdb.
	PrivatePatterns []string
	// TreeSize persists the anti-rollback high-water tree size. May be nil in
	// tests; production wiring supplies a metadata-backed implementation.
	TreeSize TreeSizeStore
	// RollbackToleranceEntries bounds a tolerated regression of the signed tree
	// head below the persisted high-water mark (CDN edge lag vs rollback attack).
	// nil uses defaultRollbackToleranceEntries; 0 is strict.
	RollbackToleranceEntries *int64
}

// SumDBVerifier verifies Go module authenticity against a signed sumdb tree head
// (Ed25519) with inclusion/consistency proofs, proxied via a GOPROXY "/sumdb/"
// endpoint or sum.golang.google.cn (fix H5). Highest tier for Go.
//
// # Self-gating
//
// Verify is a no-op StatusPass for any ref that is not an IMMUTABLE gomod
// artifact (wrong protocol, mutable list/@latest, or a private module). This
// lets a single global verification Chain include the sumdb verifier without it
// acting on other protocols.
//
// # Verification steps (per eligible artifact)
//
//  1. Parse module path + version from the ArtifactRef.
//  2. Skip .info files — no sumdb entry.
//  3. Call sumdb.Client.Lookup, which: fetches the signed tree head, verifies
//     the Ed25519 signature, verifies the inclusion proof for the record, and
//     verifies the consistency proof against the last-seen tree head (preventing
//     equivocation / fork).
//  4. Compute the h1: content hash of the quarantine artifact (dirhash.Hash1
//     for .mod; dirhash.HashZip for .zip).
//  5. Check that the h1: hash appears in the go.sum lines returned by Lookup.
//
// # Anti-rollback
//
// The client's WriteConfig callback checks the new signed tree size against the
// persisted high-water mark (TreeSizeStore). A regression triggers SecurityError
// and returns StatusFail / StatusWarn per policy.
type SumDBVerifier struct {
	cfg     SumDBConfig
	private PrivateMatcher

	// lazy-initialised sumdb client; goroutine-safe after init.
	initOnce sync.Once
	initErr  error
	ops      *specOps
	client   *xsumdb.Client
}

// NewSumDBVerifier constructs a SumDBVerifier from its runtime config.
func NewSumDBVerifier(cfg SumDBConfig) *SumDBVerifier {
	if cfg.Policy == "" {
		cfg.Policy = PolicyEnforce
	}
	return &SumDBVerifier{
		cfg:     cfg,
		private: NewPrivateMatcher(cfg.PrivatePatterns),
	}
}

// Compile-time assertion that SumDBVerifier satisfies Verifier.
var _ Verifier = (*SumDBVerifier)(nil)

func (v *SumDBVerifier) Name() string { return "sumdb" }

func (v *SumDBVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// IsPrivate reports whether modulePath (canonical, unescaped) is private under
// this verifier's GONOSUMDB patterns. Exposed so the /sumdb/ passthrough shares
// the same private-module policy as verification.
func (v *SumDBVerifier) IsPrivate(modulePath string) bool {
	return v.private.IsPrivate(modulePath)
}

// Verify checks the immutable Go module artifact against the checksum database.
//
// Returns StatusPass (at TierChecksum) for skipped cases (wrong protocol,
// mutable, private, .info). Returns StatusPass (TierSigned) on success.
// Returns StatusFail or StatusWarn (per Policy) on any verification failure.
func (v *SumDBVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Self-gate: only immutable Go module artifacts.
	if ref.Protocol != protocolGo || ref.Mutable {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "sumdb: skipped (not an immutable go module artifact)",
		}, nil
	}

	// Unescape module path; ref.Name is the bang-encoded URL-form.
	canonical, err := module.UnescapePath(moduleFromName(ref.Name))
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "sumdb: skipped (invalid escaped module path: " + err.Error() + ")",
		}, nil
	}

	// Private modules: never queried against the public sumdb.
	if v.private.IsPrivate(canonical) {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "sumdb: skipped (private module, not forwarded to public sumdb)",
		}, nil
	}

	// Parse version and file extension from the @v file component.
	version, ext, ok := sumdbFileVersionExt(ref.Version)
	if !ok {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "sumdb: skipped (unrecognised file component: " + ref.Version + ")",
		}, nil
	}

	// .info files have no sumdb entry (they carry timestamps, not content hashes).
	if ext == sumdbExtInfo {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "sumdb: skipped (.info files are not covered by the checksum database)",
		}, nil
	}

	// Obtain (or lazily initialise) the sumdb client.
	client, ops, err := v.getClient()
	if err != nil {
		return v.policyResult("sumdb: client init: " + err.Error())
	}

	// Build the version key for the sumdb lookup.
	// .mod uses "<version>/go.mod"; .zip uses "<version>" alone.
	lookupVers := version
	if ext == sumdbExtMod {
		lookupVers = version + "/go.mod"
	}

	// Lookup verifies the signed tree head, inclusion proof, and consistency
	// proof against the persisted tree head (via mergeLatest / WriteConfig).
	lines, err := client.Lookup(canonical, lookupVers)
	if err != nil {
		// SecurityError (fork/rollback) takes precedence over the lookup error.
		if secErr := ops.securityError(); secErr != nil {
			return v.policyResult(secErr.Error())
		}
		return v.policyResult(fmt.Sprintf("sumdb: lookup %s@%s: %v", canonical, lookupVers, err))
	}

	// Compute the h1: content hash of the quarantine artifact.
	var h1hash string
	switch ext {
	case sumdbExtMod:
		h1hash, err = hashGoModFile(art.Path)
	case sumdbExtZip:
		h1hash, err = dirhash.HashZip(art.Path, dirhash.DefaultHash)
	}
	if err != nil {
		return v.policyResult(fmt.Sprintf("sumdb: compute h1 hash (%s): %v", ext, err))
	}

	// Verify that one of the go.sum lines returned by the sumdb matches.
	// Format: "<module> <version> h1:<base64>"
	expectedPrefix := canonical + " " + lookupVers + " "
	for _, line := range lines {
		if strings.HasPrefix(line, expectedPrefix) {
			if strings.TrimPrefix(line, expectedPrefix) == h1hash {
				return artifact.Result{
					Status:  artifact.StatusPass,
					Tier:    artifact.TierSigned,
					Message: fmt.Sprintf("sumdb: verified %s@%s %s", canonical, lookupVers, h1hash),
				}, nil
			}
			// Hash found in sumdb but doesn't match artifact → tampered.
			dbHash := strings.TrimPrefix(line, expectedPrefix)
			return v.policyResult(fmt.Sprintf(
				"sumdb: HASH MISMATCH for %s@%s: artifact h1=%s sumdb h1=%s — possible tampered module",
				canonical, lookupVers, h1hash, dbHash,
			))
		}
	}

	// No matching line at all (sumdb returned lines for wrong version?).
	return v.policyResult(fmt.Sprintf(
		"sumdb: no matching go.sum line for %s@%s (got lines: %v)",
		canonical, lookupVers, lines,
	))
}

// policyResult returns a StatusFail or StatusWarn result per the configured
// policy, plus a nil error (the chain should not short-circuit on warn).
func (v *SumDBVerifier) policyResult(msg string) (artifact.Result, error) {
	if v.cfg.Policy == PolicyWarn {
		return artifact.Result{
			Status:  artifact.StatusWarn,
			Tier:    artifact.TierSigned,
			Message: "sumdb warn: " + msg,
		}, nil
	}
	return artifact.Result{
		Status:  artifact.StatusFail,
		Tier:    artifact.TierSigned,
		Message: msg,
	}, nil
}

// getClient lazily initialises and returns the sumdb.Client and its ops.
// Errors are cached so a bad config fails fast on every call.
func (v *SumDBVerifier) getClient() (*xsumdb.Client, *specOps, error) {
	v.initOnce.Do(func() {
		ops, err := newSpecOps(v.cfg.VerifierKey, v.cfg.URL, v.cfg.TreeSize, nil)
		if err != nil {
			v.initErr = err
			return
		}
		if t := v.cfg.RollbackToleranceEntries; t != nil {
			ops.rollbackTolerance = *t
		}
		v.ops = ops
		v.client = xsumdb.NewClient(ops)
	})
	return v.client, v.ops, v.initErr
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// sumdbFileVersionExt extracts the version and extension from an @v file
// component (e.g. "v1.2.3.mod" → "v1.2.3", ".mod", true).
// Returns ("", "", false) for unrecognised components.
func sumdbFileVersionExt(fileComponent string) (version, ext string, ok bool) {
	for _, e := range []string{sumdbExtInfo, sumdbExtMod, sumdbExtZip} {
		if strings.HasSuffix(fileComponent, e) {
			return strings.TrimSuffix(fileComponent, e), e, true
		}
	}
	return "", "", false
}

// hashGoModFile computes the h1: directory hash for a go.mod file at path.
// The go tool encodes go.mod checksums as if the file were a one-entry
// directory whose only file is named "go.mod".
func hashGoModFile(path string) (string, error) {
	return dirhash.Hash1([]string{"go.mod"}, func(_ string) (io.ReadCloser, error) {
		return os.Open(path)
	})
}

// moduleFromName strips a trailing "/@v/<...>" artifact suffix if the
// caller passed a composite name. gomod refs set Name to the bare module path,
// so this is normally an identity; it guards against composite keys.
func moduleFromName(name string) string {
	if i := strings.Index(name, "/@"); i >= 0 {
		return name[:i]
	}
	return name
}
