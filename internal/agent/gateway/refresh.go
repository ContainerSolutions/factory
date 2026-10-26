package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The bounds of a chatgpt.com 401's retry (gateway.md, "ChatGPT refresh").
const (
	maxRetryBody = 1 << 20  // a request body buffered for the resend
	max401Body   = 64 << 10 // a 401's body read before the refresh
)

// chatgptRetry sends a chatgpt.com request whose Authorization the gateway injected, and on a
// 401 asks unauthorized for the login once and resends the request with it. When it can't (a
// request body over maxRetryBody, a 401 body over max401Body, a failed refresh, or the same
// token back), the client gets the original 401 unchanged, its body as the upstream sent it.
type chatgptRetry struct {
	next         http.RoundTripper
	unauthorized func(ctx context.Context, used string) (ChatGPTLogin, error)
	log          *slog.Logger
}

func (t *chatgptRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	first := req.Clone(req.Context())
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxRetryBody+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxRetryBody {
			first.Body = readCloser{io.MultiReader(bytes.NewReader(b), req.Body), req.Body}
			return t.next.RoundTrip(first)
		}
		body = b
		first.Body = io.NopCloser(bytes.NewReader(b))
	}
	res, err := t.next.RoundTrip(first)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	head, rerr := io.ReadAll(io.LimitReader(res.Body, max401Body+1))
	original := func() (*http.Response, error) {
		res.Body = readCloser{io.MultiReader(bytes.NewReader(head), res.Body), res.Body}
		return res, nil
	}
	if rerr != nil || len(head) > max401Body {
		return original()
	}
	used, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	l, err := t.unauthorized(req.Context(), used)
	if err != nil {
		t.log.Warn("gateway: the ChatGPT refresh after a 401", "err", err)
	}
	if err != nil || l.AccessToken == "" || l.AccessToken == used {
		return original()
	}
	_ = res.Body.Close()
	again := req.Clone(req.Context())
	again.Header.Set("Authorization", "Bearer "+l.AccessToken)
	if l.AccountID != "" {
		again.Header.Set(ChatGPTAccountHeader, l.AccountID)
	}
	if body != nil {
		again.Body = io.NopCloser(bytes.NewReader(body))
	}
	return t.next.RoundTrip(again)
}

// readCloser reads from one reader and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}
