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
	return "git:tofu:" + ref.Host + "/" + ref.ProjectPath + ":" + refname
}
