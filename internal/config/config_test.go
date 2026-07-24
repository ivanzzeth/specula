package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalYAML returns a YAML string that produces a valid Config.
// Tests that need to override specific fields should start from this base.
func minimalYAML() string {
	return `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 1800
auth:
  jwt_secret: ""
  cookie_secure: false
protocols:
  oci:
    mutable_ttl_seconds: 120
    upstreams:
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
`
}

// writeYAML writes content to a temp file and returns its path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "specula-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// setenv sets an env var for the duration of the test and restores it on cleanup.
func setenv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	require.NoError(t, os.Setenv(key, value))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// ── Load from example file ────────────────────────────────────────────────────

func TestLoad_ExampleFile(t *testing.T) {
	// The example file lives at the repo root; resolve relative to this test.
	// In Go the test working directory is the package directory.
	examplePath := filepath.Join("..", "..", "specula.example.yaml")
	cfg, err := config.Load(examplePath)
	require.NoError(t, err, "example file must load and validate cleanly")

	// Server
	assert.Equal(t, "0.0.0.0:7732", cfg.Server.DataPlaneAddr)
	assert.Equal(t, "0.0.0.0:7733", cfg.Server.ControlPlaneAddr)

	// Storage — example uses ~/.specula; Load expands ~ to $HOME.
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, "local", cfg.Storage.Blob.Driver)
	assert.Equal(t, filepath.Join(home, ".specula", "blobs"), cfg.Storage.Blob.Local.Root)
	assert.Equal(t, "sqlite", cfg.Storage.Meta.Driver)
	assert.Equal(t, filepath.Join(home, ".specula", "meta.db"), cfg.Storage.Meta.DSN)

	// Cache
	assert.Equal(t, int64(300), cfg.Cache.DefaultMutableTTLSeconds)
	assert.Equal(t, int64(1800), cfg.Cache.NegativeTTLSeconds)

	// All 8 protocols must be present.
	for _, proto := range []string{"oci", "pypi", "npm", "go", "apt", "helm", "tarball", "git"} {
		_, ok := cfg.Protocols[proto]
		assert.True(t, ok, "protocol %q missing from example config", proto)
	}
	require.NotNil(t, cfg.Protocols["git"].Git)
	assert.Equal(t, filepath.Join(home, ".specula", "git"), cfg.Protocols["git"].Git.MirrorDir)

	// OCI spot-check.
	oci := cfg.Protocols["oci"]
	assert.Len(t, oci.Upstreams, 3)
	require.NotNil(t, oci.MutableTTLSeconds)
	assert.Equal(t, int64(300), *oci.MutableTTLSeconds)
	assert.Contains(t, oci.Verification.Tiers, "tofu")
	assert.Contains(t, oci.Verification.Tiers, "checksum")

	// Go protocol must reach "signed" tier.
	goProto := cfg.Protocols["go"]
	assert.Contains(t, goProto.Verification.Tiers, "signed")

	// apt must always-revalidate.
	apt := cfg.Protocols["apt"]
	require.NotNil(t, apt.MutableTTLSeconds,
		"apt sets mutable_ttl_seconds: 0 explicitly — the sentinel must survive load as a\n\t\tnon-nil 0, not decay into 'unset'")
	assert.Equal(t, config.TTLAlwaysRevalidate, *apt.MutableTTLSeconds)

	// tarball must never-revalidate.
	tarball := cfg.Protocols["tarball"]
	require.NotNil(t, tarball.MutableTTLSeconds)
	assert.Equal(t, config.TTLNeverRevalidate, *tarball.MutableTTLSeconds)
}

// ── Sentinel parsing ──────────────────────────────────────────────────────────

