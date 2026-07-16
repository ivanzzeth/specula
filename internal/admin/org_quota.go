package admin

// Self-service org creation quota (settings.KeyOrgMaxPerUser).
//
// The R1 org port deliberately left this out for want of a runtime-settings
// layer: a hard-coded constant would have been unchangeable without a redeploy,
// and an env var would have needed a restart per adjustment. With the settings
// layer ported it becomes what it always should have been — a hot-reloadable
// integer an admin edits in the UI, effective on the next request.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ivanzzeth/specula/internal/settings"
)

// systemRoleAdmin is auth.User.SystemRole's admin value. Spelled as a literal
// here because internal/auth spells it as a literal too (middleware.go
// AdminRequired); introducing a constant in only one of the two would be worse
// than matching the existing convention.
const systemRoleAdmin = "admin"

// errOrgQuotaExceeded marks the "limit reached" outcome so the caller answers
// 409 rather than 500. Any other error from checkOrgQuota is a real failure.
var errOrgQuotaExceeded = errors.New("org create quota exceeded")

// orgMaxPerUser returns the effective org.max_per_user limit.
//
// It FAILS OPEN to the default (rather than to "unlimited") whenever the value
// cannot be read or parsed: a settings store hiccup must not silently remove a
// quota, and it must not brick org creation either. DefaultOrgMaxPerUser is the
// same value the setting's own bootstrap default documents, so an unreadable
// store behaves exactly like an unconfigured one.
func (s *Server) orgMaxPerUser(ctx context.Context) int {
	if s.settings == nil {
		return settings.DefaultOrgMaxPerUser
	}
	v, err := s.settings.Effective(ctx, settings.KeyOrgMaxPerUser)
	if err != nil || v == "" {
		return settings.DefaultOrgMaxPerUser
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		// Validation should make this unreachable (Kind=int is checked on write),
		// but a value predating the validation, or hand-inserted, must not panic
		// or silently disable the quota.
		s.log.Warn("admin: org.max_per_user is not an integer; using the default",
			"value", v, "default", settings.DefaultOrgMaxPerUser)
		return settings.DefaultOrgMaxPerUser
	}
	return n
}

// checkOrgQuota reports whether userID may self-create another org.
//
// Returns errOrgQuotaExceeded when the limit is reached, a wrapped store error
// when the count cannot be taken, or nil when creation may proceed.
//
// Two deliberate exemptions:
//   - limit <= 0 means unlimited (0 is the documented "off" value; a negative
//     value can only arrive from a hand-edited store and is treated the same way
//     rather than locking everyone out).
//   - system admins are never limited. They administer the limit, and the
//     create-org endpoint is the escape hatch for a user who belongs to no org —
//     an operator who cannot create one has no way back in.
func (s *Server) checkOrgQuota(ctx context.Context, userID, systemRole string) error {
	if systemRole == systemRoleAdmin {
		return nil
	}
	limit := s.orgMaxPerUser(ctx)
	if limit <= 0 {
		return nil // unlimited
	}
	count, err := s.orgs.CountOrgsByCreator(ctx, userID)
	if err != nil {
		return fmt.Errorf("count orgs by creator: %w", err)
	}
	if count >= limit {
		return fmt.Errorf("%w: you have already created %d of a maximum %d organisation(s); "+
			"ask an administrator to raise org.max_per_user", errOrgQuotaExceeded, count, limit)
	}
	return nil
}
