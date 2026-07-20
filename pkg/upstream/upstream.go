// Package upstream re-exports the fallback-chain upstream client.
package upstream

import intupstream "github.com/ivanzzeth/specula/internal/upstream"

type (
	Client        = intupstream.Client
	Upstream      = intupstream.Upstream
	StatusError   = intupstream.StatusError
	RequestOption = intupstream.RequestOption
	Runtime       = intupstream.Runtime
	Registry      = intupstream.Registry
)

// NewClient constructs the default fallback-chain upstream Client.
func NewClient() Client {
	return intupstream.NewClient()
}

// NewClientWithRuntime constructs a Client bound to a per-protocol Runtime.
func NewClientWithRuntime(rt *Runtime) Client {
	return intupstream.NewClientWithRuntime(rt)
}

// NewRuntime constructs an empty per-protocol Runtime.
func NewRuntime(protocol string) *Runtime {
	return intupstream.NewRuntime(protocol)
}

// NewRegistry constructs a multi-protocol upstream Runtime registry.
func NewRegistry() *Registry {
	return intupstream.NewRegistry()
}

// WithOCIManifestAccept sets the Accept header for OCI manifest negotiation.
func WithOCIManifestAccept() RequestOption {
	return intupstream.WithOCIManifestAccept()
}
