package cluster

import (
	"strings"
	"testing"
)

// Observed on a real ACK cluster: the image was not pullable from the nodes, and
// `cluster install --wait` sat in `kubectl rollout status` for the full 5 minutes
// before failing with "timed out waiting for the condition" — no mention of the
// actual cause. The operator then has to go find it by hand:
//
//	kubectl -n specula-boot get events | grep Failed
//	... 403 Forbidden from the registry
//
// A terminal pull failure is knowable in seconds, so detect it and say so.
func TestBlockingPodReasonDetectsImagePullFailure(t *testing.T) {
	cases := []struct {
		name, waitingReason, message string
		wantFatal                    bool
		wantIn                       string
	}{
		{
			name:          "ImagePullBackOff carries the registry error",
			waitingReason: "ImagePullBackOff",
			message: `Back-off pulling image "docker.m.daocloud.io/ivanzz/specula:v0.10.0": ` +
				`unexpected status from HEAD request: 403 Forbidden`,
			wantFatal: true,
			wantIn:    "403 Forbidden",
		},
		{
			name:          "ErrImagePull",
			waitingReason: "ErrImagePull",
			message:       `failed to resolve reference: dial tcp: i/o timeout`,
			wantFatal:     true,
			wantIn:        "i/o timeout",
		},
		{
			name:          "InvalidImageName",
			waitingReason: "InvalidImageName",
			message:       `couldn't parse image name`,
			wantFatal:     true,
			wantIn:        "InvalidImageName",
		},
		{
			name:          "CreateContainerConfigError",
			waitingReason: "CreateContainerConfigError",
			message:       `secret "specula-tls" not found`,
			wantFatal:     true,
			wantIn:        "not found",
		},
		{
			// Transient: the image is being pulled right now. Must NOT abort.
			name:          "ContainerCreating is not fatal",
			waitingReason: "ContainerCreating",
			message:       "",
			wantFatal:     false,
		},
		{
			// A crash loop can be a slow start (probe not passing yet); the caller's
			// timeout owns that case, not a fail-fast.
			name:          "CrashLoopBackOff is not fatal",
			waitingReason: "CrashLoopBackOff",
			message:       "back-off restarting failed container",
			wantFatal:     false,
		},
		{
			name:          "PodInitializing is not fatal",
			waitingReason: "PodInitializing",
			wantFatal:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := blockingPodReason(tc.waitingReason, tc.message)
			if fatal != tc.wantFatal {
				t.Fatalf("fatal = %v, want %v (reason %q)", fatal, tc.wantFatal, reason)
			}
			if tc.wantFatal {
				if !strings.Contains(reason, tc.waitingReason) {
					t.Fatalf("reason must name the waiting reason: %q", reason)
				}
				if tc.wantIn != "" && !strings.Contains(reason, tc.wantIn) {
					t.Fatalf("reason must quote the kubelet message (%q): %q", tc.wantIn, reason)
				}
			}
		})
	}
}

// Empty input must never be treated as fatal — a Pod with no container status yet
// is simply young.
func TestBlockingPodReasonEmptyIsNotFatal(t *testing.T) {
	if _, fatal := blockingPodReason("", ""); fatal {
		t.Fatal("empty waiting state must not be fatal")
	}
}

// The parser reads `kubectl get pods -o jsonpath` output: one line per container,
// "<reason>\t<message>". It must tolerate blank lines and missing messages.
func TestParseWaitingStates(t *testing.T) {
	raw := "ContainerCreating\t\n" +
		"ImagePullBackOff\tBack-off pulling image \"x\": 403 Forbidden\n" +
		"\n"
	states := parseWaitingStates(raw)
	if len(states) != 2 {
		t.Fatalf("got %d states, want 2: %#v", len(states), states)
	}
	if states[0].Reason != "ContainerCreating" || states[0].Message != "" {
		t.Fatalf("first state wrong: %#v", states[0])
	}
	if states[1].Reason != "ImagePullBackOff" || !strings.Contains(states[1].Message, "403") {
		t.Fatalf("second state wrong: %#v", states[1])
	}
}

// The whole point: given a mix, the fatal one wins and is reported.
func TestFirstFatalWaitingState(t *testing.T) {
	raw := "ContainerCreating\t\nErrImagePull\tdial tcp: i/o timeout\n"
	reason, fatal := firstFatalWaitingState(parseWaitingStates(raw))
	if !fatal {
		t.Fatal("expected the ErrImagePull entry to be fatal")
	}
	if !strings.Contains(reason, "ErrImagePull") || !strings.Contains(reason, "i/o timeout") {
		t.Fatalf("reason = %q", reason)
	}

	if _, fatal := firstFatalWaitingState(parseWaitingStates("ContainerCreating\t\nPodInitializing\t\n")); fatal {
		t.Fatal("no fatal state expected")
	}
}

// Gap found on the ACK run: fail-fast watched only container Waiting states, but an
// UNSCHEDULABLE Pod has no containerStatuses at all — the signal lives in
// status.conditions[type=PodScheduled].reason=Unschedulable. So `--wait` burned the
// full 5 minutes on "Insufficient memory" after the mirror DaemonSet had already
// rewritten hosts.toml, exactly the outage this was supposed to prevent.
func TestUnschedulableReasonIsFatal(t *testing.T) {
	msg := "0/2 nodes are available: 1 Insufficient memory, 1 node(s) didn't match " +
		"Pod's node affinity/selector."
	reason, fatal := blockingScheduleReason("Unschedulable", msg)
	if !fatal {
		t.Fatal("Unschedulable must be fatal — waiting cannot create capacity")
	}
	for _, want := range []string{"Unschedulable", "Insufficient memory"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason must carry %q: %s", want, reason)
		}
	}
}

// A Pod that simply has not been scheduled yet reports no reason. Must not abort.
func TestEmptyScheduleReasonIsNotFatal(t *testing.T) {
	if _, fatal := blockingScheduleReason("", ""); fatal {
		t.Fatal("no reason yet must not be fatal")
	}
	if _, fatal := blockingScheduleReason("SchedulerError", "transient"); fatal {
		t.Fatal("SchedulerError is transient, not terminal")
	}
}

// Parses `kubectl get pods -o jsonpath` for the PodScheduled condition:
// "<status>\t<reason>\t<message>" per Pod.
func TestParseScheduleStates(t *testing.T) {
	raw := "True\t\t\n" +
		"False\tUnschedulable\t0/2 nodes are available: 1 Insufficient memory\n"
	reason, fatal := firstFatalScheduleState(parseScheduleStates(raw))
	if !fatal {
		t.Fatalf("expected fatal, got reason=%q", reason)
	}
	if !strings.Contains(reason, "Insufficient memory") {
		t.Fatalf("reason=%q", reason)
	}
	if _, fatal := firstFatalScheduleState(parseScheduleStates("True\t\t\n")); fatal {
		t.Fatal("a scheduled Pod must not be fatal")
	}
}
