package bootstrap

import "testing"

// parseImageRef feeds Specula's OCI path, and Specula routes non-Hub registries by
// PATH: /v2/<registry-host>/<repo>/manifests/<tag> (see oci.parseRemoteName). Stripping
// the host — which this function used to do for registry.k8s.io and any host-looking
// first segment — turns `registry.k8s.io/pause:3.10` into the Hub repo `pause`, which
// does not exist, so the warm fails with a 403 from the Hub mirror that reads like an
// upstream outage. Observed exactly that on a live cluster while the same Specula was
// serving docker.io/library/hello-world fine, and it sent me chasing the wrong problem.
func TestParseImageRefKeepsRegistryHost(t *testing.T) {
	cases := []struct{ in, path, tag string }{
		// Non-Hub registries keep the host: that IS the route.
		{"registry.k8s.io/pause:3.10", "registry.k8s.io/pause", "3.10"},
		{"registry.k8s.io/metrics-server/metrics-server:v0.7.2",
			"registry.k8s.io/metrics-server/metrics-server", "v0.7.2"},
		{"ghcr.io/owner/app:v1", "ghcr.io/owner/app", "v1"},
		{"quay.io/org/thing:latest", "quay.io/org/thing", "latest"},
		// Hub is the default namespace, so docker.io is the one host that is dropped.
		{"docker.io/library/hello-world:latest", "library/hello-world", "latest"},
		{"docker.io/bitnami/postgresql:16", "bitnami/postgresql", "16"},
		// Bare names are Hub official images.
		{"redis:7", "library/redis", "7"},
		{"nginx:latest", "library/nginx", "latest"},
		// org/name with no host is Hub too.
		{"bitnami/redis:7", "bitnami/redis", "7"},
		// A host with a port must not be mistaken for a tag.
		{"reg.example.com:5000/team/app:v2", "reg.example.com:5000/team/app", "v2"},
	}
	for _, tc := range cases {
		path, tag, err := parseImageRef(tc.in)
		if err != nil {
			t.Fatalf("parseImageRef(%q): %v", tc.in, err)
		}
		if path != tc.path || tag != tc.tag {
			t.Fatalf("parseImageRef(%q) = (%q, %q), want (%q, %q)", tc.in, path, tag, tc.path, tc.tag)
		}
	}
}

func TestParseImageRefRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", ":", "name:"} {
		if _, _, err := parseImageRef(in); err == nil {
			t.Fatalf("parseImageRef(%q) must fail", in)
		}
	}
}
