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

// scheduleState is one Pod's PodScheduled condition.
type scheduleState struct {
	Status  string
	Reason  string
	Message string
}

// blockingScheduleReason reports whether a PodScheduled condition is terminal.
//
// Unschedulable is the one that matters and the one that was missed: an
// unschedulable Pod has NO containerStatuses, so watching container Waiting states
// alone never sees "Insufficient memory". Waiting cannot create capacity, and by
// the time the rollout times out the mirror DaemonSet has already rewritten
// hosts.toml on every node — a capacity problem turned into broken node pulls.
//
// SchedulerError is transient (API hiccup, webhook blip) and is NOT terminal.
func blockingScheduleReason(reason, message string) (string, bool) {
	if strings.TrimSpace(reason) != "Unschedulable" {
		return "", false
	}
	if message = strings.TrimSpace(message); message != "" {
		return fmt.Sprintf("Unschedulable: %s", message), true
	}
	return "Unschedulable", true
}

// parseScheduleStates reads "<status>\t<reason>\t<message>" per Pod.
func parseScheduleStates(raw string) []scheduleState {
	var out []scheduleState
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		st := scheduleState{Status: strings.TrimSpace(f[0])}
		if len(f) > 1 {
			st.Reason = strings.TrimSpace(f[1])
		}
		if len(f) > 2 {
			st.Message = strings.TrimSpace(f[2])
		}
		out = append(out, st)
	}
	return out
}

func firstFatalScheduleState(states []scheduleState) (string, bool) {
	for _, s := range states {
		if reason, fatal := blockingScheduleReason(s.Reason, s.Message); fatal {
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
			// Unschedulable Pods have no container statuses at all, so the check
			// above cannot see them. This is where "Insufficient memory" lives.
			sched, serr := kubectl(kubeconfig, context, "get", "pods", "-n", ns, "-l", selector,
				"-o", `jsonpath={range .items[*]}{range .status.conditions[?(@.type=="PodScheduled")]}`+
					`{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}{end}`)
			if serr == nil {
				if reason, fatal := firstFatalScheduleState(parseScheduleStates(string(sched))); fatal {
					return fmt.Errorf("%s will not become ready — %s", rolloutTarget, reason)
				}
			}
		}
	}
}

// daemonSetAbsent reports whether a kubectl error means the DaemonSet does not exist.
//
// A deployment profile may switch the node-side agents off — a hosted Specula serves
// OTHER clusters and must not rewrite containerd config on the nodes it happens to run
// on, so `integrate.enabled=false` is a supported shape. Without this check `--wait`
// polled a DaemonSet that would never appear for the full timeout and then failed,
// reporting a perfectly healthy install as broken.
//
// Only a NotFound naming daemonsets counts. Every other error is transient: an API
// blip must not be mistaken for "disabled", or a genuinely stuck DaemonSet gets
// skipped in silence.
func daemonSetAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "notfound") && strings.Contains(msg, "daemonset")
}
