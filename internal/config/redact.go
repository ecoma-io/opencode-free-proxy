package config

import (
	"net/url"
	"strings"
)

// RedactProxyURL strips credential userinfo from a proxy URL so it is safe
// for logs, errors, and debug output. Credentials must never appear in any
// surfaced string — the full URL with a password is what transport-layer
// errors tend to carry, so every error/log path that can hold a proxy URL
// routes through here.
func RedactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Unparseable → return a shape that carries no secret content.
		return "<invalid proxy url>"
	}
	// Drop userinfo entirely (username alone is identifying).
	u.User = nil
	return u.String()
}

// HasSecret reports whether s embeds url userinfo ("user:pass@host"). It is
// the safety net tests assert with: no log line, error, or debug string may
// carry proxy credentials, and this is how that invariant is checked.
func HasSecret(s string) bool {
	lower := strings.ToLower(s)
	if i := strings.Index(lower, "://"); i >= 0 {
		rest := lower[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			userinfo := rest[:at]
			if strings.Contains(userinfo, ":") {
				return true
			}
		}
	}
	return false
}
