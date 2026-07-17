package verify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// GitSignedVerifier verifies a signed git tag or commit against a git
// allowed-signers file (ARCHITECTURE §9, DESIGN-REVIEW §5 — the optional git
// "signed" anchor). git objects are already a SHA-1/SHA-256 Merkle DAG (the
// inherent checksum tier); this verifier lifts a ref to TierSigned when the
// tag/commit at its tip carries a valid signature from a principal listed in the
// allowed-signers file.
//
//	allowed-signers file  →  set of trusted principals + SSH public keys
//	signed tag/commit     →  `git verify-tag` / `git verify-commit` against them
//
// Unlike the apt/helm keyrings this uses git's own machinery via os/exec (no
// OpenPGP dependency): `git -c gpg.ssh.allowedSignersFile=<path> verify-tag`.
//
// # Why the anchor is obtainable in CN (DESIGN-REVIEW §1.1)
//
// The allowed-signers file is a static local file the operator places
// out-of-band (e.g. /etc/specula/git-allowed-signers), exactly like the apt
// distro keyring. Verification is fully offline — no Fulcio/Rekor/TUF CDN — so
// the CN constraint that makes cosign keyless unavailable does NOT apply here.
//
// # Trust classification (per ref)
//
// VerifyRef classifies the tip of a ref into one of three states:
//
//	RefSigned    — a valid signature from an allowed principal (→ TierSigned)
//	RefUnsigned  — no signature present at all       (→ degrade to tofu, opt-in)
//	RefUntrusted — a signature present but NOT from an allowed principal, or a
//	               bad signature (→ warn+degrade, or fail-closed under enforce)
//
// The RefUnsigned/RefUntrusted split is the honest one: a forged tag that
// carries a signature no allowed key backs is a compromise signal (RefUntrusted),
// whereas a plain unsigned tag is simply not opted in (RefUnsigned). git's
// verify-tag exit code alone cannot tell them apart (both are non-zero), so the
// object body is inspected for a signature block.
//
// # SSH vs PGP
//
// The allowed-signers file authorises SSH signatures. A PGP-signed tag (still
// common on major projects, e.g. git/git) is validated by git against the local
// GPG keyring, which this anchor does not configure; such a tag therefore
// classifies as RefUntrusted (signature present, not authenticated by our
// anchor) and honestly degrades to tofu. Supplying a GPG keyring anchor is a
// documented extension, not wired here.
type GitSignedVerifier struct {
	// allowedSigners is the path to the git allowed-signers file (SSH format)
	// used by `git verify-tag` / `git verify-commit`.
	allowedSigners string
	// gitBin is the git executable used for verify-tag/verify-commit.
	gitBin string
	// policy is "enforce" (a RefUntrusted ref fails closed) or "warn" (alert and
	// degrade to tofu). Empty defaults to "warn": signed refs are opt-in.
	policy string
}

// RefTrust is the per-ref signature classification produced by VerifyRef.
type RefTrust int

const (
	// RefUnsigned means the ref tip carries no signature — not opted in.
	RefUnsigned RefTrust = iota
	// RefSigned means the ref tip carries a valid signature from an allowed principal.
	RefSigned
	// RefUntrusted means a signature is present but does not verify against the
	// allowed-signers anchor (wrong key, bad signature, or PGP without a keyring).
	RefUntrusted
)

// NewGitSignedVerifier returns a verifier that authorises signed tags/commits
// against the allowed-signers file at allowedSignersPath. An empty path is a
// fatal wiring error. policy is "enforce" or "warn" (empty → "warn"). The git
// binary is resolved from PATH.
func NewGitSignedVerifier(allowedSignersPath, policy string) (*GitSignedVerifier, error) {
	if allowedSignersPath == "" {
		return nil, fmt.Errorf("gitsigned: allowed-signers path is required for signed-ref verification")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("gitsigned: git binary not found on PATH: %w", err)
	}
	if policy == "" {
		policy = string(PolicyWarn)
	}
	return &GitSignedVerifier{allowedSigners: allowedSignersPath, gitBin: gitBin, policy: policy}, nil
}

// Compile-time assertion that GitSignedVerifier satisfies Verifier. The shared
// verification Chain is CAS-artifact-oriented and never sees Protocol=="git" —
// git artifacts are served by the bare-mirror handler, which calls VerifyRef
// directly (see internal/handler/git/signed.go), not the Chain. This method
// exists only so the type can sit in the shared verifier slice without special
// casing; it is an inert skip for every ref. The real signed-ref verification is
// VerifyRef.
var _ Verifier = (*GitSignedVerifier)(nil)

