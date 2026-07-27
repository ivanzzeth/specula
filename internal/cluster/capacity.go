package cluster

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Capacity-aware pin-node selection.
//
// AutoPinHostname used to take the first Ready worker. On a real ACK cluster both
// workers were 2 GiB instances whose ~1105Mi allocatable memory was already 95%
// and 99% requested by ACK's own system components, so Specula's 128Mi request fit
// on neither. The install pinned it to one of them anyway and left the Pod Pending
// behind "Insufficient memory" — after the mirror DaemonSet had already pointed
// both nodes' hosts.toml at a Specula that would never start, which breaks node
// pulls because CN mode deliberately has no public fallback.
//
// Picking by headroom, and refusing with the numbers when nothing fits, turns that
// into an actionable message before anything is rewritten on the nodes.

// NodeCapacity is what pin selection needs to know about a node. AllocatableMi 0
// means "unknown" — that must never be read as "full".
type NodeCapacity struct {
	Name          string
	Ready         bool
	Worker        bool
	AllocatableMi int64
	RequestedMi   int64
}

// HeadroomMi is free allocatable memory, or -1 when capacity is unknown.
func (n NodeCapacity) HeadroomMi() int64 {
	if n.AllocatableMi <= 0 {
		return -1
	}
	return n.AllocatableMi - n.RequestedMi
}

// PickPinNodeByHeadroom returns the node with the most free memory that can host a
// Pod requesting needMi. Ready workers are preferred; the control plane is a
// fallback for single-node clusters. Nodes whose capacity could not be determined
// are last-resort candidates rather than rejected.
func PickPinNodeByHeadroom(nodes []NodeCapacity, needMi int64) (string, error) {
	ready := make([]NodeCapacity, 0, len(nodes))
	for _, n := range nodes {
		if n.Ready && strings.TrimSpace(n.Name) != "" {
			ready = append(ready, n)
		}
	}
	if len(ready) == 0 {
		return "", fmt.Errorf("no Ready node to pin Specula to")
	}

	pick := func(candidates []NodeCapacity) string {
		fits := make([]NodeCapacity, 0, len(candidates))
		for _, n := range candidates {
			if h := n.HeadroomMi(); h < 0 || h >= needMi {
				fits = append(fits, n)
			}
		}
		if len(fits) == 0 {
			return ""
		}
		// Most headroom first; unknown (-1) sorts last so a measured node wins.
		sort.SliceStable(fits, func(i, j int) bool {
			hi, hj := fits[i].HeadroomMi(), fits[j].HeadroomMi()
			if hi == hj {
				return fits[i].Name < fits[j].Name
			}
			if hi < 0 {
				return false
			}
			if hj < 0 {
				return true
			}
			return hi > hj
		})
		return fits[0].Name
	}

	workers := make([]NodeCapacity, 0, len(ready))
	for _, n := range ready {
		if n.Worker {
			workers = append(workers, n)
		}
	}
	if name := pick(workers); name != "" {
		return name, nil
	}
	if name := pick(ready); name != "" {
		return name, nil
	}

	// Nothing fits: report the headroom so the operator sees the real constraint.
	sort.SliceStable(ready, func(i, j int) bool { return ready[i].HeadroomMi() > ready[j].HeadroomMi() })
	var b strings.Builder
	fmt.Fprintf(&b, "no node has %dMi of memory free for Specula", needMi)
	for _, n := range ready {
		fmt.Fprintf(&b, "; %s has %dMi free (%dMi allocatable, %dMi requested)",
			n.Name, n.HeadroomMi(), n.AllocatableMi, n.RequestedMi)
	}
	b.WriteString(" — add capacity, or lower resources.requests.memory")
	return "", fmt.Errorf("%s", b.String())
}

