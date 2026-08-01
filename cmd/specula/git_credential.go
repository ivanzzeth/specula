package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ivanzzeth/specula/internal/gitcred"
)

// runGitCredential implements: specula git-credential [<get|store|erase>]
//
// Speaks the git-credential helper protocol on stdin/stdout. Rewrites Specula
// /git/<host>/… requests to the upstream host so private clones through
// insteadOf pick up the operator's normal credentials (or GH_TOKEN).
func runGitCredential(args []string) error {
	action := "get"
	if len(args) > 0 && args[0] != "" {
		action = args[0]
	}
	attrs, err := gitcred.ParseAttrs(os.Stdin)
	if err != nil {
		return err
	}
	rewritten, ok := gitcred.RewriteForUpstream(attrs, nil)
	if !ok {
		// Not a Specula git-proxy URL — emit nothing so the next helper runs.
		return nil
	}
	switch action {
	case "get":
		filled, err := gitcred.FillOrEnv(rewritten)
		if err != nil {
			// Emit nothing: next helper / git may still prompt.
			return nil
		}
		if strings.TrimSpace(filled["password"]) == "" {
			return nil
		}
		// Return credentials bound to the ORIGINAL Specula proxy URL.
		// If we echo the rewritten host=github.com, git discards the fill
		// because it does not match the request host (127.0.0.1:7732).
		out := map[string]string{
			"protocol": attrs["protocol"],
			"host":     attrs["host"],
			"username": filled["username"],
			"password": filled["password"],
		}
		if p := attrs["path"]; p != "" {
			out["path"] = p
		}
		return gitcred.WriteAttrs(os.Stdout, out)
	case "store", "erase":
		_ = gitcred.PassThroughStoreErase(action, rewritten)
		return nil
	default:
		return fmt.Errorf("unknown action %q (want get|store|erase)", action)
	}
}
