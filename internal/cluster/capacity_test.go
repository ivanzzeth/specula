package cluster

import (
	"strings"
	"testing"
)

// Why capacity matters when picking the pin node.
//
// On a real ACK cluster AutoPinHostname picked the first Ready worker and the
// install pinned Specula there with nodeSelector. Both nodes were 2 GiB
// `ecs.e-c1m1.large` instances whose allocatable memory (~1105Mi) was already 95%
// and 99% requested by ACK's own system components, so the 128Mi request fit
// nowhere. The result was a Pod stuck Pending behind
//
//	0/2 nodes are available: 1 Insufficient memory, 1 node(s) didn't match
//	Pod's node affinity/selector
//
// while the mirror DaemonSet had ALREADY rewritten hosts.toml on both nodes to
// point at a Specula that would never start — the worst possible half-state,
// because node pulls then fail with no public fallback by design.
//
// So: pick a node that actually fits, and when none does, say so with the numbers
// instead of pinning somewhere hopeless.
func TestPickPinNodeByHeadroomPrefersTheNodeThatFits(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "n1", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 1050}, // 55 free
		{Name: "n2", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 900},  // 205 free
		{Name: "n3", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 1104}, // 1 free
	}
	got, err := PickPinNodeByHeadroom(nodes, 128)
	if err != nil {
		t.Fatalf("expected a fit: %v", err)
	}
	if got != "n2" {
		t.Fatalf("picked %q, want n2 (most headroom)", got)
	}
}

// The failure has to carry the numbers — "no node available" sends the operator
// looking at the wrong thing (they reach for the image or the PVC first).
func TestPickPinNodeByHeadroomReportsHeadroomWhenNothingFits(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "cn-chengdu.172.25.180.141", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 1050},
		{Name: "cn-chengdu.172.25.180.142", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 1104},
	}
	_, err := PickPinNodeByHeadroom(nodes, 128)
	if err == nil {
		t.Fatal("expected an error when no node has room")
	}
	msg := err.Error()
	for _, want := range []string{"128Mi", "55Mi", "cn-chengdu.172.25.180.141"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must quote the numbers (%q): %s", want, msg)
		}
	}
}

// Not-ready and control-plane nodes are not candidates while a worker exists.
func TestPickPinNodeByHeadroomSkipsNotReadyAndPrefersWorkers(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "cp", Ready: true, Worker: false, AllocatableMi: 8000, RequestedMi: 100},
		{Name: "dead", Ready: false, Worker: true, AllocatableMi: 8000, RequestedMi: 0},
		{Name: "w", Ready: true, Worker: true, AllocatableMi: 1105, RequestedMi: 900},
	}
	got, err := PickPinNodeByHeadroom(nodes, 128)
	if err != nil {
		t.Fatal(err)
	}
	if got != "w" {
		t.Fatalf("picked %q, want the Ready worker w", got)
	}
}

// A single-node cluster (control plane only, common for minikube/kind) must still
// work — fall back to the control plane when no worker can host the Pod.
func TestPickPinNodeByHeadroomFallsBackToControlPlane(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "cp", Ready: true, Worker: false, AllocatableMi: 4000, RequestedMi: 500},
	}
	got, err := PickPinNodeByHeadroom(nodes, 128)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cp" {
		t.Fatalf("picked %q, want cp", got)
	}
}

// Unknown capacity (kubectl output we could not parse) must not be treated as
// "zero free" — that would refuse to install on a perfectly fine cluster. Treat it
// as a candidate of last resort instead.
func TestPickPinNodeByHeadroomTolerLatesUnknownCapacity(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "unknown", Ready: true, Worker: true, AllocatableMi: 0, RequestedMi: 0},
	}
	got, err := PickPinNodeByHeadroom(nodes, 128)
	if err != nil {
		t.Fatalf("unknown capacity must not block the install: %v", err)
	}
	if got != "unknown" {
		t.Fatalf("picked %q", got)
	}
}

// ─── parsing ─────────────────────────────────────────────────────────────────

// Node lines come from `kubectl get nodes -o jsonpath`:
//
//	<name>\t<labels-json>\t<Ready status>\t<allocatable memory>
//
// Allocatable memory arrives in Kubernetes quantity form (Ki/Mi/Gi or plain
// bytes) and must be normalised, or every comparison silently compares garbage.
func TestParseQuantityMi(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		// 1131504Ki is 1104.99Mi — truncation is deliberate: understating capacity is
		// the safe direction when deciding whether a request fits.
		{"1131504Ki", 1104},
		{"1105Mi", 1105},
		{"2Gi", 2048},
		{"1073741824", 1024}, // plain bytes
		{"", 0},
		{"garbage", 0},
	}
	for _, tc := range cases {
		if got := parseQuantityMi(tc.in); got != tc.want {
			t.Fatalf("parseQuantityMi(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseNodeCapacityLines(t *testing.T) {
	raw := "n1\t{\"node-role.kubernetes.io/control-plane\":\"\"}\tTrue\t8Gi\n" +
		"n2\t{\"kubernetes.io/os\":\"linux\"}\tTrue\t1131504Ki\n" +
		"n3\t{}\tFalse\t1131504Ki\n" +
		"\n"
	nodes := ParseNodeCapacityLines(raw)
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes: %#v", len(nodes), nodes)
	}
	if nodes[0].Worker {
		t.Fatal("control-plane label must not count as a worker")
	}
	if !nodes[1].Worker || !nodes[1].Ready || nodes[1].AllocatableMi != 1104 {
		t.Fatalf("n2 parsed wrong: %#v", nodes[1])
	}
	if nodes[2].Ready {
		t.Fatal("n3 is NotReady")
	}
}
