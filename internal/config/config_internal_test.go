package config

// config_internal_test.go covers the unexported helpers that cannot be reached
// from the external test package (config_test):
//
//   - envProvider.ReadBytes: returns nil, nil (the koanf no-op path).
//   - insertNested with an empty keys slice: documented dead code — the
//     Read() caller uses strings.Split which always returns ≥1 element, so
//     len(keys)==0 is unreachable in production.  We exercise it directly to
//     keep the coverage honest (and to confirm it does not panic).

import (
	"testing"
)

// TestEnvProvider_ReadBytes verifies that the ReadBytes method returns nil, nil.
// koanf resolves providers that satisfy the no-bytes interface by calling Read()
// instead; ReadBytes is a no-op stub that satisfies the interface signature.
func TestEnvProvider_ReadBytes(t *testing.T) {
	ep := newEnvProvider("TEST_")
	data, err := ep.ReadBytes()
	if data != nil {
		t.Errorf("ReadBytes: data = %v; want nil", data)
	}
	if err != nil {
		t.Errorf("ReadBytes: err = %v; want nil", err)
	}
}

// TestInsertNested_EmptyKeys exercises the len(keys)==0 early-return branch of
// insertNested.  This branch is dead code in production — strings.Split always
// returns at least one element — but the guard exists as a defensive nil-check.
// Calling it with an empty slice must not panic and must leave the map unchanged.
func TestInsertNested_EmptyKeys(t *testing.T) {
	m := map[string]any{"existing": "value"}
	insertNested(m, []string{}, "should-not-be-inserted")
	if len(m) != 1 {
		t.Errorf("insertNested with empty keys mutated the map: %v", m)
	}
	if m["existing"] != "value" {
		t.Errorf("insertNested with empty keys corrupted existing entry: %v", m["existing"])
	}
}

// TestInsertNested_SingleKey covers the len(keys)==1 leaf-set branch.
func TestInsertNested_SingleKey(t *testing.T) {
	m := map[string]any{}
	insertNested(m, []string{"leaf"}, "val")
	if m["leaf"] != "val" {
		t.Errorf("insertNested single key: got %v; want val", m["leaf"])
	}
}

// TestInsertNested_NestedKey covers the recursive map-creation branch.
func TestInsertNested_NestedKey(t *testing.T) {
	m := map[string]any{}
	insertNested(m, []string{"a", "b", "c"}, "deep")
	ab, ok := m["a"].(map[string]any)
	if !ok {
		t.Fatalf("intermediate key 'a' not a map: %T", m["a"])
	}
	bc, ok := ab["b"].(map[string]any)
	if !ok {
		t.Fatalf("intermediate key 'b' not a map: %T", ab["b"])
	}
	if bc["c"] != "deep" {
		t.Errorf("leaf key 'c' = %v; want deep", bc["c"])
	}
}

// TestInsertNested_ExistingSubMap verifies that an already-existing intermediate
// map is reused rather than replaced.
func TestInsertNested_ExistingSubMap(t *testing.T) {
	existing := map[string]any{"x": "old"}
	m := map[string]any{"a": existing}

	insertNested(m, []string{"a", "y"}, "new")
	sub, ok := m["a"].(map[string]any)
	if !ok {
		t.Fatalf("key 'a' not a map after insert: %T", m["a"])
	}
	if sub["x"] != "old" {
		t.Errorf("existing sub-map key lost: x = %v; want old", sub["x"])
	}
	if sub["y"] != "new" {
		t.Errorf("new key not inserted: y = %v; want new", sub["y"])
	}
}
