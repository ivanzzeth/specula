package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditAptRisks_HTTPSWithoutCA(t *testing.T) {
	root := t.TempDir()
	list := filepath.Join(root, "specula.list")
	ca := filepath.Join(root, "missing-specula.crt")
	require.NoError(t, os.WriteFile(list, []byte(`
deb [trusted=yes] https://127.0.0.1:7732/apt/ubuntu/ jammy main
`), 0o644))

	got := auditAptRisksAt(list, ca, "https://127.0.0.1:7732")
	require.NotEmpty(t, got)
	var found bool
	for _, r := range got {
		if r.Action == "risk" && strings.Contains(r.Detail, "certificate issuer is unknown") {
			found = true
			assert.Equal(t, "apt", r.Protocol)
			assert.Equal(t, ca, r.Path)
		}
	}
	assert.True(t, found, "%+v", got)
}

func TestAuditAptRisks_HTTPListVsHTTPSAddr(t *testing.T) {
	root := t.TempDir()
	list := filepath.Join(root, "specula.list")
	ca := filepath.Join(root, "specula.crt")
	require.NoError(t, os.WriteFile(list, []byte(`
deb [trusted=yes] http://127.0.0.1:7732/apt/ubuntu/ jammy main
`), 0o644))
	require.NoError(t, os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644))

	got := auditAptRisksAt(list, ca, "https://127.0.0.1:7732")
	var found bool
	for _, r := range got {
		if r.Action == "risk" && strings.Contains(r.Detail, "http://") && strings.Contains(r.Detail, "https://") {
			found = true
			assert.Equal(t, list, r.Path)
		}
	}
	assert.True(t, found, "scheme mismatch: %+v", got)
}

func TestAuditAptRisks_CleanHTTPSWithCA(t *testing.T) {
	root := t.TempDir()
	list := filepath.Join(root, "specula.list")
	ca := filepath.Join(root, "specula.crt")
	require.NoError(t, os.WriteFile(list, []byte(`
deb [trusted=yes] https://127.0.0.1:7732/apt/ubuntu/ jammy main
`), 0o644))
	require.NoError(t, os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644))

	got := auditAptRisksAt(list, ca, "https://127.0.0.1:7732")
	for _, r := range got {
		assert.NotEqual(t, "risk", r.Action, "%+v", r)
	}
}

func TestAuditAptRisks_NoListIsSilent(t *testing.T) {
	root := t.TempDir()
	got := auditAptRisksAt(filepath.Join(root, "absent.list"), filepath.Join(root, "absent.crt"), "https://127.0.0.1:7732")
	assert.Empty(t, got)
}
