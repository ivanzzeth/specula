// Package git — signed tag/commit verification (PRD §G2 git row, ARCHITECTURE §9).
//
// After a successful bare-mirror sync, updateSignedRefs walks each ref tip and
// asks the allowed-signers verifier whether it carries a valid signature from a
// trusted principal. A ref that does earns the `signed` tier (recorded as a
// per-ref pin so RepoTier and the cache browser can report it, and as a
// specula_verification_total series so /metrics can). A ref that does not stays
// at its tofu pin — signed refs are OPT-IN (PRD §G2: "签名 tag/commit（配
// allowed-signers）；否则 tofu"), so an unsigned ref is a skip, never a failure.
//
// # Failure semantics (the three honest outcomes)
//
//   - No signature (RefUnsigned): the signed check did not run — no pin, no
//     metric series (PRD §7.5 / 455f11f: a skipped check is expressed by
//     ABSENCE, never a fabricated pass). The ref keeps its tofu guarantee.
//   - Valid signature (RefSigned): record the signed pin + a signed/pass series.
//   - Signature present but NOT from an allowed principal (RefUntrusted): this is
//     a compromise signal (a forged/rotated/PGP-without-keyring tag). It records
//     a signed/fail series and an alert, and any stale signed pin is removed so
//     the repo cannot keep claiming `signed`. Under policy=enforce it ALSO fails
//     closed — the mirror is not served (bcc92b4 precedent: never hand a client
//     bytes an authenticity policy has rejected). Under policy=warn (the default,
//     because signed refs are opt-in) it degrades to tofu and still serves.
package git

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/verify"
)

// RefSignedKeyFor returns the MetadataStore mutable-entry key recording that a
// ref has earned the signed tier. Format: "git:signed:<host>/<project>:<refname>".
// The stored Digest is the SHA the signature was verified against, so a later
// sync can skip re-verifying an unchanged ref and can detect a signed→unsigned
// regression.
func RefSignedKeyFor(repo, refname string) string {
	return "git:signed:" + repo + ":" + refname
}

// updateSignedRefs verifies signatures on the mirror's ref tips against the
// allowed-signers anchor and records which refs reached TierSigned. It returns
// failClosed=true when policy=enforce and at least one ref carries an untrusted
// signature (the caller must then refuse to serve), plus human-readable alerts
// for the caller to log.
//
// v must be non-nil (the caller gates on h.signedRefs != nil).
func updateSignedRefs(
	ctx context.Context,
	v *verify.GitSignedVerifier,
	ms meta.MetadataStore,
	mirrorDir string,
	ref repoRef,
	log *slog.Logger,
) (failClosed bool, alerts []string) {
	mirrorPath := filepath.Join(mirrorDir, ref.mirrorRelPath())
	repo := ref.Host + "/" + ref.ProjectPath

	refs, err := listRefs(mirrorPath)
	if err != nil {
		return false, []string{fmt.Sprintf("git signed: list refs failed for %s: %v", ref.mirrorRelPath(), err)}
	}

	for refname, sha := range refs {
		signedKey := RefSignedKeyFor(repo, refname)

		// Cache: a ref already known signed at this exact SHA needs no re-verify
		// and no duplicate metric — the signature has not moved.
		if existing, gerr := ms.GetMutable(ctx, signedKey); gerr == nil && existing != nil && existing.Digest == sha {
			continue
		}

		trust, msg, verr := v.VerifyRef(ctx, mirrorPath, refname)
		if verr != nil {
			// Infrastructural failure (cannot read the object) — not a verdict.
			alerts = append(alerts, fmt.Sprintf("git signed: verify %s in %s: %v", refname, repo, verr))
			continue
		}

		switch trust {
		case verify.RefSigned:
			if putErr := ms.PutMutable(ctx, artifact.MutableEntry{
				Key:        signedKey,
				Protocol:   Protocol,
				Digest:     sha,
				TTLSeconds: tofuTTL,
				FetchedAt:  time.Now().UTC(),
			}); putErr != nil {
				alerts = append(alerts, fmt.Sprintf("git signed: set signed pin for %s in %s: %v", refname, repo, putErr))
				continue
			}
			metrics.RecordVerification(Protocol, v.Name(), artifact.TierSigned, artifact.StatusPass)
			if log != nil {
				log.Info("git: signed ref verified",
					slog.String("ref", refname), slog.String("repo", repo), slog.String("detail", msg))
			}

		case verify.RefUntrusted:
			// Signature present but not authenticated by the anchor — remove any
			// stale signed pin so the repo cannot keep claiming `signed`.
			_ = ms.DeleteMutable(ctx, signedKey)
			metrics.RecordVerification(Protocol, v.Name(), artifact.TierSigned, artifact.StatusFail)
			alerts = append(alerts, fmt.Sprintf(
				"git signed: UNTRUSTED signature on %s in %s: %s", refname, repo, msg))
			if log != nil {
				log.Warn("git: untrusted ref signature",
					slog.String("ref", refname), slog.String("repo", repo), slog.String("detail", msg))
			}
			if v.Enforce() {
				failClosed = true
			}

		case verify.RefUnsigned:
			// Opt-in: no signature means the signed check simply did not run for
			// this ref — no pin, no metric series (absence == "not checked here").
			// The ref keeps its tofu pin. Clear any stale signed pin from a prior
			// signed→unsigned regression.
			_ = ms.DeleteMutable(ctx, signedKey)
		}
	}

	return failClosed, alerts
}
