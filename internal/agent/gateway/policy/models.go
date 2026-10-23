package policy

import (
	"net/http"
	"net/url"
	"strings"
)

// modelMethods are the methods the model hosts take, exactly as sent (Go keeps a method's case):
// any other (TRACE and TRACK, which echo the request back, in any case) is api-write.
var modelMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// pathRule matches a canonical path: exactly, as one more segment under a prefix (segment), or
// as anything under a prefix (subtree).
type pathRule struct {
	path string
	kind uint8
}

const (
	exact uint8 = iota
	segment
	subtree
)

func (p pathRule) match(s string) bool {
	switch p.kind {
	case exact:
		return s == p.path
	case segment:
		rest, ok := strings.CutPrefix(s, p.path)
		return ok && rest != "" && !strings.Contains(rest, "/")
	default:
		rest, ok := strings.CutPrefix(s, p.path)
		return ok && rest != ""
	}
}

// modelPaths are the paths a request that gets the owner's login may take on each model host
// (gateway.md, the host table): the calls Phase 0 recorded claude-code and codex making
// (P1-phase0-report.md, Q4 and Q5), and the inference routes they belong to. A host without a
// row takes none: claude.ai, console.anthropic.com and platform.claude.com.
var modelPaths = map[string][]pathRule{
	"api.anthropic.com": {
		{"/v1/messages", exact},
		{"/v1/messages/count_tokens", exact},
		{"/v1/models", exact},
		{"/v1/models/", segment},
		{"/api/claude_code/settings", exact},
		{"/api/claude_code/policy_limits", exact},
		{"/api/claude_cli/bootstrap", exact},
		{"/api/eval/sdk-", segment},
	},
	ChatGPTHost: {
		{"/backend-api/codex/", subtree},
		{"/backend-api/ps/plugins/list", exact},
		{"/backend-api/ps/mcp", exact},
		{"/backend-api/wham/accounts/check", exact},
		{"/backend-api/wham/settings/user", exact},
	},
}

// DecideModel applies a model host's method and path rule to a request inside its tunnel,
// before the login is read: any method outside the model set is api-write. A request that gets
// the owner's login (every chatgpt.com request, whose account id is set while a login exists;
// an Anthropic request that carried a carrier, Authorization or X-Api-Key, whatever its value)
// must then have a canonical path (else bad-path) on its host's list, compared case-sensitively
// with any query (else model-path). An Anthropic request without a carrier goes on without a
// credential, its path unchecked. host is the CONNECT's normalised host.
func DecideModel(host, method string, u *url.URL, carrier bool) Code {
	if !modelMethods[method] {
		return CodeAPIWrite
	}
	if ClassOf(host) != ChatGPT && !carrier {
		return ""
	}
	if !Canonical(u) {
		return CodeBadPath
	}
	for _, p := range modelPaths[NormalizeHost(host)] {
		if p.match(u.Path) {
			return ""
		}
	}
	return CodeModelPath
}
