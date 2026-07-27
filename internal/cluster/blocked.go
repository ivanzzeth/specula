package cluster

import (
	"fmt"
	"strings"
	"time"
)

// Fail fast on a Pod that will never become ready.
//
// `kubectl rollout status --timeout=5m` reports only "timed out waiting for the
// condition". On a real ACK cluster where the image was not pullable that burned
// the full five minutes and told the operator nothing — the actual cause (a 403
// from the registry) was only visible via `kubectl get events`. A terminal pull or
// config failure is knowable within seconds, so surface it instead of waiting.

// waitingState is one container's Waiting state.
type waitingState struct {
	Reason  string
	Message string
}

// fatalWaitingReasons never resolve on their own: no amount of waiting fixes an
// unreachable registry or a missing Secret. CrashLoopBackOff is deliberately NOT
// here — a slow-starting daemon looks identical for the first few restarts, and
// the caller's timeout is the right owner of that case.
var fatalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"ErrImageNeverPull":          true,
	"InvalidImageName":           true,
	"RegistryUnavailable":        true,
	"CreateContainerConfigError": true,
}

// blockingPodReason reports whether a container's Waiting state is terminal, and a
// human-readable reason that quotes the kubelet's own message — that message is
// where the registry's 403 / timeout actually lives.
func blockingPodReason(reason, message string) (string, bool) {
	reason = strings.TrimSpace(reason)
	if reason == "" || !fatalWaitingReasons[reason] {
		return "", false
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return reason, true
	}
	return fmt.Sprintf("%s: %s", reason, message), true
}

// parseWaitingStates reads `kubectl get pods -o jsonpath` output shaped as one
// "<reason>\t<message>" line per waiting container. Blank lines and absent
// messages are normal (a container with no Waiting state emits nothing).
func parseWaitingStates(raw string) []waitingState {
	var out []waitingState
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		reason, message, _ := strings.Cut(line, "\t")
		if strings.TrimSpace(reason) == "" {
			continue
		}
		out = append(out, waitingState{Reason: strings.TrimSpace(reason), Message: strings.TrimSpace(message)})
	}
	return out
}

func firstFatalWaitingState(states []waitingState) (string, bool) {
	for _, s := range states {
		if reason, fatal := blockingPodReason(s.Reason, s.Message); fatal {
			return reason, true
		}
	}
	return "", false
}

// waitRolloutOrFailFast waits for a workload's Pods while polling for a terminal
// Waiting state. It returns as soon as either the rollout succeeds or a Pod is
// provably stuck, so an unpullable image fails in seconds with the registry's own
// error rather than after the full timeout with none.
func waitRolloutOrFailFast(kubeconfig, context, ns, selector, rolloutTarget string, timeout time.Duration) error {
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := kubectl(kubeconfig, context, "rollout", "status", rolloutTarget,
			"-n", ns, "--timeout="+formatDuration(timeout))
		done <- result{err}
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			return r.err
		case <-ticker.C:
			out, err := kubectl(kubeconfig, context, "get", "pods", "-n", ns, "-l", selector,
				"-o", `jsonpath={range .items[*].status.containerStatuses[*]}`+
					`{.state.waiting.reason}{"\t"}{.state.waiting.message}{"\n"}{end}`)
			if err != nil {
				continue // transient API error: the rollout watch still owns the outcome
			}
			if reason, fatal := firstFatalWaitingState(parseWaitingStates(string(out))); fatal {
				return fmt.Errorf("%s will not become ready — %s", rolloutTarget, reason)
			}
		}
	}
}