func (v *GitSignedVerifier) Name() string        { return "gitsigned" }
func (v *GitSignedVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// Verify is an inert skip: the git bare-mirror handler performs signed-ref
// verification via VerifyRef, never through the shared Chain (which handles CAS
// artifacts and never receives git refs). Returning StatusSkip means this entry
// contributes no verification series and makes no tier claim — the honest value
// for "did not run here" (PRD §7.5).
func (v *GitSignedVerifier) Verify(_ context.Context, ref artifact.ArtifactRef, _ *artifact.Artifact) (artifact.Result, error) {
	return artifact.Result{
		Status:  artifact.StatusSkip,
		Tier:    artifact.TierChecksum,
		Message: "gitsigned: skipped (git refs are verified by the mirror handler via VerifyRef, not the chain)",
	}, nil
}

// Policy reports the configured policy ("enforce" or "warn").
func (v *GitSignedVerifier) Policy() string { return v.policy }

// Enforce reports whether an untrusted signature must fail closed.
func (v *GitSignedVerifier) Enforce() bool { return v.policy == string(PolicyEnforce) }

// signatureMarkers are the object-body markers that prove a git ref tip carries
// a cryptographic signature (as opposed to being plainly unsigned).
var signatureMarkers = []string{
	"-----BEGIN SSH SIGNATURE-----",
	"-----BEGIN PGP SIGNATURE-----",
	"gpgsig ", // commit-header form
}

// VerifyRef classifies the signature on the tag/commit object at refname inside
// the bare mirror rooted at mirrorPath.
//
// It runs `git -C <mirror> -c gpg.ssh.allowedSignersFile=<anchor> verify-tag`
// (for annotated-tag objects) or `verify-commit` (for commit objects). A zero
// exit is RefSigned; a non-zero exit is disambiguated by inspecting the object
// body for a signature block — present → RefUntrusted, absent → RefUnsigned.
//
// The returned message is a human-readable diagnostic (git's own output on the
// non-signed paths). An error is returned only for infrastructural failures
// (e.g. the object cannot be read), never for an unsigned/untrusted verdict —
// those are normal classifications the caller acts on per policy.
func (v *GitSignedVerifier) VerifyRef(ctx context.Context, mirrorPath, refname string) (RefTrust, string, error) {
	objType, err := v.objectType(ctx, mirrorPath, refname)
	if err != nil {
		return RefUnsigned, "", fmt.Errorf("gitsigned: object type for %s: %w", refname, err)
	}

	var verifyArg string
	switch objType {
	case "tag":
		verifyArg = "verify-tag"
	case "commit":
		verifyArg = "verify-commit"
	default:
		// Trees/blobs are never signed ref tips.
		return RefUnsigned, fmt.Sprintf("gitsigned: %s is a %s object, not signable", refname, objType), nil
	}

	cmd := exec.CommandContext(ctx, v.gitBin, "-C", mirrorPath,
		"-c", "gpg.ssh.allowedSignersFile="+v.allowedSigners,
		verifyArg, refname)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return RefSigned, strings.TrimSpace(string(out)), nil
	}

	// Non-zero exit: signed-but-untrusted, or simply unsigned?
	signed, sigErr := v.objectHasSignature(ctx, mirrorPath, refname, objType)
	if sigErr != nil {
		return RefUnsigned, "", fmt.Errorf("gitsigned: read %s object for %s: %w", objType, refname, sigErr)
	}
	msg := strings.TrimSpace(string(out))
	if signed {
		return RefUntrusted, msg, nil
	}
	return RefUnsigned, msg, nil
}

// objectType returns the git object type of refname's tip ("tag", "commit", ...).
func (v *GitSignedVerifier) objectType(ctx context.Context, mirrorPath, refname string) (string, error) {
	out, err := exec.CommandContext(ctx, v.gitBin, "-C", mirrorPath,
		"cat-file", "-t", refname).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// objectHasSignature reports whether refname's tip object carries a signature
// block, distinguishing an unsigned ref from a signed-but-untrusted one.
func (v *GitSignedVerifier) objectHasSignature(ctx context.Context, mirrorPath, refname, objType string) (bool, error) {
	out, err := exec.CommandContext(ctx, v.gitBin, "-C", mirrorPath,
		"cat-file", objType, refname).Output()
	if err != nil {
		return false, err
	}
	body := string(out)
	for _, m := range signatureMarkers {
		if strings.Contains(body, m) {
			return true, nil
		}
	}
	return false, nil
}