func TestLoad_TTLSentinels(t *testing.T) {
	tests := []struct {
		name            string
		yamlFrag        string
		wantDefaultMTTL int64
		wantNegTTL      int64
		wantProtoMTTL   int64
	}{
		{
			name: "never_revalidate_sentinel",
			yamlFrag: `
cache:
  default_mutable_ttl_seconds: -1
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: -1
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantDefaultMTTL: config.TTLNeverRevalidate,
			wantNegTTL:      0,
			wantProtoMTTL:   config.TTLNeverRevalidate,
		},
		{
			name: "always_revalidate_sentinel",
			yamlFrag: `
cache:
  default_mutable_ttl_seconds: 0
  negative_ttl_seconds: 600
protocols:
  oci:
    mutable_ttl_seconds: 0
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantDefaultMTTL: config.TTLAlwaysRevalidate,
			wantNegTTL:      600,
			wantProtoMTTL:   config.TTLAlwaysRevalidate,
		},
		{
			name: "positive_ttl",
			yamlFrag: `
cache:
  default_mutable_ttl_seconds: 7200
  negative_ttl_seconds: 1800
protocols:
  oci:
    mutable_ttl_seconds: 120
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantDefaultMTTL: 7200,
			wantNegTTL:      1800,
			wantProtoMTTL:   120,
		},
	}

	// Build base YAML (server + storage) and merge with each test fragment.
	base := `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
`

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, base+tc.yamlFrag)
			cfg, err := config.Load(path)
			require.NoError(t, err)

			assert.Equal(t, tc.wantDefaultMTTL, cfg.Cache.DefaultMutableTTLSeconds,
				"default_mutable_ttl_seconds")
			assert.Equal(t, tc.wantNegTTL, cfg.Cache.NegativeTTLSeconds,
				"negative_ttl_seconds")
			got := cfg.Protocols["oci"].MutableTTLSeconds
			require.NotNil(t, got, "protocols.oci.mutable_ttl_seconds was set in YAML")
			assert.Equal(t, tc.wantProtoMTTL, *got,
				"protocols.oci.mutable_ttl_seconds")
		})
	}
}

// ── Environment variable overrides ────────────────────────────────────────────

func TestLoad_EnvOverride(t *testing.T) {
	path := writeYAML(t, minimalYAML())

	// Override the data plane address and the OCI mutable TTL via env.
	setenv(t, "SPECULA_SERVER__DATA_PLANE_ADDR", ":9000")
	setenv(t, "SPECULA_PROTOCOLS__OCI__MUTABLE_TTL_SECONDS", "999")

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, ":9000", cfg.Server.DataPlaneAddr,
		"env override SPECULA_SERVER__DATA_PLANE_ADDR must win")
	assert.Equal(t, ":8080", cfg.Server.ControlPlaneAddr,
		"unoveridden field must retain YAML value")
	require.NotNil(t, cfg.Protocols["oci"].MutableTTLSeconds)
	assert.Equal(t, int64(999), *cfg.Protocols["oci"].MutableTTLSeconds,
		"env override SPECULA_PROTOCOLS__OCI__MUTABLE_TTL_SECONDS must win")
}

func TestLoad_EnvOverride_StorageDriver(t *testing.T) {
	// Start with local blob driver, override to s3 + provide required bucket.
	yaml := `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
    s3:
      bucket: ""
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`
	path := writeYAML(t, yaml)

	setenv(t, "SPECULA_STORAGE__BLOB__DRIVER", "s3")
	setenv(t, "SPECULA_STORAGE__BLOB__S3__BUCKET", "my-bucket")

	cfg, err := config.Load(path)
	require.NoError(t, err)

	assert.Equal(t, "s3", cfg.Storage.Blob.Driver)
	assert.Equal(t, "my-bucket", cfg.Storage.Blob.S3.Bucket)
}

func TestLoad_EnvOverride_ControlPlane(t *testing.T) {
	path := writeYAML(t, minimalYAML())
	setenv(t, "SPECULA_SERVER__CONTROL_PLANE_ADDR", ":9090")
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Server.ControlPlaneAddr)
}

// ── Validation errors ──────────────────────────────────────────────────────────

func TestValidate_Errors(t *testing.T) {
	base := `
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`

	tests := []struct {
		name        string
		yaml        string
		wantErrMsgs []string // substrings that must appear in the error
	}{
		{
			name: "missing_server_addrs",
			yaml: `
server:
  data_plane_addr: ""
  control_plane_addr: ""
` + base,
			wantErrMsgs: []string{
				"server.data_plane_addr",
				"server.control_plane_addr",
			},
		},
		{
			name: "unknown_blob_driver",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: gcs
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"storage.blob.driver"},
		},
		{
			name: "s3_missing_bucket",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: s3
    s3:
      bucket: ""
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"storage.blob.s3.bucket"},
		},
		{
			name: "unknown_meta_driver",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: mysql
    dsn: root@tcp(localhost)/specula
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"storage.meta.driver"},
		},
		{
			name: "negative_negative_ttl",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: -5
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"cache.negative_ttl_seconds"},
		},
		{
			name: "protocol_no_upstreams",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams: []
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"protocols.oci", "at least one upstream"},
		},
		{
			name: "upstream_missing_base_url",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: ""
        priority: 1
        official: true
    verification:
      tiers: [checksum]
      quorum: 1
`,
			wantErrMsgs: []string{"protocols.oci.upstreams[0].base_url"},
		},
		{
			name: "unknown_tier",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [magic]
      quorum: 1
`,
			wantErrMsgs: []string{"unknown tier", "magic"},
		},
		{
			name: "consensus_without_quorum",
			yaml: `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 0
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams:
      - name: hub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
    verification:
      tiers: [consensus, checksum]
      quorum: 0
`,
			wantErrMsgs: []string{"protocols.oci.verification.quorum"},
		},
		{
			name: "multiple_errors_reported_together",
			yaml: `
server:
  data_plane_addr: ""
  control_plane_addr: ""
storage:
  blob:
    driver: bad
    local:
      root: ""
  meta:
    driver: bad
    dsn: ""
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: -1
protocols:
  oci:
    mutable_ttl_seconds: 60
    upstreams: []
    verification:
      tiers: [nope]
      quorum: 0
`,
			wantErrMsgs: []string{
				"server.data_plane_addr",
				"server.control_plane_addr",
				"storage.blob.driver",
				"storage.meta.driver",
				"cache.negative_ttl_seconds",
				"protocols.oci",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, tc.yaml)
			_, err := config.Load(path)
			require.Error(t, err, "expected validation error")

			msg := err.Error()
			for _, want := range tc.wantErrMsgs {
				assert.True(t, strings.Contains(msg, want),
					"error %q should contain %q", msg, want)
			}
		})
	}
}

// ── Validate called directly ───────────────────────────────────────────────────

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			DataPlaneAddr:    ":5000",
			ControlPlaneAddr: ":8080",
		},
		Storage: config.StorageConfig{
			Blob: config.BlobStorageConfig{
				Driver: "local",
				Local:  config.LocalBlobConfig{Root: "/tmp/blobs"},
			},
			Meta: config.MetaStorageConfig{
				Driver: "postgres",
				DSN:    "postgres://localhost/specula",
			},
		},
		Cache: config.CacheConfig{
			DefaultMutableTTLSeconds: 300,
			NegativeTTLSeconds:       1800,
		},
		Protocols: map[string]config.ProtocolConfig{
			"oci": {
				MutableTTLSeconds: config.TTLPtr(120),
				Upstreams: []config.UpstreamConfig{
					{Name: "hub", BaseURL: "https://registry-1.docker.io", Priority: 1, Official: true},
				},
				Verification: config.VerificationConfig{
					Tiers:  []string{"tofu", "checksum"},
					Quorum: 1,
				},
			},
			"go": {
				MutableTTLSeconds: config.TTLPtr(config.TTLNeverRevalidate),
				Upstreams: []config.UpstreamConfig{
					{Name: "goproxy-cn", BaseURL: "https://goproxy.cn", Priority: 1},
				},
				Verification: config.VerificationConfig{
					Tiers:  []string{"signed", "tofu", "checksum"},
					Quorum: 1,
				},
			},
		},
	}

	err := config.Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_S3Driver(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			DataPlaneAddr:    ":5000",
			ControlPlaneAddr: ":8080",
		},
		Storage: config.StorageConfig{
			Blob: config.BlobStorageConfig{
				Driver: "s3",
				S3: config.S3BlobConfig{
					Bucket:       "my-bucket",
					Endpoint:     "https://minio.internal:9000",
					Region:       "us-east-1",
					UsePathStyle: true,
				},
			},
			Meta: config.MetaStorageConfig{
				Driver: "sqlite",
				DSN:    "/tmp/meta.db",
			},
		},
		Cache: config.CacheConfig{
			DefaultMutableTTLSeconds: config.TTLNeverRevalidate,
			NegativeTTLSeconds:       0,
		},
		Protocols: map[string]config.ProtocolConfig{
			"oci": {
				Upstreams: []config.UpstreamConfig{
					{Name: "hub", BaseURL: "https://registry-1.docker.io", Priority: 1},
				},
				Verification: config.VerificationConfig{
					Tiers:  []string{"checksum"},
					Quorum: 1,
				},
			},
		},
	}

	err := config.Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_ConsensusRequiresQuorum(t *testing.T) {
	tests := []struct {
		name    string
		quorum  int
		wantErr bool
	}{
		{"quorum_zero", 0, true},
		{"quorum_negative", -1, true},
		{"quorum_one", 1, false},
		{"quorum_two", 2, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Server: config.ServerConfig{
					DataPlaneAddr: ":5000", ControlPlaneAddr: ":8080",
				},
				Storage: config.StorageConfig{
					Blob: config.BlobStorageConfig{
						Driver: "local",
						Local:  config.LocalBlobConfig{Root: "/tmp/blobs"},
					},
					Meta: config.MetaStorageConfig{Driver: "sqlite", DSN: "/tmp/meta.db"},
				},
				Cache: config.CacheConfig{NegativeTTLSeconds: 0},
				Protocols: map[string]config.ProtocolConfig{
					"npm": {
						Upstreams: []config.UpstreamConfig{
							{Name: "npmmirror", BaseURL: "https://registry.npmmirror.com", Priority: 1},
							{Name: "huawei", BaseURL: "https://repo.huaweicloud.com/repository/npm", Priority: 2},
						},
						Verification: config.VerificationConfig{
							Tiers:  []string{"consensus", "tofu"},
							Quorum: tc.quorum,
						},
					},
				},
			}

			err := config.Validate(cfg)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "quorum")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ── TTL sentinel constants ────────────────────────────────────────────────────

func TestTTLSentinelValues(t *testing.T) {
	assert.Equal(t, int64(-1), config.TTLNeverRevalidate)
	assert.Equal(t, int64(0), config.TTLAlwaysRevalidate)
	// Sentinels must be distinguishable from each other.
	assert.NotEqual(t, config.TTLNeverRevalidate, config.TTLAlwaysRevalidate)
}

// ── Missing file ─────────────────────────────────────────────────────────────

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path/specula.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config: load file")
}

// ── EnvPrefix constant exported ───────────────────────────────────────────────

func TestEnvPrefix(t *testing.T) {
	assert.Equal(t, "SPECULA_", config.EnvPrefix)
}

// TestLoad_PortsAreBakedIn guards the product's ports being real defaults rather
// than a suggestion that only exists in specula.example.yaml.
//
// Before this, a config omitting server.* did not start at all — the ports were
// documented but not built in, so every deployment had to restate them and any
// omission was a hard startup failure rather than a sensible default.
func TestLoad_PortsAreBakedIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	// Deliberately no `server:` block at all.
	require.NoError(t, os.WriteFile(path, []byte(`
storage:
  blob:
    driver: local
    local:
      root: /tmp/specula-test-blobs
  meta:
    driver: sqlite
    dsn: /tmp/specula-test.db
`), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err, "a config without a server block must start on the built-in ports")
	assert.Equal(t, config.DefaultDataPlaneAddr, cfg.Server.DataPlaneAddr)
	assert.Equal(t, config.DefaultControlPlaneAddr, cfg.Server.ControlPlaneAddr)
	assert.Equal(t, "0.0.0.0:7732", cfg.Server.DataPlaneAddr, "SPEC on a phone keypad; not 5000")
	assert.Equal(t, "0.0.0.0:7733", cfg.Server.ControlPlaneAddr, "not 8080")
}

// TestLoad_ExplicitPortsOverrideDefaults keeps the defaults from becoming a floor
// an operator cannot move.
func TestLoad_ExplicitPortsOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  data_plane_addr: ":19999"
  control_plane_addr: ":19998"
storage:
  blob: {driver: local, local: {root: /tmp/b}}
  meta: {driver: sqlite, dsn: /tmp/m.db}
`), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":19999", cfg.Server.DataPlaneAddr)
	assert.Equal(t, ":19998", cfg.Server.ControlPlaneAddr)
}

