package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// trafficWindow is the sliding wall-clock window used for "recent MB/s".
const trafficWindow = 60 * time.Second

// trafficEvent is one completed data-plane response observation.
type trafficEvent struct {
	at       time.Time
	protocol string
	bytes    int64
	dur      time.Duration
}

var (
	trafficMu     sync.Mutex
	trafficEvents []trafficEvent // newest appended; pruned on record/snapshot

	// Lifetime cumulatives (also mirrored to Prometheus counters/histograms).
	trafficBytes atomic.Uint64 // process-wide; per-proto below
	trafficReqs  atomic.Uint64
)

// perProtoCum is lifetime totals per protocol (for Snapshot without scraping Prom).
type perProtoCum struct {
	bytes uint64
	reqs  uint64
	nanos uint64 // sum of request durations
}

var trafficByProto sync.Map // string → *perProtoCum

func cumFor(protocol string) *perProtoCum {
	if v, ok := trafficByProto.Load(protocol); ok {
		return v.(*perProtoCum)
	}
	c := &perProtoCum{}
	actual, _ := trafficByProto.LoadOrStore(protocol, c)
	return actual.(*perProtoCum)
}

// recordTraffic notes one finished request for runtime throughput stats.
func recordTraffic(protocol string, bytes int64, dur time.Duration) {
	if bytes < 0 {
		bytes = 0
	}
	if dur < 0 {
		dur = 0
	}
	c := cumFor(protocol)
	atomic.AddUint64(&c.bytes, uint64(bytes))
	atomic.AddUint64(&c.reqs, 1)
	atomic.AddUint64(&c.nanos, uint64(dur.Nanoseconds()))
	trafficBytes.Add(uint64(bytes))
	trafficReqs.Add(1)

	now := time.Now()
	trafficMu.Lock()
	trafficEvents = append(trafficEvents, trafficEvent{
		at: now, protocol: protocol, bytes: bytes, dur: dur,
	})
	pruneTrafficLocked(now)
	trafficMu.Unlock()
}

func pruneTrafficLocked(now time.Time) {
	cut := now.Add(-trafficWindow)
	kept := make([]trafficEvent, 0, len(trafficEvents))
	for _, e := range trafficEvents {
		if !e.at.Before(cut) {
			kept = append(kept, e)
		}
	}
	trafficEvents = kept
}

// ProtoTraffic is one protocol's lifetime + recent-window throughput.
type ProtoTraffic struct {
	Protocol string `json:"protocol"`

	// Lifetime (since process start).
	BytesTotal            uint64  `json:"bytes_total"`
	RequestsTotal         uint64  `json:"requests_total"`
	DurationSecondsTotal  float64 `json:"duration_seconds_total"`
	TransferMBpsLifetime float64 `json:"transfer_mbps_lifetime"` // bytes / active request time

	// Sliding wall-clock window (default 60s).
	WindowSeconds         float64 `json:"window_seconds"`
	WindowBytes           uint64  `json:"window_bytes"`
	WindowRequests        uint64  `json:"window_requests"`
	WindowDurationSeconds float64 `json:"window_duration_seconds"`
	WindowMBpsWall        float64 `json:"window_mbps_wall"`     // bytes / 60s wall
	WindowMBpsTransfer    float64 `json:"window_mbps_transfer"` // bytes / sum(request durations) in window
}

// TrafficSnapshot is the live runtime throughput view.
type TrafficSnapshot struct {
	UptimeSeconds float64        `json:"uptime_seconds"`
	WindowSeconds float64        `json:"window_seconds"`
	BytesTotal    uint64         `json:"bytes_total"`
	RequestsTotal uint64         `json:"requests_total"`
	Protocols     []ProtoTraffic `json:"protocols"`
}

var processStart = time.Now()

// SnapshotTraffic returns lifetime + last-60s throughput per protocol.
func SnapshotTraffic() TrafficSnapshot {
	now := time.Now()
	uptime := now.Sub(processStart).Seconds()

	trafficMu.Lock()
	pruneTrafficLocked(now)
	events := append([]trafficEvent(nil), trafficEvents...)
	trafficMu.Unlock()

	type winAgg struct {
		bytes uint64
		reqs  uint64
		nanos uint64
	}
	win := map[string]*winAgg{}
	for _, e := range events {
		a := win[e.protocol]
		if a == nil {
			a = &winAgg{}
			win[e.protocol] = a
		}
		a.bytes += uint64(e.bytes)
		a.reqs++
		a.nanos += uint64(e.dur.Nanoseconds())
	}

	seen := map[string]struct{}{}
	var protos []ProtoTraffic
	trafficByProto.Range(func(k, v any) bool {
		p := k.(string)
		seen[p] = struct{}{}
		c := v.(*perProtoCum)
		pt := ProtoTraffic{
			Protocol:             p,
			BytesTotal:           atomic.LoadUint64(&c.bytes),
			RequestsTotal:        atomic.LoadUint64(&c.reqs),
			DurationSecondsTotal: float64(atomic.LoadUint64(&c.nanos)) / 1e9,
			WindowSeconds:        trafficWindow.Seconds(),
		}
		if pt.DurationSecondsTotal > 0 {
			pt.TransferMBpsLifetime = (float64(pt.BytesTotal) / (1024 * 1024)) / pt.DurationSecondsTotal
		}
		if w := win[p]; w != nil {
			pt.WindowBytes = w.bytes
			pt.WindowRequests = w.reqs
			pt.WindowDurationSeconds = float64(w.nanos) / 1e9
			pt.WindowMBpsWall = (float64(w.bytes) / (1024 * 1024)) / trafficWindow.Seconds()
			if pt.WindowDurationSeconds > 0 {
				pt.WindowMBpsTransfer = (float64(w.bytes) / (1024 * 1024)) / pt.WindowDurationSeconds
			}
		}
		protos = append(protos, pt)
		return true
	})
	for _, p := range AllProtocols {
		if _, ok := seen[p]; ok {
			continue
		}
		pt := ProtoTraffic{Protocol: p, WindowSeconds: trafficWindow.Seconds()}
		if w := win[p]; w != nil {
			pt.WindowBytes = w.bytes
			pt.WindowRequests = w.reqs
			pt.WindowDurationSeconds = float64(w.nanos) / 1e9
			pt.WindowMBpsWall = (float64(w.bytes) / (1024 * 1024)) / trafficWindow.Seconds()
			if pt.WindowDurationSeconds > 0 {
				pt.WindowMBpsTransfer = (float64(w.bytes) / (1024 * 1024)) / pt.WindowDurationSeconds
			}
		}
		protos = append(protos, pt)
	}
	sort.Slice(protos, func(i, j int) bool { return protos[i].Protocol < protos[j].Protocol })

	return TrafficSnapshot{
		UptimeSeconds: uptime,
		WindowSeconds: trafficWindow.Seconds(),
		BytesTotal:    trafficBytes.Load(),
		RequestsTotal: trafficReqs.Load(),
		Protocols:     protos,
	}
}
