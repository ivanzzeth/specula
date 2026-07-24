package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/pkg/verify"
)

// buildMaturityVerifier constructs the cool-down gate from per-protocol
// verification.maturity blocks. Returns nil when no protocol enables it.
func buildMaturityVerifier(cfg *config.Config, log *slog.Logger) *verify.MaturityVerifier {
	if cfg == nil {
		return nil
	}
	specs := map[string]verify.MaturitySpec{}
	for name, pc := range cfg.Protocols {
		m := pc.Verification.Maturity
		if m == nil || strings.TrimSpace(m.MinAge) == "" {
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(m.MinAge))
		if err != nil || d <= 0 {
			log.Warn("specula: maturity min_age ignored", "protocol", name, "min_age", m.MinAge, "err", err)
			continue
		}
		pol := verify.MaturityWarn
		if strings.EqualFold(strings.TrimSpace(m.Policy), "enforce") {
			pol = verify.MaturityEnforce
		}
		proto := strings.ToLower(strings.TrimSpace(name))
		specs[proto] = verify.MaturitySpec{MinAge: d, Policy: pol}
		log.Info("specula: maturity cool-down enabled",
			"protocol", proto, "min_age", d.String(), "policy", string(pol))
	}
	if len(specs) == 0 {
		return nil
	}
	return verify.NewMaturityVerifier(specs)
}
