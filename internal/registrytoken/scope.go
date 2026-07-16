package registrytoken

import "strings"

// Scope is a parsed requested scope from a /token ?scope= parameter.
type Scope struct {
	Type    string   // "repository"
	Name    string   // "<org>/<repo>"
	Actions []string // pull | push | delete
}

// ParseScopes parses zero or more raw scope strings of the form
// "repository:<name>:<action>[,<action>…]" into Scopes. Malformed entries are
// skipped. The resource name may contain '/' (org/repo) but never ':', so the
// type is split off the front and the actions off the end, leaving the name in
// the middle.
func ParseScopes(raw []string) []Scope {
	out := make([]Scope, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		first := strings.IndexByte(s, ':')
		last := strings.LastIndexByte(s, ':')
		if first < 0 || last <= first {
			continue // need at least type:name:actions
		}
		typ := s[:first]
		name := s[first+1 : last]
		actionsRaw := s[last+1:]
		if typ == "" || name == "" || actionsRaw == "" {
			continue
		}
		var actions []string
		for _, a := range strings.Split(actionsRaw, ",") {
			if a = strings.TrimSpace(a); a != "" {
				actions = append(actions, a)
			}
		}
		if len(actions) == 0 {
			continue
		}
		out = append(out, Scope{Type: typ, Name: name, Actions: actions})
	}
	return out
}

// ParseResourceScope maps an HTTP request onto the (repository, action) it
// requires. It recognises the OCI Distribution data-plane paths:
//
//	/v2/<name>/manifests/<ref>
//	/v2/<name>/blobs/<digest>
//	/v2/<name>/blobs/uploads/…
//	/v2/<name>/tags/list
//
// The action is derived from the method: GET/HEAD → pull, DELETE → delete,
// everything else (PUT/PATCH/POST) → push. isRepoReq is false for the bare
// /v2/ version probe (no scope required) or an unrecognised path.
func ParseResourceScope(method, path string) (repoName, action string, isRepoReq bool) {
	if path == "/v2" || path == "/v2/" {
		return "", "", false
	}
	if !strings.HasPrefix(path, "/v2/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/v2/")

	name := ""
	switch {
	case strings.Contains(rest, "/manifests/"):
		name = rest[:strings.LastIndex(rest, "/manifests/")]
	case strings.Contains(rest, "/blobs/"):
		name = rest[:strings.LastIndex(rest, "/blobs/")]
	case strings.HasSuffix(rest, "/tags/list"):
		name = strings.TrimSuffix(rest, "/tags/list")
	default:
		return "", "", false
	}
	if name == "" {
		return "", "", false
	}
	return name, actionForMethod(method), true
}

// actionForMethod maps an HTTP method to a Distribution scope action.
func actionForMethod(method string) string {
	switch method {
	case "GET", "HEAD":
		return ActionPull
	case "DELETE":
		return ActionDelete
	default: // PUT, PATCH, POST → write
		return ActionPush
	}
}
