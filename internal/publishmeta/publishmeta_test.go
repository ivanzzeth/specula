package publishmeta_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/publishmeta"
)

func TestFromNPMPackument(t *testing.T) {
	body := []byte(`{"time":{"created":"2020-01-01T00:00:00.000Z","1.3.0":"2021-06-15T12:00:00.000Z","modified":"2022-01-01T00:00:00.000Z"}}`)
	ts, ok := publishmeta.FromNPMPackument(body, "1.3.0")
	require.True(t, ok)
	assert.Equal(t, 2021, ts.Year())
	assert.Equal(t, time.June, ts.Month())
	_, ok = publishmeta.FromNPMPackument(body, "9.9.9")
	assert.False(t, ok)
	_, ok = publishmeta.FromNPMPackument([]byte(`not-json`), "1.0.0")
	assert.False(t, ok)
	_, ok = publishmeta.FromNPMPackument(body, "")
	assert.False(t, ok)
	_, ok = publishmeta.FromNPMPackument([]byte(`{"time":{}}`), "1.0.0")
	assert.False(t, ok)
}

func TestVersionFromNPMTarball(t *testing.T) {
	assert.Equal(t, "1.3.0", publishmeta.VersionFromNPMTarball("left-pad", "left-pad-1.3.0.tgz"))
	assert.Equal(t, "2.0.0", publishmeta.VersionFromNPMTarball("@scope/pkg", "pkg-2.0.0.tgz"))
	assert.Equal(t, "1.0.0", publishmeta.VersionFromNPMTarball("pkg", "pkg-1.0.0.tar.gz"))
	assert.Equal(t, "weird", publishmeta.VersionFromNPMTarball("other", "weird.tgz"))
}

func TestFromPyPIWarehouseJSON(t *testing.T) {
	body := []byte(`{"releases":{"2.0.0":[
		{"filename":"flask-2.0.0.tar.gz","upload_time_iso_8601":"2021-05-11T14:00:00.000000Z"},
		{"filename":"flask-2.0.0-py3-none-any.whl","upload_time":"2021-05-11 13:00:00"}
	]}}`)
	ts, ok := publishmeta.FromPyPIWarehouseJSON(body, "2.0.0")
	require.True(t, ok)
	assert.Equal(t, 2021, ts.Year())
	// earliest of the two uploads
	assert.Equal(t, 13, ts.UTC().Hour())

	ts, ok = publishmeta.FromPyPIWarehouseJSON(body, "flask-2.0.0.tar.gz")
	require.True(t, ok)
	assert.Equal(t, 2021, ts.Year())

	_, ok = publishmeta.FromPyPIWarehouseJSON([]byte(`{}`), "2.0.0")
	assert.False(t, ok)
	_, ok = publishmeta.FromPyPIWarehouseJSON([]byte(`not-json`), "2.0.0")
	assert.False(t, ok)
	_, ok = publishmeta.FromPyPIWarehouseJSON(body, "no-such-file.whl")
	assert.False(t, ok)
}

func TestFromPyPISimpleJSON(t *testing.T) {
	body := []byte(`{"files":[{"filename":"pkg-1.0-py3-none-any.whl","upload-time":"2023-02-03T04:05:06Z"}]}`)
	ts, ok := publishmeta.FromPyPISimpleJSON(body, "pkg-1.0-py3-none-any.whl")
	require.True(t, ok)
	assert.Equal(t, 2023, ts.Year())
	_, ok = publishmeta.FromPyPISimpleJSON(body, "other.whl")
	assert.False(t, ok)
	_, ok = publishmeta.FromPyPISimpleJSON([]byte(`not-json`), "x.whl")
	assert.False(t, ok)
}

func TestFromCargoCrateAPI(t *testing.T) {
	body := []byte(`{"version":{"created_at":"2019-03-20T15:00:00.000000Z","updated_at":"2019-03-21T00:00:00.000000Z"}}`)
	ts, ok := publishmeta.FromCargoCrateAPI(body)
	require.True(t, ok)
	assert.Equal(t, 2019, ts.Year())
	assert.Equal(t, time.March, ts.Month())

	// Fall back to updated_at when created_at missing/invalid.
	ts, ok = publishmeta.FromCargoCrateAPI([]byte(`{"version":{"updated_at":"2020-01-02T03:04:05Z"}}`))
	require.True(t, ok)
	assert.Equal(t, 2020, ts.Year())

	_, ok = publishmeta.FromCargoCrateAPI([]byte(`not-json`))
	assert.False(t, ok)
	_, ok = publishmeta.FromCargoCrateAPI([]byte(`{"version":{}}`))
	assert.False(t, ok)
}
