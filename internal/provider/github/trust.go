package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
)

const (
	// DefaultTier is the tier of an actor no trust rule matches.
	DefaultTier = "anyone"
	// DefaultGroupTTL is how long a Group member source resolved for trust is reused, when
	// the Group sets no cache.ttl.
	DefaultGroupTTL = 10 * time.Minute
	// maxMemberPages bounds a member listing (100 per page).
	maxMemberPages = 50
)

type membersEntry struct {
	logins []string
	at     time.Time
}

// actor is the event's sender: "<provider>:<login>", isBot for a Bot account, isFactory and
// identity for one of the factory's own Apps (recognised by its bot login, "<slug>[bot]"), and
// the tier from the Provider's trust rules.
func (c *Conn) actor(ctx context.Context, e *envelope) (*v1alpha1.Actor, error) {
	if e.Sender == nil || e.Sender.Login == "" {
		return nil, nil
	}
	login := e.Sender.Login
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return nil, err
	}
	a := &v1alpha1.Actor{ID: c.name + ":" + login, IsBot: e.Sender.Type == "Bot"}
	if id, ok := bots[strings.ToLower(login)]; ok {
		a.IsFactory, a.Identity, a.IsBot = true, id, true
	}
	if a.Tier, err = c.Tier(ctx, login, senderAssociation(e)); err != nil {
		return nil, err
	}
	return a, nil
}

// senderAssociation is GitHub's author_association of the sender: the association of the
// comment, review, PR or issue the sender wrote. When the sender isn't the author (a
// maintainer labelling someone's issue), the payload's association is the author's, so it
// is not used: "".
func senderAssociation(e *envelope) string {
	login := e.Sender.Login
	switch {
	case e.Comment != nil && sameLogin(e.Comment.User.Login, login):
		return e.Comment.AuthorAssociation
	case e.Review != nil && sameLogin(e.Review.User.Login, login):
		return e.Review.AuthorAssociation
	case e.PullRequest != nil && sameLogin(e.PullRequest.User.Login, login):
		return e.PullRequest.AuthorAssociation
	case e.Issue != nil && sameLogin(e.Issue.User.Login, login):
		return e.Issue.AuthorAssociation
	}
	return ""
}

// factoryBots maps the factory Apps' bot logins (lower-case) to their identities, learned
// once from GET /app (each App's slug).
func (c *Conn) factoryBots(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	bots := c.bots
	c.mu.Unlock()
	if bots != nil {
		return bots, nil
	}
	out := map[string]string{}
	for _, id := range c.minter.Identities() {
		app, err := c.minter.App(ctx, id)
		c.record(id, err)
		if err != nil {
			return nil, fmt.Errorf("learn the factory Apps' logins: %w", err)
		}
		if app.Slug != "" {
			out[strings.ToLower(app.BotLogin())] = id
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bots == nil {
		c.bots = out
	}
	return c.bots, nil
}

// Tier resolves a GitHub login's tier through the Provider's trust rules, in order: the
// first rule whose matchers all match wins (a rule with none matches anyone), else
// DefaultTier. Matchers: group (the login is in one of the catalog Group's member sources on
// this Provider, resolved through ResolveGroup and cached for the Group's cache.ttl, default
// DefaultGroupTTL), authorAssociation (association, as GitHub reports it), bots (the login).
// workspaceMember is Slack's and never matches. Logins compare case-insensitively.
func (c *Conn) Tier(ctx context.Context, login, association string) (string, error) {
	for _, r := range c.trust {
		ok, err := c.matchTrust(ctx, r, login, association)
		if err != nil {
			return "", err
		}
		if ok {
			return r.Tier, nil
		}
	}
	return DefaultTier, nil
}

func (c *Conn) matchTrust(ctx context.Context, r v1alpha1.TrustRule, login, association string) (bool, error) {
	if r.WorkspaceMember {
		return false, nil
	}
	if len(r.AuthorAssociation) > 0 && !containsFold(r.AuthorAssociation, association) {
		return false, nil
	}
	if len(r.Bots) > 0 && !containsFold(r.Bots, login) {
		return false, nil
	}
	if r.Group != "" {
		return c.inGroup(ctx, r.Group, login)
	}
	return true, nil
}

// inGroup reports whether login is in the catalog Group named group, through its member
// sources on this Provider. A source that can't be resolved here (an unknown team or org, a
// Slack user group, an invalid source) only means "not a member of that source".
func (c *Conn) inGroup(ctx context.Context, group, login string) (bool, error) {
	if c.groups == nil {
		return false, nil
	}
	g, ok := c.groups(group)
	if !ok {
		c.log.WarnContext(ctx, "trust rule names an unknown group", "group", group)
		return false, nil
	}
	ttl := DefaultGroupTTL
	if g.Cache != nil {
		if d, err := g.Cache.TTL.Parse(); err == nil && d > 0 {
			ttl = d
		}
	}
	for _, src := range g.Members {
		if src.Provider != c.name {
			continue
		}
		members, err := c.cachedMembers(ctx, src, ttl)
		switch {
		case errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrUnsupported), errors.Is(err, provider.ErrInvalid):
			c.log.WarnContext(ctx, "group member source not resolved", "group", group, "err", err)
			continue
		case err != nil:
			return false, fmt.Errorf("resolve group %q: %w", group, err)
		}
		if containsFold(members, login) {
			return true, nil
		}
	}
	return false, nil
}

// cachedMembers is ResolveGroup(src), reused for ttl.
func (c *Conn) cachedMembers(ctx context.Context, src v1alpha1.GroupSource, ttl time.Duration) ([]string, error) {
	keyBytes, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("group source key: %w", err)
	}
	key := string(keyBytes)
	now := c.clock.Now()
	c.mu.Lock()
	ent, ok := c.memberCache[key]
	c.mu.Unlock()
	if ok && now.Before(ent.at.Add(ttl)) {
		return ent.logins, nil
	}
	logins, err := c.ResolveGroup(ctx, src)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.memberCache[key] = membersEntry{logins: logins, at: now}
	c.mu.Unlock()
	return logins, nil
}

