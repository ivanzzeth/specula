package integrate

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAddr is the Specula base URL used when --addr is omitted.
//
// Specula serves every protocol, the Admin API and the WebUI on ONE port (7733), so
// there is a single address to point clients at — an Ingress URL in a cluster, this
// loopback default on a node. https because plain http:// in hosts.toml against a
// TLS listener yields HTTP 400 / handshake failures on pull.
const DefaultAddr = "https://127.0.0.1:7733"

// normalizeAddr validates and canonicalizes a Specula base URL.
// Empty addr becomes DefaultAddr (https).
func normalizeAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid --addr %q (want https://host:port or http://host:port)", addr)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("invalid --addr scheme %q (want http or https)", u.Scheme)
	}
	u.Scheme = scheme
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// resolveDataPlaneAddr normalizes addr and, for http://, probes whether the
// port only speaks TLS. When TLS-only, auto-upgrades to https:// so hosts.toml
// does not silently embed a scheme that yields HTTP 400 on pull.
//
// skipProbe disables the network check (unit tests / offline dry-runs).
func resolveDataPlaneAddr(addr string, skipProbe bool) (resolved string, upgraded bool, err error) {
	resolved, err = normalizeAddr(addr)
	if err != nil {
		return "", false, err
	}
	if skipProbe || !strings.HasPrefix(resolved, "http://") {
		return resolved, false, nil
	}
	tlsOnly, probeErr := probeTLSOnlyEndpoint(resolved)
	if probeErr != nil {
		// Ambiguous network — keep http but do not invent https.
		return resolved, false, nil
	}
	if !tlsOnly {
		return resolved, false, nil
	}
	httpsAddr := "https://" + strings.TrimPrefix(resolved, "http://")
	return httpsAddr, true, nil
}

// probeTLSOnlyEndpoint reports whether host:port completes a TLS handshake
// while cleartext HTTP to /v2/ does not look like a Specula/registry reply.
func probeTLSOnlyEndpoint(httpAddr string) (tlsOnly bool, err error) {
	u, err := url.Parse(httpAddr)
	if err != nil {
		return false, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "443")
	}

	tlsOK := false
	dialer := &net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // probe only
	if err == nil {
		tlsOK = true
		_ = conn.Close()
	}

	httpOK := false
	client := &http.Client{
		Timeout: 1500 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, reqErr := http.NewRequest(http.MethodGet, strings.TrimRight(httpAddr, "/")+"/v2/", nil)
	if reqErr == nil {
		res, getErr := client.Do(req)
		if getErr == nil {
			_ = res.Body.Close()
			// Registry protocol: 200/401. Other codes (incl. 400 from TLS
			// mis-speak) do not count as a working cleartext data plane.
			if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusUnauthorized {
				httpOK = true
			}
		}
	}

	if tlsOK && !httpOK {
		return true, nil
	}
	return false, nil
}
