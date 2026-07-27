package cluster

import (
	"errors"
	"strings"
	"testing"
)

// Why the probe goes through the API server instead of a node IP + NodePort:
//
// The old default resolved a node InternalIP and curl'd <nodeIP>:30732/healthz
// from the operator's machine. That is unreachable in the two places doctor is
// most needed — a macOS/docker-driver minikube (observed: "curl (52) Empty reply
// from server" while the node itself answered 200) and a laptop pointed at a
// managed cloud cluster whose nodes live inside a VPC. Both report a healthy
// cluster as broken. The API server can always reach the Service, and kubectl is
// already a hard dependency, so the proxy path works everywhere and drops the
// curl requirement.
func TestServiceProxyPath(t *testing.T) {
	got := serviceProxyPath("specula-boot", "boot-specula-bootstrap", 7732, "healthz")
	want := "/api/v1/namespaces/specula-boot/services/boot-specula-bootstrap:7732/proxy/healthz"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestServiceProxyPathNormalisesSubpath(t *testing.T) {
	// A leading slash on the sub-path must not produce a doubled slash, and "v2/"
	// must keep its trailing slash (the registry handshake is served on /v2/).
	got := serviceProxyPath("ns", "svc", 80, "/v2/")
	want := "/api/v1/namespaces/ns/services/svc:80/proxy/v2/"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// /v2/ answers 401 BY DESIGN (Docker token handshake), and `kubectl get --raw`
// surfaces that as a non-zero exit with a credentials message. Treating any error
// as failure would make a correctly-working registry look broken; treating any
// error as success would hide a Service with no endpoints. So classify.
func TestClassifyV2ProbeAuthChallengeIsHealthy(t *testing.T) {
	// Exact text observed from kubectl against a live Specula service.
	out := "error: You must be logged in to the server (the server has asked for the client to provide credentials)"
	code, err := classifyV2Probe(out, errors.New("exit status 1"))
	if err != nil {
		t.Fatalf("auth challenge must be healthy, got %v", err)
	}
	if code != "401" {
		t.Fatalf("code = %q, want 401", code)
	}
}

func TestClassifyV2ProbeSuccessIs200(t *testing.T) {
	code, err := classifyV2Probe("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != "200" {
		t.Fatalf("code = %q, want 200", code)
	}
}

func TestClassifyV2ProbeUnreachableIsFailure(t *testing.T) {
	unreachable := []string{
		`Error from server (ServiceUnavailable): no endpoints available for service "boot-specula-bootstrap"`,
		`Error from server (NotFound): services "boot-specula-bootstrap" not found`,
		`error: dial tcp 10.96.0.1:443: connect: connection refused`,
		`Error from server: net/http: TLS handshake timeout`,
		`error: dial tcp: lookup api.example.com: no such host`,
	}
	for _, out := range unreachable {
		if _, err := classifyV2Probe(out, errors.New("exit status 1")); err == nil {
			t.Fatalf("must be reported as a failure: %s", out)
		}
	}
}

// The failure message has to carry kubectl's own output, or an operator sees
// "exit status 1" and nothing else.
func TestClassifyV2ProbeErrorCarriesOutput(t *testing.T) {
	out := `Error from server (ServiceUnavailable): no endpoints available for service "x"`
	_, err := classifyV2Probe(out, errors.New("exit status 1"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no endpoints available") {
		t.Fatalf("error must quote kubectl output, got: %v", err)
	}
}
