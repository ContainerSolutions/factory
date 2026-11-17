package policy

import "testing"

// TestDecideModel has a row per model host path list in gateway.md's host table: the allowed
// paths, the owner's data and the token minting refused, the non-canonical forms, and the rule
// only gating a request that gets the login.
func TestDecideModel(t *testing.T) {
	const api, chatgpt = "api.anthropic.com", ChatGPTHost
	tests := []struct {
		name, host, method, uri string
		carrier                 bool
		want                    Code
	}{
		{"messages", api, "POST", "/v1/messages?beta=true", true, ""},
		{"count tokens", api, "POST", "/v1/messages/count_tokens", true, ""},
		{"models", api, "GET", "/v1/models", true, ""},
		{"a model", api, "GET", "/v1/models/claude-x", true, ""},
		{"settings", api, "GET", "/api/claude_code/settings", true, ""},
		{"policy limits", api, "GET", "/api/claude_code/policy_limits", true, ""},
		{"bootstrap", api, "GET", "/api/claude_cli/bootstrap", true, ""},
		{"eval", api, "POST", "/api/eval/sdk-abc123", true, ""},
		{"message batches", api, "GET", "/v1/messages/batches", true, CodeModelPath},
		{"a batch's results", api, "GET", "/v1/messages/batches/b1/results", true, CodeModelPath},
		{"a file's content", api, "GET", "/v1/files/x/content", true, CodeModelPath},
		{"files", api, "GET", "/v1/files", true, CodeModelPath},
		{"api key minting", api, "POST", "/api/oauth/claude_cli/create_api_key", true, CodeModelPath},
		{"oauth profile", api, "GET", "/api/oauth/profile", true, CodeModelPath},
		{"another claude_code path", api, "GET", "/api/claude_code/sessions", true, CodeModelPath},
		{"under a model", api, "GET", "/v1/models/x/y", true, CodeModelPath},
		{"models prefix only", api, "GET", "/v1/models/", true, CodeBadPath},
		{"eval prefix only", api, "GET", "/api/eval/sdk-", true, CodeModelPath},
		{"another eval path", api, "GET", "/api/eval/other", true, CodeModelPath},
		{"case differs", api, "POST", "/V1/messages", true, CodeModelPath},
		{"a trailing slash", api, "POST", "/v1/messages/", true, CodeBadPath},
		{"dot dot", api, "GET", "/v1/messages/../files/x/content", true, CodeBadPath},
		{"double slash", api, "POST", "//v1/messages", true, CodeBadPath},
		{"encoded slash", api, "POST", "/v1%2Fmessages", true, CodeBadPath},
		{"encoded letter", api, "POST", "/v1/m%65ssages", true, CodeBadPath},
		{"no carrier, any path", api, "POST", "/api/event_logging/v2/batch", false, ""},
		{"no carrier, not canonical", api, "GET", "/v1/files/../x", false, ""},
		{"TRACE without a carrier", api, "TRACE", "/v1/messages", false, CodeAPIWrite},
		{"TRACE on a listed path", api, "TRACE", "/v1/messages", true, CodeAPIWrite},
		{"lower-case get", api, "get", "/v1/models", true, CodeAPIWrite},

		{"claude.ai", "claude.ai", "POST", "/v1/messages", true, CodeModelPath},
		{"claude.ai conversations", "claude.ai", "GET", "/api/organizations/o/chat_conversations", true, CodeModelPath},
		{"console", "console.anthropic.com", "GET", "/v1/messages", true, CodeModelPath},
		{"platform", "platform.claude.com", "GET", "/v1/oauth/token", true, CodeModelPath},
		{"claude.ai without a carrier", "claude.ai", "GET", "/x", false, ""},

		{"codex responses", chatgpt, "GET", "/backend-api/codex/responses", false, ""},
		{"codex compact", chatgpt, "POST", "/backend-api/codex/responses/compact", true, ""},
		{"codex models", chatgpt, "GET", "/backend-api/codex/models?client_version=0.163.0", true, ""},
		{"plugins", chatgpt, "GET", "/backend-api/ps/plugins/list", true, ""},
		{"mcp", chatgpt, "POST", "/backend-api/ps/mcp", true, ""},
		{"accounts check", chatgpt, "GET", "/backend-api/wham/accounts/check", true, ""},
		{"user settings", chatgpt, "GET", "/backend-api/wham/settings/user", true, ""},
		{"codex prefix only", chatgpt, "GET", "/backend-api/codex", true, CodeModelPath},
		{"conversations", chatgpt, "GET", "/backend-api/conversations", true, CodeModelPath},
		{"a conversation", chatgpt, "GET", "/backend-api/conversation/c1", true, CodeModelPath},
		{"memories", chatgpt, "GET", "/backend-api/memories", true, CodeModelPath},
		{"files", chatgpt, "GET", "/backend-api/files/f1/download", true, CodeModelPath},
		{"cloud tasks", chatgpt, "GET", "/backend-api/wham/tasks/list", true, CodeModelPath},
		{"another ps path", chatgpt, "GET", "/backend-api/ps/other", true, CodeModelPath},
		{"no Authorization still gated", chatgpt, "GET", "/backend-api/conversations", false, CodeModelPath},
		{"out of codex", chatgpt, "GET", "/backend-api/codex/../conversations", true, CodeBadPath},
		{"encoded out of codex", chatgpt, "GET", "/backend-api/codex/..%2Fconversations", true, CodeBadPath},
		{"case differs", chatgpt, "GET", "/Backend-api/codex/responses", true, CodeModelPath},
		{"TRACE", chatgpt, "TRACE", "/backend-api/codex/responses", true, CodeAPIWrite},
	}
	for _, tt := range tests {
		t.Run(tt.host+" "+tt.name, func(t *testing.T) {
			if got := DecideModel(tt.host, tt.method, mustURL(t, tt.uri), tt.carrier); got != tt.want {
				t.Errorf("DecideModel(%s %s) = %q, want %q", tt.method, tt.uri, got, tt.want)
			}
		})
	}
}

