package policy

import (
	"slices"
	"strings"
)

// infraCookies are codex's allow list of chatgpt.com cookies
// (codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs at rust-v0.163.0): Cloudflare's
// infrastructure cookies and the __oailb load-balancer cookie. Any cf_chl_* name passes too.
var infraCookies = []string{
	"__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb", "_cfuvid",
	"cf_clearance", "cf_ob_info", "cf_use_ob",
}

// InfraCookiePrefix starts the names of Cloudflare's challenge cookies, which pass too.
const InfraCookiePrefix = "cf_chl_"

// InfraCookies returns the cookie names the model hosts keep (CookiesInfraOnly), besides any
// name that starts with InfraCookiePrefix.
func InfraCookies() []string { return slices.Clone(infraCookies) }

// IsInfraCookie reports whether name, compared exactly (cookie names are case-sensitive), is an
// infrastructure cookie.
func IsInfraCookie(name string) bool {
	return slices.Contains(infraCookies, name) ||
		strings.HasPrefix(name, InfraCookiePrefix) && isToken(name)
}

// FilterCookies applies CookiesInfraOnly to a request's Cookie header values: it keeps the
// name=value pairs whose name IsInfraCookie, in order and as they were sent, joined into one
// value with "; ". It drops every other cookie, and any pair that isn't name=value or whose value
// isn't an RFC 6265 cookie-value (no space, comma, backslash or control character, quotes only
// around it), so a lenient upstream parser can't read another cookie out of a kept one. It
// returns nil when nothing is left, so the caller removes the header.
func FilterCookies(values []string) []string {
	var kept []string
	for _, v := range values {
		for part := range strings.SplitSeq(v, ";") {
			part = strings.Trim(part, " \t")
			name, value, ok := strings.Cut(part, "=")
			if ok && IsInfraCookie(name) && validCookieValue(value) {
				kept = append(kept, part)
			}
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return []string{strings.Join(kept, "; ")}
}

// isToken reports whether s is a non-empty RFC 9110 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// validCookieValue reports whether v is an RFC 6265 cookie-value: cookie-octets, optionally
// inside one pair of double quotes.
func validCookieValue(v string) bool {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x21 || c > 0x7e || c == '"' || c == ',' || c == ';' || c == '\\' {
			return false
		}
	}
	return true
}
