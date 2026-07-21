package metrics

import (
	"fmt"
	"strings"
)

// FormatTrafficTable renders a human-readable runtime throughput view.
func FormatTrafficTable(s TrafficSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "specula traffic  uptime=%.0fs  window=%.0fs  total=%s in %d reqs\n",
		s.UptimeSeconds, s.WindowSeconds, formatTrafficBytes(s.BytesTotal), s.RequestsTotal)
	fmt.Fprintf(&b, "%-8s %12s %8s %10s %12s %12s %12s\n",
		"PROTO", "BYTES", "REQS", "XFER MB/s", "WIN BYTES", "WIN MB/s", "WIN XFER")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 88))
	for _, p := range s.Protocols {
		if p.BytesTotal == 0 && p.WindowBytes == 0 {
			continue // omit idle protocols for a readable live view
		}
		fmt.Fprintf(&b, "%-8s %12s %8d %10s %12s %12s %12s\n",
			p.Protocol,
			formatTrafficBytes(p.BytesTotal),
			p.RequestsTotal,
			formatTrafficRate(p.TransferMBpsLifetime),
			formatTrafficBytes(p.WindowBytes),
			formatTrafficRate(p.WindowMBpsWall),
			formatTrafficRate(p.WindowMBpsTransfer),
		)
	}
	fmt.Fprintf(&b, "\nXFER MB/s = bytes / active request time (transfer speed while streaming).\n")
	fmt.Fprintf(&b, "WIN MB/s  = bytes in last %.0fs / wall clock (service throughput).\n", s.WindowSeconds)
	fmt.Fprintf(&b, "WIN XFER  = bytes in window / sum of request durations in window.\n")
	return b.String()
}

func formatTrafficBytes(n uint64) string {
	switch {
	case n >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	case n >= 1024*1024:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatTrafficRate(v float64) string {
	if v <= 0 {
		return "—"
	}
	if v < 0.01 {
		return fmt.Sprintf("%.1f KiB/s", v*1024)
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
