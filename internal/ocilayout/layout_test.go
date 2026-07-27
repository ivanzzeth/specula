package ocilayout

import "testing"

func TestResolve(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		pathPrefix string
		layout     string
		host       string
		want       string
		wantEr     bool
	}{
		{name: "empty"},
		{
			name:       "path_prefix wins",
			pathPrefix: "ddn-k8s/registry.k8s.io",
			layout:     HuaweiDDN,
			host:       "registry.k8s.io",
			want:       "ddn-k8s/registry.k8s.io",
		},
		{
			name:   "huawei-ddn remote host",
			layout: HuaweiDDN,
			host:   "registry.k8s.io",
			want:   "ddn-k8s/registry.k8s.io",
		},
		{
			name:   "huawei-ddn hub default",
			layout: HuaweiDDN,
			want:   "ddn-k8s/docker.io",
		},
		{
			name:   "unknown layout",
			layout: "nope",
			wantEr: true,
		},
		{
			name:       "trim slashes on path_prefix",
			pathPrefix: "/ddn-k8s/registry.k8s.io/",
			want:       "ddn-k8s/registry.k8s.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.pathPrefix, tc.layout, tc.host)
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
