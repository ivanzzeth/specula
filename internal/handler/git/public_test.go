package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeForgeHosts_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		path    string
		status  int
		body    string
		wantPub bool
		wantErr bool
	}{
		{
			name:    "codeberg public",
			host:    "codeberg.org",
			path:    "owner/repo",
			status:  http.StatusOK,
			body:    `{"private":false}`,
			wantPub: true,
		},
		{
			name:    "codeberg private flag",
			host:    "codeberg.org",
			path:    "owner/private",
			status:  http.StatusOK,
			body:    `{"private":true}`,
			wantPub: false,
		},
		{
			name:    "codeberg 404",
			host:    "codeberg.org",
			path:    "owner/missing",
			status:  http.StatusNotFound,
			wantPub: false,
		},
		{
			name:    "bitbucket public",
			host:    "bitbucket.org",
			path:    "workspace/repo",
			status:  http.StatusOK,
			body:    `{"is_private":false}`,
			wantPub: true,
		},
		{
			name:    "bitbucket private",
			host:    "bitbucket.org",
			path:    "workspace/private",
			status:  http.StatusOK,
			body:    `{"is_private":true}`,
			wantPub: false,
		},
		{
			name:    "bitbucket 404",
			host:    "bitbucket.org",
			path:    "workspace/missing",
			status:  http.StatusNotFound,
			wantPub: false,
		},
		{
			name:    "sr.ht public page",
			host:    "git.sr.ht",
			path:    "owner/repo",
			status:  http.StatusOK,
			wantPub: true,
		},
		{
			name:    "sr.ht 404",
			host:    "git.sr.ht",
			path:    "owner/missing",
			status:  http.StatusNotFound,
			wantPub: false,
		},
		{
			name:    "sr.ht forbidden",
			host:    "git.sr.ht",
			path:    "owner/private",
			status:  http.StatusForbidden,
			wantPub: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != "" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer srv.Close()

			checker := probeCheckerWithServer(srv)
			ref := repoRef{Host: tt.host, ProjectPath: tt.path}
			pub, err := checker.probe(context.Background(), ref)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPub, pub)
		})
	}
}