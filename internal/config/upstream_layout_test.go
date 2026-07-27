package config

import "testing"

func TestResolveUpstreamPathPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		u      UpstreamConfig
		host   string
		want   string
		wantEr bool
	}{
		{name: "empty", u: UpstreamConfig{}, want: ""},
		{
			name: "path_prefix wins",
			u:    UpstreamConfig{PathPrefix: "ddn-k8s/registry.k8s.io", Layout: "huawei-ddn"},
			host: "registry.k8s.io",
			want: "ddn-k8s/registry.k8s.io",
		},
		{
			name: "huawei-ddn remote host",
			u:    UpstreamConfig{Layout: "huawei-ddn"},
			host: "registry.k8s.io",
			want: "ddn-k8s/registry.k8s.io",
		},
		{
			name: "huawei-ddn hub default",
			u:    UpstreamConfig{Layout: "huawei-ddn"},
			want: "ddn-k8s/docker.io",
		},
		{
			name:   "unknown layout",
			u:      UpstreamConfig{Layout: "nope"},
			wantEr: true,
		},
		{
			name: "trim slashes on path_prefix",
			u:    UpstreamConfig{PathPrefix: "/ddn-k8s/registry.k8s.io/"},
			want: "ddn-k8s/registry.k8s.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveUpstreamPathPrefix(tc.u, tc.host)
			if tc.wantEr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