// ── BUG A: unknown config key silently ignored ─────────────────────────────
//
// RED test: misplaced sumdb under verification must be rejected.
// sumdb is a field of ProtocolConfig (sibling to upstreams/verification),
// NOT a field of VerificationConfig. Placing it under verification silently
// disabled the Go signed-tier anchor.
//
// BEFORE the fix: Load returns nil — sumdb is silently swallowed.
// AFTER the fix:  Load returns an error naming the unknown key.

// TestLoad_UnknownKey_SumDBMisplacedUnderVerification is the primary RED test
// for BUG A. sumdb belongs at protocols.go (ProtocolConfig.SumDB), not under
// protocols.go.verification (VerificationConfig). A typo here silently drops
// Go's signed-tier supply-chain anchor.
func TestLoad_UnknownKey_SumDBMisplacedUnderVerification(t *testing.T) {
	yaml := `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
protocols:
  go:
    mutable_ttl_seconds: 300
    upstreams:
      - name: goproxy-cn
        base_url: https://goproxy.cn
        priority: 1
        official: false
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      sumdb:
        url: https://sum.golang.google.cn
        policy: enforce
`
	path := writeYAML(t, yaml)
	_, err := config.Load(path)
	require.Error(t, err,
		"misplaced sumdb under verification must be rejected — "+
			"before the fix, Load returns nil and Go's signed tier is silently disabled")
	assert.Contains(t, err.Error(), "sumdb",
		"error must name the unknown key so the operator can fix the typo")
}