// TestDecideModelEndpoint has a row per row of the model endpoint's route (gateway.md): each
// method and path, another method, another path, and the non-canonical forms.
func TestDecideModelEndpoint(t *testing.T) {
	tests := []struct {
		name, method, uri string
		want              Code
	}{
		{"chat completions", "POST", "/v1/chat/completions", ""},
		{"chat completions with a query", "POST", "/v1/chat/completions?stream=true", ""},
		{"models", "GET", "/v1/models", ""},
		{"a model", "GET", "/v1/models/qwen3-coder-next", ""},
		{"GET chat completions", "GET", "/v1/chat/completions", CodeModelPath},
		{"POST models", "POST", "/v1/models", CodeModelPath},
		{"HEAD models", "HEAD", "/v1/models", CodeModelPath},
		{"TRACE", "TRACE", "/v1/chat/completions", CodeModelPath},
		{"lower-case post", "post", "/v1/chat/completions", CodeModelPath},
		{"completions", "POST", "/v1/completions", CodeModelPath},
		{"responses", "POST", "/v1/responses", CodeModelPath},
		{"embeddings", "POST", "/v1/embeddings", CodeModelPath},
		{"vLLM's metrics", "GET", "/metrics", CodeModelPath},
		{"vLLM's tokenize", "POST", "/tokenize", CodeModelPath},
		{"under a model", "GET", "/v1/models/org/model", CodeModelPath},
		{"case differs", "POST", "/V1/chat/completions", CodeModelPath},
		{"the root", "GET", "/", CodeModelPath},
		{"models prefix only", "GET", "/v1/models/", CodeBadPath},
		{"a trailing slash", "POST", "/v1/chat/completions/", CodeBadPath},
		{"dot dot", "GET", "/v1/models/../../metrics", CodeBadPath},
		{"double slash", "POST", "//v1/chat/completions", CodeBadPath},
		{"encoded slash", "GET", "/v1/models/org%2Fmodel", CodeBadPath},
		{"not canonical, another method", "DELETE", "/v1/models/../x", CodeBadPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideModelEndpoint(tt.method, mustURL(t, tt.uri)); got != tt.want {
				t.Errorf("DecideModelEndpoint(%s %s) = %q, want %q", tt.method, tt.uri, got, tt.want)
			}
		})
	}
}
