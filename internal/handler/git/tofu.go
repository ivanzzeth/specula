// Package git — TOFU ref→SHA pinning + force-push detection (ARCHITECTURE §9).
//
// After every successful bare-mirror sync, updateTOFUPins reads the current
// ref→SHA map from the mirror and compares each ref against the previously
// pinned SHA in MetadataStore:
//
//   - No pin  → first-lock: record the SHA. No alert.
//   - Same SHA → confirmed fast-forward (or no change). No alert.
//   - Different SHA:
//   - If `git merge-base --is-ancestor old new` succeeds → fast-forward update.
//   - Otherwise → NON-FAST-FORWARD alert (force-push / history rewrite).
//
// TOFU pins are stored as MutableEntry rows with TTL=-1 (permanent) and keys
// of the form "git:tofu:<host>/<project>:<refname>".
//
// DESIGN-REVIEW §5: "tofu:首次锁定 digest + 变更告警 (force-push/改史)"
package git

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

const tofuTTL = int64(-1) // permanent pin

// updateTOFUPins synchronises ref→SHA TOFU pins in the MetadataStore for
// the bare mirror at <mirrorDir>/<ref.mirrorRelPath()>. Returns a (possibly
// empty) slice of human-readable alert messages for the caller to log.
//
// Any per-ref error is non-fatal: it is converted to a warning message so
// one problematic ref cannot block serving of the others.
func updateTOFUPins(
	ctx context.Context,
	ms meta.MetadataStore,
	mirrorDir string,
	ref repoRef,
	log *slog.Logger,
) []string {
	mirrorPath := filepath.Join(mirrorDir, ref.mirrorRelPath())

	refs, err := listRefs(mirrorPath)
	if err != nil {
		return []string{fmt.Sprintf("git tofu: list refs failed for %s: %v", ref.mirrorRelPath(), err)}
	}

	var alerts []string
	for refname, sha := range refs {
		key := refTOFUKey(ref, refname)
		me, err := ms.GetMutable(ctx, key)
		if err != nil {
			alerts = append(alerts,
				fmt.Sprintf("git tofu: read pin for %s in %s: %v", refname, ref.mirrorRelPath(), err))
			continue
		}

		if me == nil || me.Digest == "" {
			// First sight: pin the current SHA.
			if putErr := ms.PutMutable(ctx, artifact.MutableEntry{
				Key:        key,
				Protocol:   Protocol,
				Digest:     sha,
				TTLSeconds: tofuTTL,
				FetchedAt:  time.Now().UTC(),
			}); putErr != nil {
				alerts = append(alerts,
					fmt.Sprintf("git tofu: set pin for %s in %s: %v", refname, ref.mirrorRelPath(), putErr))
			} else {
				// A newly locked ref is the moment the tofu guarantee turns on:
				// record the achieved tier so /metrics reflects it (the tier is a
				// real tofu PASS — a first-lock, not a change alert).
				metrics.RecordVerification(Protocol, "tofu", artifact.TierTofu, artifact.StatusPass)
			}
			// First-lock is informational, not an alert.
			continue
		}

		if me.Digest == sha {
			// Unchanged — confirmed.
			continue
		}

		// SHA changed: check for fast-forward.
		ff := isFastForward(mirrorPath, me.Digest, sha)
		if !ff {
			alert := fmt.Sprintf(
				"git tofu: NON-FAST-FORWARD update on %s in %s: was %s, now %s — possible force-push or history rewrite",
				refname, ref.mirrorRelPath(), me.Digest, sha,
			)
			alerts = append(alerts, alert)
			// A non-fast-forward change is the tofu guarantee firing: still the
			// tofu tier, but a WARN (force-push / history rewrite).
			metrics.RecordVerification(Protocol, "tofu", artifact.TierTofu, artifact.StatusWarn)
			if log != nil {
				log.Warn("git: non-fast-forward ref update detected",
					slog.String("ref", refname),
					slog.String("repo", ref.mirrorRelPath()),
					slog.String("old_sha", me.Digest),
					slog.String("new_sha", sha),
				)
			}
		}

		// Update pin to the new SHA regardless of direction.
		// The caller has already been alerted; the operator decides next steps.
		_ = ms.PutMutable(ctx, artifact.MutableEntry{
			Key:        key,
			Protocol:   Protocol,
			Digest:     sha,
			TTLSeconds: tofuTTL,
			FetchedAt:  time.Now().UTC(),
		})
	}
	return alerts
}

