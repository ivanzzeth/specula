package hf

import (
	"encoding/json"
	"net/url"
	"strings"
)

// rewriteJSONURLs rewrites absolute http(s) URLs in JSON whose host is a known
// Hugging Face domain to point at the Specula proxy (base + prefix + path+query).
func rewriteJSONURLs(data []byte, base, prefix string) []byte {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	rewriteValue(doc, base, prefix)
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

func rewriteValue(v any, base, prefix string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok {
				if rewritten := rewriteHubURL(s, base, prefix); rewritten != s {
					x[k] = rewritten
					continue
				}
			}
			rewriteValue(val, base, prefix)
		}
	case []any:
		for i, val := range x {
			if s, ok := val.(string); ok {
				if rewritten := rewriteHubURL(s, base, prefix); rewritten != s {
					x[i] = rewritten
					continue
				}
			}
			rewriteValue(val, base, prefix)
		}
	}
}

func rewriteHubURL(s, base, prefix string) string {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	host := strings.ToLower(u.Hostname())
	if !isHubHost(host) {
		return s
	}
	p := strings.TrimRight(prefix, "/")
	out := base + p + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func isHubHost(host string) bool {
	return strings.Contains(host, "huggingface.co") ||
		strings.Contains(host, "hf.co") ||
		strings.Contains(host, "hf-mirror.com")
}