// TestLoad_UnknownKey_TopLevel verifies that a completely unknown top-level key
// is rejected (belt-and-braces: covers non-protocol misplacement).
func TestLoad_UnknownKey_TopLevel(t *testing.T) {
	yaml := minimalYAML() + `
bogus_key: this-should-not-exist
`
	path := writeYAML(t, yaml)
	_, err := config.Load(path)
	require.Error(t, err, "an unknown top-level YAML key must be rejected")
	assert.Contains(t, err.Error(), "bogus_key")
}

// ── BUG B: PRD §6 config model does not load ──────────────────────────────
//
// RED test: the YAML block in docs/PRD.md §6 must parse through config.Load.
// Before the fix the PRD uses a stale schema (bare-string upstreams, wrong
// verification sub-keys) that causes a startup failure when followed literally.
//
// This test is the permanent guard: if PRD §6 ever drifts from the real schema
// again, this test goes red and the PR cannot merge.

// prdSection6Header is the minimum required config that PRD §6 omits (it shows
// only the protocols block as an excerpt). We prepend it so config.Load can
// validate the full config.
const prdSection6Header = `
server:
  data_plane_addr: ":5000"
  control_plane_addr: ":8080"
storage:
  blob:
    driver: local
    local:
      root: /tmp/blobs
  meta:
    driver: sqlite
    dsn: /tmp/meta.db
`

