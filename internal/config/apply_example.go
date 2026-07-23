package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ApplyExampleOptions controls how the embedded reference config is merged into
// an existing operator config file.
type ApplyExampleOptions struct {
	// Overwrite prefers example leaf values when both sides define a key.
	// Maps still deep-merge; named list entries still merge by name.
	Overwrite bool

	// FillEmpty replaces empty strings, empty slices, and empty maps in the
	// operator config with the corresponding example value.
	FillEmpty bool

	// DryRun computes the merge and change report without writing.
	DryRun bool

	// Backup writes path.bak.<UTC timestamp> before replacing path.
	// Ignored when DryRun is true. Default true when calling ApplyExample via CLI.
	Backup bool

	// NoBackup disables the backup even when Backup is the CLI default.
	NoBackup bool
}

// ApplyExampleResult summarizes a merge of the embedded example into path.
type ApplyExampleResult struct {
	Path       string
	BackupPath string
	Created    bool // true when path did not exist and was written from example alone
	DryRun     bool
	Added      []string // dotted paths newly present after merge
	Changed    []string // dotted paths whose value changed
	// Wrote is true when the config file was replaced on disk.
	Wrote bool
}

// ApplyExample merges the embedded ExampleYAML into the YAML file at path.
//
// Default policy (neither Overwrite nor FillEmpty):
//   - missing keys are copied from the example (additive upgrade)
//   - existing operator values win
//   - string lists are unioned (operator order first, then new example items)
//   - lists of maps with a "name" field are merged by name (operator entry wins;
//     missing keys inside an entry are filled from the example entry)
//
// The result is validated with Load+Validate before write (and on dry-run).
func ApplyExample(path string, opts ApplyExampleOptions) (*ApplyExampleResult, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("config: empty path")
	}

	exampleRoot, err := decodeYAMLMap(ExampleYAML)
	if err != nil {
		return nil, fmt.Errorf("config: parse embedded example: %w", err)
	}

	res := &ApplyExampleResult{Path: path, DryRun: opts.DryRun}
	var userRoot map[string]any
	raw, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		userRoot, err = decodeYAMLMap(raw)
		if err != nil {
			return nil, fmt.Errorf("config: parse %q: %w", path, err)
		}
	case os.IsNotExist(readErr):
		res.Created = true
		userRoot = map[string]any{}
	default:
		return nil, fmt.Errorf("config: read %q: %w", path, readErr)
	}

	before := cloneMap(userRoot)
	merged := deepMerge(userRoot, exampleRoot, mergeOptions{
		Overwrite: opts.Overwrite,
		FillEmpty: opts.FillEmpty,
	})
	res.Added, res.Changed = diffMaps(before, merged)

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("config: marshal merged yaml: %w", err)
	}

	// Validate via a temp file so Load's file provider works unchanged.
	tmp, err := os.CreateTemp(filepath.Dir(path), "specula-apply-example-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("config: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("config: close temp: %w", err)
	}
	if _, err := Load(tmpPath); err != nil {
		return nil, fmt.Errorf("config: merged result failed validation: %w", err)
	}

	if opts.DryRun {
		return res, nil
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("config: mkdir %q: %w", dir, err)
		}
	}

	doBackup := opts.Backup && !opts.NoBackup && !res.Created
	if doBackup {
		if _, err := os.Stat(path); err == nil {
			bak := path + ".bak." + time.Now().UTC().Format("20060102T150405Z")
			if err := copyFile(path, bak); err != nil {
				return nil, fmt.Errorf("config: backup: %w", err)
			}
			res.BackupPath = bak
		}
	}

	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, fmt.Errorf("config: write %q: %w", path, err)
	}
	res.Wrote = true
	return res, nil
}

type mergeOptions struct {
	Overwrite bool
	FillEmpty bool
}

func decodeYAMLMap(raw []byte) (map[string]any, error) {
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("root must be a mapping, got %T", v)
	}
	return m, nil
}

// deepMerge returns a new map: keys from overlay are merged into base per opts.
// Neither input is mutated.
func deepMerge(base, overlay map[string]any, opts mergeOptions) map[string]any {
	out := cloneMap(base)
	for k, ov := range overlay {
		bv, has := out[k]
		if !has || isNull(bv) {
			out[k] = cloneAny(ov)
			continue
		}
		if opts.FillEmpty && isEmptyValue(bv) {
			out[k] = cloneAny(ov)
			continue
		}
		bMap, bOK := asStringMap(bv)
		oMap, oOK := asStringMap(ov)
		if bOK && oOK {
			out[k] = deepMerge(bMap, oMap, opts)
			continue
		}
		bList, bListOK := asAnySlice(bv)
		oList, oListOK := asAnySlice(ov)
		if bListOK && oListOK {
			out[k] = mergeSlices(bList, oList, opts)
			continue
		}
		if opts.Overwrite {
			out[k] = cloneAny(ov)
		}
		// else keep base
	}
	return out
}

