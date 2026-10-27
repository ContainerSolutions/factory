package tokens

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/provider"
)

// maxRepoPages bounds InstallationRepos (100 repos per page).
const maxRepoPages = 10

// InstallationRepos returns the full names ("owner/name") of the repos the identity's App is
// installed on in the account owner (an organisation or a user), sorted. The adapter expands
// a RuleSet scope's repo globs with it.
//
// Listing an installation's repos needs a token that isn't scoped to one repo, so this is the
// second token body without "repositories" (OrgToken is the first). It carries only
// metadata: read, which every installation has, it lives and is cached inside the Minter,
// and it is never returned: the caller gets repo names only. The installation is found with
// GET /orgs/{owner}/installation, then GET /users/{owner}/installation when owner isn't an
// organisation.
func (m *Minter) InstallationRepos(ctx context.Context, identityName, owner string) ([]string, error) {
	id, ok := m.idents[identityName]
	switch {
	case !ok:
		return nil, fmt.Errorf("list installation repos: unknown identity %q (have %v): %w", identityName, m.Identities(), provider.ErrInvalid)
	case owner == "" || strings.ContainsAny(owner, "/ "):
		return nil, fmt.Errorf("list installation repos: owner %q: %w", owner, provider.ErrInvalid)
	}
	perms := map[string]string{"metadata": "read"}
	key := "\x01repos\x00" + identityName + "\x00" + strings.ToLower(owner)
	var tok cachedToken
	for {
		var (
			retry bool
			err   error
		)
		if cached, ok := m.cached(key); ok {
			tok = cached
			break
		}
		tok, retry, err = m.flight.do(ctx, &m.mu, key, func() (cachedToken, error) {
			if tok, ok := m.cached(key); ok {
				return tok, nil
			}
			find := func() (cachedInstall, error) { return m.findAccountInstallation(ctx, id, owner) }
			tok, err := m.mintAccount(ctx, id, owner, perms, find)
			if err != nil {
				return cachedToken{}, err
			}
			m.mu.Lock()
			m.pruneLocked()
			m.tokens[key] = tok
			m.mu.Unlock()
			return tok, nil
		})
		if retry {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list installation repos of %s on %s: %w", identityName, owner, err)
		}
		break
	}
	var out []string
	q := url.Values{"per_page": {"100"}}
	for range maxRepoPages {
		var page struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		h, err := m.send(ctx, m.plain, tok.value, http.MethodGet, "installation/repositories", q, nil, http.StatusOK, &page, "list installation repos")
		if err != nil {
			return nil, fmt.Errorf("list installation repos of %s on %s: %w", identityName, owner, err)
		}
		for _, r := range page.Repositories {
			if o, _, ok := strings.Cut(r.FullName, "/"); ok && strings.EqualFold(o, owner) {
				out = append(out, r.FullName)
			}
		}
		next, ok := NextPage(h)
		if !ok {
			break
		}
		q = next
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// findAccountInstallation looks up the App's installation on an account: the organisation's,
// else (404) the user's (GET /users/{owner}/installation).
func (m *Minter) findAccountInstallation(ctx context.Context, id *identity, owner string) (cachedInstall, error) {
	inst, err := m.findOrgInstallation(ctx, id, owner)
	if !errors.Is(err, provider.ErrNotFound) {
		return inst, err
	}
	var resp struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
	}
	path := "users/" + url.PathEscape(owner) + "/installation"
	if err := m.call(ctx, id, http.MethodGet, path, nil, http.StatusOK, &resp, "find installation for user "+owner); err != nil {
		return cachedInstall{}, err
	}
	if resp.ID <= 0 {
		return cachedInstall{}, fmt.Errorf("github: find installation for user %s: no installation id in the response", owner)
	}
	return cachedInstall{id: resp.ID, permissions: resp.Permissions, fetchedAt: m.clock.Now()}, nil
}

// NextPage returns the query of the Link header's rel="next" URL. Only the query is used: the
// next call goes to the same path on the configured API, wherever the link points.
func NextPage(h http.Header) (url.Values, bool) {
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