// TestPRDSection6_YAMLLoads extracts the YAML block from docs/PRD.md §6 and
// runs it through config.Load. It is the permanent guard for BUG B: if the
// PRD's schema example drifts, this test goes red.
func TestPRDSection6_YAMLLoads(t *testing.T) {
	prdPath := filepath.Join("..", "..", "docs", "PRD.md")
	data, err := os.ReadFile(prdPath)
	require.NoError(t, err, "docs/PRD.md must be readable")

	yamlBlock := extractPRDSection6YAML(t, string(data))
	require.NotEmpty(t, yamlBlock, "PRD §6 must contain a ```yaml code block")

	// Prepend the required non-protocol config (PRD §6 is a protocols-only excerpt).
	fullYAML := prdSection6Header + yamlBlock
	path := writeYAML(t, fullYAML)

	_, loadErr := config.Load(path)
	require.NoError(t, loadErr,
		"PRD §6 YAML block must load and validate without error.\n"+
			"If this fails, PRD §6 is out of sync with the real config schema.\n"+
			"Fix: update docs/PRD.md §6 to match specula.example.yaml.")
}

// extractPRDSection6YAML finds the ## 6. section in PRD.md content and returns
// the body of its first ```yaml … ``` block.
func extractPRDSection6YAML(t *testing.T, content string) string {
	t.Helper()

	// Locate the ## 6. heading.
	sec6Idx := strings.Index(content, "\n## 6.")
	require.True(t, sec6Idx >= 0, "PRD.md must contain a '## 6.' section")
	rest := content[sec6Idx+1:] // skip the leading newline

	// Trim at the next section boundary so we only search within §6.
	if nextSec := strings.Index(rest[5:], "\n## "); nextSec >= 0 {
		rest = rest[:nextSec+5]
	}

	// Extract the yaml fenced block.
	const fence = "```yaml\n"
	start := strings.Index(rest, fence)
	require.True(t, start >= 0, "PRD §6 must contain a ```yaml block")
	rest = rest[start+len(fence):]
	end := strings.Index(rest, "```")
	require.True(t, end >= 0, "PRD §6 yaml block must have a closing ```")
	return rest[:end]
}