func mergeSlices(base, overlay []any, opts mergeOptions) []any {
	if opts.Overwrite {
		return cloneSlice(overlay)
	}
	if opts.FillEmpty && len(base) == 0 {
		return cloneSlice(overlay)
	}
	if namedMaps(base) || namedMaps(overlay) {
		return mergeNamedMaps(base, overlay, opts)
	}
	if allStrings(base) && allStrings(overlay) {
		return unionStrings(base, overlay)
	}
	// Heterogeneous / unstructured lists: keep operator list.
	return cloneSlice(base)
}

func mergeNamedMaps(base, overlay []any, opts mergeOptions) []any {
	order := make([]string, 0, len(base)+len(overlay))
	byName := map[string]map[string]any{}

	addNew := func(m map[string]any, name string) {
		byName[name] = cloneMap(m)
		order = append(order, name)
	}

	for _, it := range base {
		m, ok := asStringMap(it)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, hit := byName[name]; hit {
			continue
		}
		addNew(m, name)
	}
	for _, it := range overlay {
		m, ok := asStringMap(it)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if existing, hit := byName[name]; hit {
			byName[name] = deepMerge(existing, m, mergeOptions{
				Overwrite: opts.Overwrite,
				FillEmpty: opts.FillEmpty,
			})
			continue
		}
		addNew(m, name)
	}

	out := make([]any, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

func unionStrings(base, overlay []any) []any {
	seen := map[string]struct{}{}
	out := make([]any, 0, len(base)+len(overlay))
	add := func(items []any) {
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				continue
			}
			if _, hit := seen[s]; hit {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	add(base)
	add(overlay)
	return out
}

func namedMaps(items []any) bool {
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		m, ok := asStringMap(it)
		if !ok {
			return false
		}
		name, _ := m["name"].(string)
		if strings.TrimSpace(name) == "" {
			return false
		}
	}
	return true
}

func allStrings(items []any) bool {
	if len(items) == 0 {
		return true
	}
	for _, it := range items {
		if _, ok := it.(string); !ok {
			return false
		}
	}
	return true
}

func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			sk, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[sk] = val
		}
		return out, true
	default:
		return nil, false
	}
}

func asAnySlice(v any) ([]any, bool) {
	switch s := v.(type) {
	case []any:
		return s, true
	default:
		rv := reflect.ValueOf(v)
		if !rv.IsValid() || rv.Kind() != reflect.Slice {
			return nil, false
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
}

func isNull(v any) bool { return v == nil }

func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case map[any]any:
		return len(x) == 0
	default:
		rv := reflect.ValueOf(v)
		if rv.IsValid() && rv.Kind() == reflect.Slice {
			return rv.Len() == 0
		}
		return false
	}
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneSlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	if v == nil {
		return nil
	}
	if m, ok := asStringMap(v); ok {
		return cloneMap(m)
	}
	if s, ok := asAnySlice(v); ok {
		return cloneSlice(s)
	}
	return v
}

func diffMaps(before, after map[string]any) (added, changed []string) {
	var walk func(prefix string, b, a map[string]any)
	walk = func(prefix string, b, a map[string]any) {
		keys := map[string]struct{}{}
		for k := range b {
			keys[k] = struct{}{}
		}
		for k := range a {
			keys[k] = struct{}{}
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			bv, bHas := b[k]
			av, aHas := a[k]
			switch {
			case !bHas && aHas:
				added = append(added, path)
			case bHas && aHas:
				bm, bOK := asStringMap(bv)
				am, aOK := asStringMap(av)
				if bOK && aOK {
					walk(path, bm, am)
					continue
				}
				if !reflect.DeepEqual(normalize(bv), normalize(av)) {
					changed = append(changed, path)
				}
			}
		}
	}
	walk("", before, after)
	return added, changed
}

func normalize(v any) any {
	if m, ok := asStringMap(v); ok {
		return cloneMap(m)
	}
	if s, ok := asAnySlice(v); ok {
		return cloneSlice(s)
	}
	return v
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o644)
}

// FormatApplyExampleReport renders a human-readable summary for the CLI.
func FormatApplyExampleReport(r *ApplyExampleResult) string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("dry-run: no files written\n")
	}
	if r.Created {
		b.WriteString(fmt.Sprintf("would create %s from embedded example\n", r.Path))
	} else {
		b.WriteString(fmt.Sprintf("target: %s\n", r.Path))
	}
	if r.BackupPath != "" {
		b.WriteString(fmt.Sprintf("backup: %s\n", r.BackupPath))
	}
	if len(r.Added) == 0 && len(r.Changed) == 0 {
		b.WriteString("no changes (already up to date with example keys)\n")
		return b.String()
	}
	if len(r.Added) > 0 {
		b.WriteString(fmt.Sprintf("added (%d):\n", len(r.Added)))
		for _, p := range r.Added {
			b.WriteString("  + " + p + "\n")
		}
	}
	if len(r.Changed) > 0 {
		b.WriteString(fmt.Sprintf("changed (%d):\n", len(r.Changed)))
		for _, p := range r.Changed {
			b.WriteString("  ~ " + p + "\n")
		}
	}
	if r.Wrote {
		b.WriteString("wrote merged config (YAML comments from the old file are not preserved)\n")
	}
	return b.String()
}
