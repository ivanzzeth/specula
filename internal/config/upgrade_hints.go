package config

import (
	"fmt"
	"strings"
)

// UpgradeHint is a non-fatal startup warning: the running config is valid but
// missing a newer allowlist that operators usually want after upgrading.
// Specula never auto-rewrites the file; the Message tells how to opt in.
type UpgradeHint struct {
	Section string // apply-example --section value (apt, helm, …)
	Message string
}

// UpgradeHints inspects cfg for protocols that are enabled but still on the
// legacy empty-allowlist shape. Empty allowlists remain supported; these are
// soft nudges toward the multi-source defaults in the embedded example.
func UpgradeHints(cfg *Config) []UpgradeHint {
	if cfg == nil {
		return nil
	}
	var out []UpgradeHint
	for name, pc := range cfg.Protocols {
		proto := strings.ToLower(strings.TrimSpace(name))
		switch proto {
		case "apt":
			if pc.Apt == nil || len(pc.Apt.Repositories) == 0 {
				out = append(out, hint("apt",
					"protocols.apt has no repositories allowlist (legacy path-prefix mode); "+
						"consider: specula config apply-example --section apt"))
			}
		case "helm":
			if pc.Helm == nil || len(pc.Helm.Repositories) == 0 {
				out = append(out, hint("helm",
					"protocols.helm has no repositories allowlist (legacy subpath mode); "+
						"consider: specula config apply-example --section helm"))
			}
		case "conda":
			if pc.Conda == nil || len(pc.Conda.Channels) == 0 {
				out = append(out, hint("conda",
					"protocols.conda has no channels allowlist (legacy cloud-root mode); "+
						"consider: specula config apply-example --section conda"))
			}
		case "cargo":
			if pc.Cargo == nil || len(pc.Cargo.Registries) == 0 {
				out = append(out, hint("cargo",
					"protocols.cargo has no registries allowlist (legacy index path mode); "+
						"consider: specula config apply-example --section cargo"))
			}
		case "oci":
			if pc.OCI == nil || len(pc.OCI.RemoteRegistries) == 0 {
				out = append(out, hint("oci",
					"protocols.oci has no remote_registries allowlist (path-style multi-registry pulls disabled); "+
						"consider: specula config apply-example --section oci"))
			}
		}
	}
	return out
}

func hint(section, msg string) UpgradeHint {
	return UpgradeHint{Section: section, Message: fmt.Sprintf("config upgrade hint [%s]: %s", section, msg)}
}
