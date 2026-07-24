// Package publishmeta extracts registry-advertised publish/upload timestamps
// for the maturity cool-down gate (PRD v0.10). Prefer these over HTTP
// Last-Modified, which CDNs may rewrite.
package publishmeta

import (
	"encoding/json"
	"strings"
	"time"
)

// FromNPMPackument reads packument.time[version] (ISO-8601).
func FromNPMPackument(packument []byte, version string) (time.Time, bool) {
	var doc struct {
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(packument, &doc); err != nil || doc.Time == nil {
		return time.Time{}, false
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return time.Time{}, false
	}
	raw, ok := doc.Time[version]
	if !ok {
		return time.Time{}, false
	}
	return parseTime(raw)
}

// VersionFromNPMTarball derives the semver key used in packument.time from a
// tarball filename (e.g. pkg=left-pad file=left-pad-1.3.0.tgz → 1.3.0).
func VersionFromNPMTarball(pkg, file string) string {
	base := strings.TrimSuffix(file, ".tgz")
	base = strings.TrimSuffix(base, ".tar.gz")
	name := pkg
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		name = pkg[i+1:]
	}
	prefix := name + "-"
	if strings.HasPrefix(base, prefix) {
		return strings.TrimPrefix(base, prefix)
	}
	return base
}

// FromPyPIWarehouseJSON reads releases[version][*].upload_time_iso_8601
// (Warehouse /pypi/<name>/json). Picks the earliest upload for that version.
// When version looks like a filename, falls back to filename match.
func FromPyPIWarehouseJSON(body []byte, versionOrFile string) (time.Time, bool) {
	key := strings.TrimSpace(versionOrFile)
	var doc struct {
		Releases map[string][]struct {
			Filename      string `json:"filename"`
			UploadTimeISO string `json:"upload_time_iso_8601"`
			UploadTime    string `json:"upload_time"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Releases == nil {
		return time.Time{}, false
	}
	if files, ok := doc.Releases[key]; ok && len(files) > 0 {
		return earliest(files)
	}
	var best time.Time
	found := false
	for _, files := range doc.Releases {
		for _, f := range files {
			if f.Filename != key {
				continue
			}
			t, ok := parseUpload(f.UploadTimeISO, f.UploadTime)
			if !ok {
				continue
			}
			if !found || t.Before(best) {
				best, found = t, true
			}
		}
	}
	return best, found
}

func earliest(files []struct {
	Filename      string `json:"filename"`
	UploadTimeISO string `json:"upload_time_iso_8601"`
	UploadTime    string `json:"upload_time"`
}) (time.Time, bool) {
	var best time.Time
	found := false
	for _, f := range files {
		t, ok := parseUpload(f.UploadTimeISO, f.UploadTime)
		if !ok {
			continue
		}
		if !found || t.Before(best) {
			best, found = t, true
		}
	}
	return best, found
}

func parseUpload(iso, legacy string) (time.Time, bool) {
	if t, ok := parseTime(iso); ok {
		return t, true
	}
	return parseTime(legacy)
}

// FromPyPISimpleJSON reads PEP 691 simple index files[].upload-time matching
// filename (ArtifactRef.Version for file refs is the filename).
func FromPyPISimpleJSON(body []byte, filename string) (time.Time, bool) {
	var doc struct {
		Files []struct {
			Filename   string `json:"filename"`
			UploadTime string `json:"upload-time"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return time.Time{}, false
	}
	filename = strings.TrimSpace(filename)
	for _, f := range doc.Files {
		if f.Filename != filename {
			continue
		}
		return parseTime(f.UploadTime)
	}
	return time.Time{}, false
}

// FromCargoCrateAPI reads crates.io API v1 crate version created_at
// (GET /api/v1/crates/<name>/<version>).
func FromCargoCrateAPI(body []byte) (time.Time, bool) {
	var doc struct {
		Version struct {
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return time.Time{}, false
	}
	if t, ok := parseTime(doc.Version.CreatedAt); ok {
		return t, true
	}
	return parseTime(doc.Version.UpdatedAt)
}

func parseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
