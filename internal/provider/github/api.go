package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/tokens"
)

// perPage is the page size of every list call (GitHub's maximum).
const perPage = 100

// repoToken mints (or reuses) the read identity's token on repo with perms.
func (c *Conn) repoToken(ctx context.Context, repo string, perms map[string]string) (string, error) {
	cred, err := c.minter.Token(ctx, provider.GitCredentialRequest{
		Identity: c.readIdentity, Repo: repo, Permissions: perms, TTL: tokens.MaxTTL,
	})
	c.record(c.readIdentity, err)
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}

// orgToken mints (or reuses) the read identity's members: read token on org.
func (c *Conn) orgToken(ctx context.Context, org string) (string, error) {
	tok, _, err := c.minter.OrgToken(ctx, c.readIdentity, org, map[string]string{"members": "read"})
	c.record(c.readIdentity, err)
	return tok, err
}

// list GETs path page by page with token (at most maxPages pages of perPage), calling each with
// every item, and stops early when each returns false.
func (c *Conn) list(ctx context.Context, token, path string, query url.Values, maxPages int, each func(json.RawMessage) (bool, error)) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", fmt.Sprint(perPage))
	for range maxPages {
		var items []json.RawMessage
		h, err := c.minter.Get(ctx, token, path, q, &items)
		if err != nil {
			return err
		}
		for _, it := range items {
			more, err := each(it)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
		next, ok := nextQuery(h)
		if !ok {
			return nil
		}
		q = next
	}
	return nil
}

// nextQuery returns the query of the Link header's rel="next" URL. Only the query is used:
// the next call goes to the same path on the configured API, wherever the link points.
func nextQuery(h http.Header) (url.Values, bool) {
	for _, link := range h.Values("Link") {
		for part := range strings.SplitSeq(link, ",") {
			target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
			if !ok || !strings.Contains(params, `rel="next"`) {
				continue
			}
			u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
			if err != nil {
				return nil, false
			}
			return u.Query(), true
		}
	}
	return nil, false
}
