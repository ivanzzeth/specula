// Package metrics provides an opt-in Prometheus metrics hook for Specula.
//
// The default facade does not pull Prometheus. Import this package when you
// want /metrics instrumentation:
//
//	import "github.com/ivanzzeth/specula/pkg/metrics"
//
//	mux.Handle("/gomod/", metrics.HTTPMiddleware("gomod", handler))
package metrics

import (
	"net/http"

	intmetrics "github.com/ivanzzeth/specula/internal/metrics"
)

// HTTPMiddleware wraps next with Specula request metrics for protocol.
func HTTPMiddleware(protocol string, next http.Handler) http.Handler {
	return intmetrics.Middleware(protocol, next)
}