// ParseNodeCapacityLines reads `kubectl get nodes -o jsonpath` output shaped as
// "<name>\t<labels-json>\t<Ready status>\t<allocatable memory>" per line.
func ParseNodeCapacityLines(raw string) []NodeCapacity {
	var out []NodeCapacity
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		name := strings.TrimSpace(f[0])
		if name == "" {
			continue
		}
		labels := f[1]
		n := NodeCapacity{
			Name:  name,
			Ready: strings.EqualFold(strings.TrimSpace(f[2]), "True"),
			Worker: !strings.Contains(labels, "node-role.kubernetes.io/control-plane") &&
				!strings.Contains(labels, "node-role.kubernetes.io/master"),
		}
		if len(f) >= 4 {
			n.AllocatableMi = parseQuantityMi(f[3])
		}
		out = append(out, n)
	}
	return out
}

// parseQuantityMi converts a Kubernetes memory quantity to MiB. Unparseable input
// yields 0, which callers treat as "unknown", never as "full".
func parseQuantityMi(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(q, "Ki"):
		q, mult = strings.TrimSuffix(q, "Ki"), 1024
	case strings.HasSuffix(q, "Mi"):
		q, mult = strings.TrimSuffix(q, "Mi"), 1024*1024
	case strings.HasSuffix(q, "Gi"):
		q, mult = strings.TrimSuffix(q, "Gi"), 1024*1024*1024
	case strings.HasSuffix(q, "Ti"):
		q, mult = strings.TrimSuffix(q, "Ti"), 1024*1024*1024*1024
	}
	v, err := strconv.ParseInt(strings.TrimSpace(q), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * mult / (1024 * 1024)
}

// DefaultRequestMi mirrors resources.requests.memory in the bootstrap chart. Pin
// selection has to know it: a node with less headroom than this cannot host
// Specula no matter how healthy it looks.
const DefaultRequestMi = 128

// AutoPinNode picks the pin node by memory headroom, summing per-node Pod requests
// the way the scheduler does. It returns an error naming the headroom when nothing
// fits — deliberately BEFORE helm runs, because the mirror DaemonSet rewrites
// hosts.toml on every node and pointing that at a Pod that can never schedule
// breaks node pulls (CN mode has no public fallback by design).
func AutoPinNode(kubeconfig, context string, needMi int64) (string, error) {
	if needMi <= 0 {
		needMi = DefaultRequestMi
	}
	out, err := kubectl(kubeconfig, context, "get", "nodes",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.labels}{"\t"}`+
			`{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\t"}`+
			`{.status.allocatable.memory}{"\n"}{end}`)
	if err != nil {
		return "", err
	}
	nodes := ParseNodeCapacityLines(string(out))
	if len(nodes) == 0 {
		return "", fmt.Errorf("no nodes returned")
	}
	requested := nodeMemoryRequestsMi(kubeconfig, context)
	for i := range nodes {
		nodes[i].RequestedMi = requested[nodes[i].Name]
	}
	return PickPinNodeByHeadroom(nodes, needMi)
}

// nodeMemoryRequestsMi sums memory requests of non-terminal Pods per node. A
// failure yields an empty map, which leaves headroom "unknown" rather than
// pretending the cluster is empty.
func nodeMemoryRequestsMi(kubeconfig, context string) map[string]int64 {
	out, err := kubectl(kubeconfig, context, "get", "pods", "--all-namespaces",
		"--field-selector=status.phase!=Succeeded,status.phase!=Failed",
		"-o", `jsonpath={range .items[*]}{.spec.nodeName}{"\t"}`+
			`{range .spec.containers[*]}{.resources.requests.memory}{","}{end}{"\n"}{end}`)
	if err != nil {
		return nil
	}
	sums := map[string]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		node, reqs, ok := strings.Cut(line, "\t")
		node = strings.TrimSpace(node)
		if !ok || node == "" {
			continue
		}
		for _, q := range strings.Split(reqs, ",") {
			sums[node] += parseQuantityMi(q)
		}
	}
	return sums
}
