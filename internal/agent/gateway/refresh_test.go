package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
)

// TestChatGPTRetryOn401: a chatgpt.com 401 to an injected token is resent once with the login
// ChatGPTUnauthorized returns, the same body included; when it can't be, the client gets the
// original 401 and its body.
func TestChatGPTRetryOn401(t *testing.T) {
	const newTok = "access-sk-live-refreshed-token"
	big := strings.Repeat("x", maxRetryBody+1)
	long := strings.Repeat("e", max401Body+1)
	refreshed := func(context.Context, string) (ChatGPTLogin, error) {
		return ChatGPTLogin{AccessToken: newTok, AccountID: accountID}, nil
	}
	tests := []struct {
		name         string
		unauthorized func(context.Context, string) (ChatGPTLogin, error)
		auth         bool
		body         string
		status       int
		upstream     int // requests the upstream got
		asked        int // ChatGPTUnauthorized calls
		resp         string
		reason       string // the upstream's 401 body; default "expired"
	}{
		{"refreshed", refreshed, true, `{"input":"hi"}`, 200, 2, 1, "ok", ""},
		{"refreshed, no body", refreshed, true, "", 200, 2, 1, "ok", ""},
		{"the refresh fails", func(context.Context, string) (ChatGPTLogin, error) {
			return ChatGPTLogin{AccessToken: accessTok}, errors.New("chatgpt: refused")
		}, true, `{"input":"hi"}`, 401, 1, 1, "expired\n", ""},
		{"the same token back", func(context.Context, string) (ChatGPTLogin, error) {
			return ChatGPTLogin{AccessToken: accessTok}, nil
		}, true, "", 401, 1, 1, "expired\n", ""},
		{"not configured", nil, true, "", 401, 1, 0, "expired\n", ""},
		{"not injected", refreshed, false, "", 401, 1, 0, "expired\n", ""},
		{"a body over 1 MiB", refreshed, true, big, 401, 1, 0, "expired\n", ""},
		{"a 401 body over 64 KiB", refreshed, true, "", 401, 1, 0, long + "\n", long},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			upstream := func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(b))
				mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer "+newTok {
					reason := tt.reason
					if reason == "" {
						reason = "expired"
					}
					http.Error(w, reason, http.StatusUnauthorized)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}
			var asked []string
			var unauthorized func(context.Context, string) (ChatGPTLogin, error)
			if tt.unauthorized != nil {
				unauthorized = func(ctx context.Context, used string) (ChatGPTLogin, error) {
					mu.Lock()
					asked = append(asked, used)
					mu.Unlock()
					return tt.unauthorized(ctx, used)
				}
			}
			e, got := startIntercept(t, upstream, func(c *Config) {
				c.ChatGPT = chatgptFrom(ChatGPTLogin{AccessToken: accessTok, AccountID: accountID}, nil)
				c.ChatGPTUnauthorized = unauthorized
			})
			raw := "POST /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\nContent-Length: " + strconv.Itoa(len(tt.body)) + "\r\n"
			if tt.auth {
				raw += phBearer
			}
			raw += "\r\n" + tt.body
			status, _, body := e.inTunnel(t, policy.ChatGPTHost, raw)
			if status != tt.status || body != tt.resp {
				t.Errorf("got %d %q, want %d %q", status, body, tt.status, tt.resp)
			}
			mu.Lock()
			defer mu.Unlock()
			if n := len(got.all()); n != tt.upstream || len(asked) != tt.asked {
				t.Fatalf("upstream got %d requests, want %d; asked %d times, want %d", n, tt.upstream, len(asked), tt.asked)
			}
			for _, u := range asked {
				if u != accessTok {
					t.Errorf("asked with used %q, want the injected token", u)
				}
			}
			for i, b := range bodies {
				if b != tt.body {
					t.Errorf("upstream request %d got a %d-byte body, want %d", i, len(b), len(tt.body))
				}
			}
			if tt.upstream == 2 {
				if a := got.all()[1].Header.Get("Authorization"); a != "Bearer "+newTok {
					t.Errorf("the resend had Authorization %q", a)
				}
			}
		})
	}
}
