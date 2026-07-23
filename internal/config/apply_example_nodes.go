package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// mergeYAMLPreservingComments merges overlay into base YAML bytes using
// yaml.Node trees so comments attached to base keys are retained. New keys
// copied from overlay keep the overlay's comments.
func mergeYAMLPreservingComments(baseRaw, overlayRaw []byte, opts mergeOptions) ([]byte, error) {
	var baseDoc, overDoc yaml.Node
	if err := yaml.Unmarshal(baseRaw, &baseDoc); err != nil {
		return nil, fmt.Errorf("parse base: %w", err)
	}
	if err := yaml.Unmarshal(overlayRaw, &overDoc); err != nil {
		return nil, fmt.Errorf("parse overlay: %w", err)
	}
	if baseDoc.Kind != yaml.DocumentNode || len(baseDoc.Content) == 0 {
		baseDoc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if overDoc.Kind != yaml.DocumentNode || len(overDoc.Content) == 0 {
		return nil, fmt.Errorf("overlay root must be a document")
	}
	baseRoot := baseDoc.Content[0]
	overRoot := overDoc.Content[0]
	if baseRoot.Kind != yaml.MappingNode {
		baseRoot = &yaml.Node{Kind: yaml.MappingNode}
		baseDoc.Content[0] = baseRoot
	}
	if overRoot.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("overlay root must be a mapping")
	}
	merged := mergeMappingNode(baseRoot, overRoot, opts)
	baseDoc.Content[0] = merged

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&baseDoc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mergeMappingNode(base, overlay *yaml.Node, opts mergeOptions) *yaml.Node {
	out := &yaml.Node{
		Kind:        yaml.MappingNode,
		Tag:         base.Tag,
		HeadComment: base.HeadComment,
		LineComment: base.LineComment,
		FootComment: base.FootComment,
		Style:       base.Style,
	}
	overIdx := mappingIndex(overlay)

	// Preserve base key order; then append new overlay keys.
	seen := map[string]struct{}{}
	for i := 0; i+1 < len(base.Content); i += 2 {
		keyNode := base.Content[i]
		valNode := base.Content[i+1]
		key := keyNode.Value
		seen[key] = struct{}{}
		overVal, hasOver := overIdx[key]
		if !hasOver {
			out.Content = append(out.Content, cloneNode(keyNode), cloneNode(valNode))
			continue
		}
		mergedVal := mergeValueNode(valNode, overVal, opts)
		out.Content = append(out.Content, cloneNode(keyNode), mergedVal)
	}
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		keyNode := overlay.Content[i]
		valNode := overlay.Content[i+1]
		key := keyNode.Value
		if _, ok := seen[key]; ok {
			continue
		}
		out.Content = append(out.Content, cloneNode(keyNode), cloneNode(valNode))
	}
	return out
}

func mergeValueNode(base, overlay *yaml.Node, opts mergeOptions) *yaml.Node {
	if base == nil || isNullNode(base) {
		return cloneNode(overlay)
	}
	if opts.FillEmpty && isEmptyNode(base) {
		return cloneNode(overlay)
	}
	if base.Kind == yaml.MappingNode && overlay.Kind == yaml.MappingNode {
		return mergeMappingNode(base, overlay, opts)
	}
	if base.Kind == yaml.SequenceNode && overlay.Kind == yaml.SequenceNode {
		return mergeSequenceNode(base, overlay, opts)
	}
	if opts.Overwrite {
		return cloneNode(overlay)
	}
	return cloneNode(base)
}

func mergeSequenceNode(base, overlay *yaml.Node, opts mergeOptions) *yaml.Node {
	if opts.Overwrite {
		return cloneNode(overlay)
	}
	if opts.FillEmpty && len(base.Content) == 0 {
		return cloneNode(overlay)
	}
	if namedMapSequence(base) || namedMapSequence(overlay) {
		return mergeNamedMapSequence(base, overlay, opts)
	}
	if allScalarStrings(base) && allScalarStrings(overlay) {
		return unionStringSequence(base, overlay)
	}
	return cloneNode(base)
}

func mergeNamedMapSequence(base, overlay *yaml.Node, opts mergeOptions) *yaml.Node {
	out := &yaml.Node{Kind: yaml.SequenceNode, Style: base.Style}
	order := []string{}
	byName := map[string]*yaml.Node{}
	keyByName := map[string]*yaml.Node{} // unused; values are full map nodes

	ingest := func(seq *yaml.Node, isOverlay bool) {
		for _, item := range seq.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			name := mappingString(item, "name")
			if name == "" {
				continue
			}
			if existing, hit := byName[name]; hit {
				if isOverlay {
					byName[name] = mergeMappingNode(existing, item, opts)
				}
				continue
			}
			byName[name] = cloneNode(item)
			order = append(order, name)
			keyByName[name] = item
		}
	}
	ingest(base, false)
	ingest(overlay, true)
	for _, name := range order {
		out.Content = append(out.Content, byName[name])
	}
	return out
}

