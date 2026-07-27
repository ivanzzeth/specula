package cluster

import (
	"fmt"
	"strings"
	"testing"
)

// The note printed after `cluster install` claimed the nodes dial
// http://127.0.0.1:30733 no matter what. Under the hosted profile — internal
// LoadBalancer, no NodePort — the nodes actually dial the LB address, so the note
// named a port nothing listens on and sent me debugging the wrong thing.
func TestMirrorEndpointNoteReportsTheRealEndpoint(t *testing.T) {
	got := mirrorEndpointNote("http://172.25.180.149:7733", true)
	if !strings.Contains(got, "172.25.180.149:7733") {
		t.Errorf("note does not name the real endpoint: %q", got)
	}
	if strings.Contains(got, "30733") {
		t.Errorf("note still asserts the NodePort default: %q", got)
	}
	if strings.Contains(got, "NodePort") {
		t.Errorf("note calls a LoadBalancer address a NodePort: %q", got)
	}
}

// No mirror DaemonSet is not "nodes dial the NodePort" — it means the nodes are
// not pointed at Specula at all, which is the thing the operator must notice.
func TestMirrorEndpointNoteSaysNodesAreNotPointedAtSpecula(t *testing.T) {
	got := mirrorEndpointNote(fmt.Sprintf("http://127.0.0.1:%d", DefaultNodePort), false)
	for _, want := range []string{"NOT pointed at Specula", "mirror.enabled=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q does not contain %q", got, want)
		}
	}
}

// The DaemonSet args come back as a JSON array in jsonpath output.
func TestExtractEndpointArgReadsTheLiveDaemonSetArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`["bootstrap-mirror","write","--endpoint=http://172.25.180.149:7733","--certs-dir=/host-certs.d"]`,
			"http://172.25.180.149:7733"},
		{`["--endpoint=http://127.0.0.1:30733"]`, "http://127.0.0.1:30733"},
		{`["bootstrap-mirror","write","--certs-dir=/host-certs.d"]`, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractEndpointArg(c.in); got != c.want {
			t.Errorf("extractEndpointArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