// ResolveGroup implements provider.Conn: the GitHub logins in one member source, sorted and
// without duplicates.
//
//   - orgRole {org, role}: the org's members with that role: "admin" the owners, "member"
//     every member (GET /orgs/{org}/members?role=admin|all);
//   - team "org/slug": the team's members, child teams included (GET
//     /orgs/{org}/teams/{slug}/members);
//   - users: the logins listed.
//
// The org calls use the read identity's org token (members: read). An unknown org or team is
// provider.ErrNotFound; a source for another Provider, with no or several sources, a malformed
// team or an unknown role is provider.ErrInvalid; a userGroup (Slack) is
// provider.ErrUnsupported.
func (c *Conn) ResolveGroup(ctx context.Context, src v1alpha1.GroupSource) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src.Provider != c.name {
		return nil, fmt.Errorf("github %s: resolve group: source is for provider %q: %w", c.name, src.Provider, provider.ErrInvalid)
	}
	if err := src.Validate(); err != nil {
		return nil, fmt.Errorf("github %s: resolve group: %w: %w", c.name, err, provider.ErrInvalid)
	}
	switch {
	case src.Users != nil:
		return sortedUnique(src.Users), nil
	case src.UserGroup != "":
		return nil, fmt.Errorf("github %s: resolve group: userGroup is a Slack source: %w", c.name, provider.ErrUnsupported)
	case src.Team != "":
		org, slug, ok := strings.Cut(src.Team, "/")
		if !ok || org == "" || slug == "" || strings.Contains(slug, "/") {
			return nil, fmt.Errorf("github %s: resolve group: team %q: want org/slug: %w", c.name, src.Team, provider.ErrInvalid)
		}
		return c.members(ctx, org, "orgs/"+org+"/teams/"+slug+"/members", nil)
	default:
		role := map[string]string{"admin": "admin", "member": "all"}[src.OrgRole.Role]
		if role == "" || src.OrgRole.Org == "" || strings.Contains(src.OrgRole.Org, "/") {
			return nil, fmt.Errorf("github %s: resolve group: orgRole %s/%s: want an org and role admin or member: %w",
				c.name, src.OrgRole.Org, src.OrgRole.Role, provider.ErrInvalid)
		}
		return c.members(ctx, src.OrgRole.Org, "orgs/"+src.OrgRole.Org+"/members", url.Values{"role": {role}})
	}
}

// members lists the logins of a member listing on org.
func (c *Conn) members(ctx context.Context, org, path string, query url.Values) ([]string, error) {
	tok, err := c.orgToken(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("github %s: resolve group: %w", c.name, err)
	}
	var logins []string
	err = c.list(ctx, tok, path, query, maxMemberPages, func(item json.RawMessage) (bool, error) {
		var u ghUser
		if err := json.Unmarshal(item, &u); err != nil {
			return false, fmt.Errorf("decode member: %w", err)
		}
		if u.Login != "" {
			logins = append(logins, u.Login)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("github %s: resolve group: %w", c.name, err)
	}
	return sortedUnique(logins), nil
}

func containsFold(list []string, s string) bool {
	return s != "" && slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// sortedUnique returns a sorted copy of ss without duplicates (never nil).
func sortedUnique(ss []string) []string {
	out := append([]string{}, ss...)
	slices.Sort(out)
	return slices.Compact(out)
}