func unionStringSequence(base, overlay *yaml.Node) *yaml.Node {
	out := &yaml.Node{Kind: yaml.SequenceNode, Style: base.Style}
	seen := map[string]struct{}{}
	add := func(seq *yaml.Node) {
		for _, n := range seq.Content {
			if n.Kind != yaml.ScalarNode {
				continue
			}
			if _, ok := seen[n.Value]; ok {
				continue
			}
			seen[n.Value] = struct{}{}
			out.Content = append(out.Content, cloneNode(n))
		}
	}
	add(base)
	add(overlay)
	return out
}

func mappingIndex(m *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if m == nil || m.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		out[m.Content[i].Value] = m.Content[i+1]
	}
	return out
}

func mappingString(m *yaml.Node, key string) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return strings.TrimSpace(m.Content[i+1].Value)
		}
	}
	return ""
}

func namedMapSequence(seq *yaml.Node) bool {
	if seq == nil || seq.Kind != yaml.SequenceNode || len(seq.Content) == 0 {
		return false
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode || mappingString(item, "name") == "" {
			return false
		}
	}
	return true
}

func allScalarStrings(seq *yaml.Node) bool {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false
	}
	for _, n := range seq.Content {
		if n.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}

func isNullNode(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null" || (n.Kind == yaml.ScalarNode && (n.Value == "null" || n.Value == "~"))
}

func isEmptyNode(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value == "" || n.Tag == "!!null"
	case yaml.SequenceNode, yaml.MappingNode:
		return len(n.Content) == 0
	default:
		return false
	}
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	if len(n.Content) > 0 {
		out.Content = make([]*yaml.Node, len(n.Content))
		for i, c := range n.Content {
			out.Content[i] = cloneNode(c)
		}
	}
	return &out
}

// filterExampleYAML restricts the embedded example to the given protocol
// section names under protocols.* (plus nested protocol-specific blocks).
// Empty sections → full example.
func filterExampleYAML(exampleRaw []byte, sections []string) ([]byte, error) {
	if len(sections) == 0 {
		return exampleRaw, nil
	}
	want := map[string]struct{}{}
	for _, s := range sections {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if s == "gomod" {
			s = "go"
		}
		if s == "docker" {
			s = "oci"
		}
		if s == "pip" {
			s = "pypi"
		}
		want[s] = struct{}{}
	}
	if len(want) == 0 {
		return exampleRaw, nil
	}

	root, err := decodeYAMLMap(exampleRaw)
	if err != nil {
		return nil, err
	}
	protos, _ := asStringMap(root["protocols"])
	if protos == nil {
		return exampleRaw, nil
	}
	filtered := map[string]any{}
	for name, body := range protos {
		if _, ok := want[name]; ok {
			filtered[name] = body
		}
	}
	// Overlay only protocols — do not clobber server/storage from example
	// when the operator asked for a protocol subset.
	out := map[string]any{"protocols": filtered}
	return yaml.Marshal(out)
}

// IntegrateHintsForChanges returns integrate commands to re-run after a
// config merge that touched client-facing multi-source settings.
func IntegrateHintsForChanges(added, changed []string, configPath string) []string {
	touched := map[string]struct{}{}
	mark := func(proto string) { touched[proto] = struct{}{} }
	consider := func(p string) {
		switch {
		case strings.Contains(p, "protocols.apt") || strings.HasPrefix(p, "protocols.apt"):
			mark("apt")
		case strings.Contains(p, "protocols.helm"):
			mark("helm")
		case strings.Contains(p, "protocols.conda"):
			mark("conda")
		case strings.Contains(p, "protocols.git"):
			mark("git")
		case strings.Contains(p, "protocols.cargo"):
			mark("cargo")
		case strings.Contains(p, "protocols.oci"):
			mark("oci")
		}
	}
	for _, p := range added {
		consider(p)
	}
	for _, p := range changed {
		consider(p)
	}
	if len(touched) == 0 {
		return nil
	}
	order := []string{"oci", "apt", "helm", "git", "conda", "cargo"}
	cfgFlag := ""
	if configPath != "" && configPath != "specula.yaml" {
		cfgFlag = " --config " + configPath
	} else if configPath != "" {
		cfgFlag = " --config " + configPath
	}
	var hints []string
	for _, p := range order {
		if _, ok := touched[p]; !ok {
			continue
		}
		sudo := ""
		if p == "apt" || p == "oci" {
			sudo = "sudo "
		}
		hints = append(hints, fmt.Sprintf("%sspecula integrate --protocols %s%s", sudo, p, cfgFlag))
	}
	return hints
}
