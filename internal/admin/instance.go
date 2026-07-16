package admin

import (
	"net"
	"net/http"
	"strings"
)

// handleInstance → GET /api/v1/instance.
//
// Serves the facts about *this deployment* that the browser cannot work out for
// itself. Today that is one thing, and it matters: the address of the OCI
// registry.
//
// The WebUI is served by the control plane; the registry answers on the data
// plane. Those differ by port locally and, behind an Ingress, usually by
// hostname too — so window.location.host is simply the wrong answer, and using
// it produced a Push guide whose `docker login` / `docker push` commands pointed
// at the control plane. (Worse, before the /v2/ guard the SPA answered GET /v2/
// with 200 and docker reported "Login Succeeded" against the wrong port.)
//
// Unauthenticated on purpose: it exposes only the address the client is about to
// be told to connect to, and the login screen may want it before a session
// exists.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, InstanceResponse{
		RegistryHost: s.registryHost(r),
	})
}

// registryHost resolves the public registry address: the configured value when
// set, else a best-effort derivation of "<host the browser used>:<data plane
// port>", which holds for a local single-binary run.
func (s *Server) registryHost(r *http.Request) string {
	if s.cfg != nil && s.cfg.Server.RegistryPublicHost != "" {
		return s.cfg.Server.RegistryPublicHost
	}
	if s.cfg == nil {
		return r.Host
	}

	// Derive: keep the hostname the browser is already using (it demonstrably
	// routes here) and swap in the data plane's port.
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	port := strings.TrimPrefix(s.cfg.Server.DataPlaneAddr, ":")
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	if port == "" {
		return r.Host
	}
	return net.JoinHostPort(host, port)
}
