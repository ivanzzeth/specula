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
//	/v2/<name>/referrers/<digest>
//
// The action is derived from the method: GET/HEAD → pull, DELETE → delete,
// everything else (PUT/PATCH/POST) → push. The one exception is the blob-upload
// session namespace, which is always push (see below). isRepoReq is false for
// the bare /v2/ version probe (no scope required) or an unrecognised path.
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
	case strings.Contains(rest, "/blobs/uploads"):
		// An upload session is part of the push flow whatever the method: even
		// GET /v2/<name>/blobs/uploads/<uuid> (resume — "how much have you got?")
		// is meaningful only to a pusher and must be challenged with push scope.
		// Deriving pull from the GET would challenge the client for a pull token,
		// which the data-plane's push chokepoint then rejects — a 403 on a
		// perfectly legitimate resumable upload.
		name = rest[:strings.Index(rest, "/blobs/uploads")]
		if name == "" {
			return "", "", false
		}
		return name, ActionPush, true
	case strings.Contains(rest, "/manifests/"):
		name = rest[:strings.LastIndex(rest, "/manifests/")]
	case strings.Contains(rest, "/blobs/"):
		name = rest[:strings.LastIndex(rest, "/blobs/")]
	case strings.HasSuffix(rest, "/tags/list"):
		name = strings.TrimSuffix(rest, "/tags/list")
	case strings.Contains(rest, "/referrers/"):
		name = rest[:strings.LastIndex(rest, "/referrers/")]
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
