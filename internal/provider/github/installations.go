package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

var _ provider.InstallationChecker = (*Conn)(nil)

// MissingInstallations implements provider.InstallationChecker: per owner, the repos the
// identity's App installation lists (tokens.Minter.InstallationRepos); an owner the App isn't
// installed on at all misses every one of its repos. Repos compare case-insensitively.
func (c *Conn) MissingInstallations(ctx context.Context, identity string, repos []string) (string, []string, error) {
	byOwner := map[string][]string{}
	var owners []string
	for _, r := range repos {
		owner, _, ok := strings.Cut(r, "/")
		if !ok || owner == "" {
			return "", nil, fmt.Errorf("github %s: check installations: repo %q: %w", c.name, r, provider.ErrInvalid)
		}
		key := strings.ToLower(owner)
		if _, seen := byOwner[key]; !seen {
			owners = append(owners, owner)
		}
		byOwner[key] = append(byOwner[key], r)
	}
	var missing []string
	for _, owner := range owners {
		installed, err := c.minter.InstallationRepos(ctx, identity, owner)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			missing = append(missing, byOwner[strings.ToLower(owner)]...)
			continue
		case err != nil:
			return "", nil, fmt.Errorf("github %s: check installations of %s: %w", c.name, identity, err)
		}
		for _, r := range byOwner[strings.ToLower(owner)] {
			if !slices.ContainsFunc(installed, func(i string) bool { return strings.EqualFold(i, r) }) {
				missing = append(missing, r)
			}
		}
	}
	return c.appSlug(ctx, identity), missing, nil
}

// appSlug is the identity's App's slug, from the factory bots' logins; "" when they can't be
// learnt.
func (c *Conn) appSlug(ctx context.Context, identity string) string {
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return ""
	}
	for login, id := range bots {
		if id == identity {
			return strings.TrimSuffix(login, "[bot]")
		}
	}
	return ""
}

// notInstalled turns a mint error that says the App isn't installed on the repo into a
// *provider.NotInstalledError: the installation lookup's 404 (GET /repos/{repo}/installation),
// or the token request's 422 when the owner's cached installation doesn't cover the repo.
// Any other error is returned as it is.
func (c *Conn) notInstalled(ctx context.Context, identity, repo string, err error) error {
	var apiErr *tokens.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.StatusCode == 404 && strings.HasPrefix(apiErr.Op, "find installation for"),
		apiErr.StatusCode == 422 && apiErr.Op == "create installation token":
		return &provider.NotInstalledError{Identity: identity, App: c.appSlug(ctx, identity), Repo: repo, Err: err}
	}
	return err
}