// listRefs runs `git for-each-ref` in mirrorPath and returns a
// refname→objectSHA map. Returns an empty map (no error) for an empty repo.
func listRefs(mirrorPath string) (map[string]string, error) {
	cmd := exec.Command("git", "-C", mirrorPath,
		"for-each-ref", "--format=%(refname) %(objectname)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w", err)
	}
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		result[parts[0]] = parts[1]
	}
	return result, nil
}

// isFastForward returns true when newSHA is reachable from oldSHA in the
// mirror at mirrorPath — i.e., newSHA is a descendant of oldSHA.
//
// Any failure of `git merge-base --is-ancestor` (including the case where
// oldSHA has been garbage-collected after a force-push) is treated as
// non-fast-forward, which is the conservative/correct interpretation.
func isFastForward(mirrorPath, oldSHA, newSHA string) bool {
	cmd := exec.Command("git", "-C", mirrorPath,
		"merge-base", "--is-ancestor", oldSHA, newSHA)
	err := cmd.Run()
	if err != nil {
		// Exit 1 = not an ancestor; exit 128 = object not found (also non-ff).
		return false
	}
	return true
}

// refTOFUKey returns the MetadataStore mutable-entry key for a git ref TOFU pin.
// Format: "git:tofu:<host>/<project>:<refname>"
func refTOFUKey(ref repoRef, refname string) string {
	return RefTOFUKeyFor(ref.Host+"/"+ref.ProjectPath, refname)
}

// RefTOFUKeyFor returns the TOFU pin key for refname in repo, where repo is the
// canonical "<host>/<project>" repository name (e.g. "github.com/octocat/Hello-World").
//
// Exported so the control plane can ask what a mirrored repo has actually
// earned, rather than guessing at the key format.
func RefTOFUKeyFor(repo, refname string) string {
	return "git:tofu:" + repo + ":" + refname
}

// RepoTier reports the honest trust tier a mirrored repo has EARNED, as a
// PRD §G2 tier name, or "" when it has earned nothing.
//
// The tier is derived from real state, never asserted from configuration, and is
// the HIGHEST tier any of the mirror's refs reached:
//
//   - `signed` — at least one ref carries a valid signature from an allowed
//     principal, recorded by updateSignedRefs as a RefSignedKeyFor pin. This is
//     a per-ref property (release tags sign; branch tips usually do not), so
//     repo-level `signed` means "this mirror has ≥1 authenticated ref", not that
//     every ref is authenticated — the per-ref truth lives in
//     specula_verification_total{check="gitsigned"}.
//   - `tofu` — no signed ref, but a ref→SHA pin exists (RefTOFUKeyFor). A pin is
//     what makes the tofu guarantee true: first-sight lock plus a
//     non-fast-forward alert on every later change (see updateTOFUPins), so a
//     pinned repo has force-push / history-rewrite detection live.
//   - "" — the repo has earned nothing (no refs, or no pins).
//
// `signed` is reached only when a signed-refs verifier is configured AND a ref's
// tip actually verified against the allowed-signers anchor — never asserted from
// config. When no verifier is configured, no signed pin is ever written, so this
// correctly tops out at tofu.
func RepoTier(ctx context.Context, ms meta.MetadataStore, mirrorDir, repo string) string {
	if ms == nil || mirrorDir == "" || repo == "" {
		return ""
	}
	refs, err := listRefs(filepath.Join(mirrorDir, repo+gitSuffix))
	if err != nil {
		return "" // cannot enumerate refs → cannot substantiate any tier
	}
	tofu := false
	for refname := range refs {
		if me, err := ms.GetMutable(ctx, RefSignedKeyFor(repo, refname)); err == nil && me != nil && me.Digest != "" {
			// At least one ref carries a verified signature: authenticity is live.
			return artifact.TierSigned.String()
		}
		me, err := ms.GetMutable(ctx, RefTOFUKeyFor(repo, refname))
		if err != nil {
			return ""
		}
		if me != nil && me.Digest != "" {
			tofu = true
		}
	}
	if tofu {
		return artifact.TierTofu.String()
	}
	return ""
}
