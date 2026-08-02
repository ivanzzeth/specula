package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// Protocol defaults: every protocol Specula implements is served unless the
// operator switches it off.
//
// This used to be the other way round — a handler was registered only if the
// config happened to name its protocol — which meant a config that configured
// `protocols.oci` (every chart, every ConfigMap we ship) silently served OCI and
// 404'd /npm, /pypi, /helm, /go and /apt. The binary supported all of them; the
// deployment just never said so. For a product whose entire thesis is "the
// upstreams you need are unreachable from CN", shipping a mirror that quietly
// mirrors one protocol out of twelve is the worst kind of default.
//
// The table is not written out again here. It is parsed from the embedded
// example.yaml, which already carries a maintained China-first upstream chain
// with an official upstream last, per-protocol TTLs and verification tiers. One
// table, one place to keep current: edit example.yaml and the built-in defaults
// move with it.

// gitIsHAUnsafe names the protocol that must not be switched on implicitly under
// HA. The git handler keeps bare mirrors on local disk; with more than one
// replica each replica would answer from its own clone, so it is opt-in only.
const gitIsHAUnsafe = "git"

// DefaultProtocols returns the built-in protocol table: the protocols Specula
// serves when the config does not mention them. The result is a fresh copy the
// caller may mutate.
func DefaultProtocols() (map[string]ProtocolConfig, error) {
	raw, err := yaml.Parser().Unmarshal(ExampleYAML)
	if err != nil {
		return nil, fmt.Errorf("config: parse embedded example: %w", err)
	}

	k := koanf.New(".")
	if err := k.Load(confmap.Provider(raw, "."), nil); err != nil {
		return nil, fmt.Errorf("config: load embedded example: %w", err)
	}

	var table map[string]ProtocolConfig
	dc := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
		// The embedded example is ours: a key it carries that ProtocolConfig has
		// no field for is a bug in this repo, not operator error, and it should
		// surface the first time anyone loads a config rather than silently
		// dropping a setting from every default.
		ErrorUnused: true,
	}
	if err := k.UnmarshalWithConf("protocols", &table, koanf.UnmarshalConf{DecoderConfig: dc}); err != nil {
		return nil, fmt.Errorf("config: unmarshal embedded example protocols: %w", err)
	}

	// A default with no upstream chain cannot serve anything, and would trip
	// Validate's "at least one upstream is required". example.yaml carries such
	// blocks as documentation of the shape (helm/apt/tarball catalogues an
	// operator is expected to fill); they stay opt-in.
	for name, pc := range table {
		if len(pc.Upstreams) == 0 {
			delete(table, name)
		}
	}
	return table, nil
}

// DefaultProtocolNames lists the protocols DefaultProtocols enables, sorted.
func DefaultProtocolNames() ([]string, error) {
	table, err := DefaultProtocols()
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(table)), nil
}

// applyProtocolDefaults fills in protocols the config does not mention and drops
// the ones it disables. It runs after unmarshal and before Validate, so a
// defaulted protocol is validated exactly like a hand-written one.
//
// wroteUpstreams reports whether the config set protocols.<name>.upstreams at
// any layer (file or env). It exists to keep `upstreams: []` meaning what it
// says: an operator who writes an empty chain gets Validate's "at least one
// upstream is required" rather than a silent substitution of ours. Only an
// ABSENT key inherits the built-in chain. nil means "nothing was explicit",
// which is the right reading for a Config assembled in code.
//
// Precedence, narrowest first:
//
//   - `enabled: false` removes the protocol, defaulted or not. Downstream code
//     tests presence in the map, so a disabled protocol is an absent one and no
//     handler is registered.
//   - a protocol the operator wrote wins as written.
//   - a block with no upstreams key inherits the default chain, so one that just
//     retunes mutable_ttl_seconds does not have to restate the mirror list.
//   - everything else comes from DefaultProtocols.
func applyProtocolDefaults(cfg *Config, wroteUpstreams func(protocol string) bool) error {
	table, err := DefaultProtocols()
	if err != nil {
		return err
	}
	if cfg.Protocols == nil {
		cfg.Protocols = make(map[string]ProtocolConfig, len(table))
	}
	if wroteUpstreams == nil {
		wroteUpstreams = func(string) bool { return false }
	}

	for name, def := range table {
		// git under HA: node-local bare mirrors are not shared across replicas.
		if name == gitIsHAUnsafe && cfg.Server.HA {
			continue
		}
		pc, ok := cfg.Protocols[name]
		if !ok {
			cfg.Protocols[name] = def
			continue
		}
		if len(pc.Upstreams) == 0 && !wroteUpstreams(name) {
			pc.Upstreams = def.Upstreams
			cfg.Protocols[name] = pc
		}
	}

	for name, pc := range cfg.Protocols {
		if pc.Enabled != nil && !*pc.Enabled {
			delete(cfg.Protocols, name)
		}
	}
	return nil
}

// applyOfficialEgressProxy stamps Egress.OfficialProxy onto every upstream with
// Official=true whose Proxy is still empty. Runs after applyProtocolDefaults so
// built-in chains receive the stamp without operators rewriting upstreams.
func applyOfficialEgressProxy(cfg *Config) {
	if cfg == nil {
		return
	}
	proxy := strings.TrimSpace(cfg.Egress.OfficialProxy)
	if proxy == "" {
		return
	}
	for name, pc := range cfg.Protocols {
		changed := false
		for i := range pc.Upstreams {
			if !pc.Upstreams[i].Official {
				continue
			}
			if strings.TrimSpace(pc.Upstreams[i].Proxy) != "" {
				continue // operator already pinned a per-upstream proxy
			}
			pc.Upstreams[i].Proxy = proxy
			changed = true
		}
		if changed {
			cfg.Protocols[name] = pc
		}
	}
}
