package fake

import (
	"context"
	"fmt"
	"slices"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
)

// Observe implements provider.Conn: a copy of the seeded (and since changed) object.
func (c *Conn) Observe(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	if err := c.begin(ctx, MethodObserve); err != nil {
		return provider.Observation{}, err
	}
	defer c.mu.Unlock()
	o, err := c.lookupLocked(ref)
	if err != nil {
		return provider.Observation{}, fmt.Errorf("fake: observe: %w", err)
	}
	return cloneObservation(*o), nil
}

// SetTeam sets the members of a team ("org/team-slug").
func (c *Conn) SetTeam(team string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teams[team] = sortedUnique(logins)
}

// SetOrgRole sets the members of an org with a role (e.g. the admins).
func (c *Conn) SetOrgRole(org, role string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgRoles[v1alpha1.OrgRole{Org: org, Role: role}] = sortedUnique(logins)
}

// SetUserGroup sets the members of a user group (Slack).
func (c *Conn) SetUserGroup(name string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userGroups[name] = sortedUnique(logins)
}

// ResolveGroup implements provider.Conn from the membership set with SetTeam, SetOrgRole and
// SetUserGroup; users are returned as listed. A team, org role or user group that was never
// set is ErrNotFound; a source for another provider, or with not exactly one source, is
// ErrInvalid.
func (c *Conn) ResolveGroup(ctx context.Context, g v1alpha1.GroupSource) ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("fake: resolve group: %w: %w", err, provider.ErrInvalid)
	}
	if err := c.begin(ctx, MethodResolveGroup); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if g.Provider != c.p.Metadata.Name {
		return nil, fmt.Errorf("fake: resolve group: source is for provider %q, not %q: %w",
			g.Provider, c.p.Metadata.Name, provider.ErrInvalid)
	}
	var (
		members []string
		ok      bool
		what    string
	)
	switch {
	case g.Team != "":
		members, ok = c.teams[g.Team]
		what = "team " + g.Team
	case g.OrgRole != nil:
		members, ok = c.orgRoles[*g.OrgRole]
		what = fmt.Sprintf("org %s role %s", g.OrgRole.Org, g.OrgRole.Role)
	case g.UserGroup != "":
		members, ok = c.userGroups[g.UserGroup]
		what = "user group " + g.UserGroup
	default:
		return sortedUnique(g.Users), nil
	}
	if !ok {
		return nil, fmt.Errorf("fake: resolve group: %s: %w", what, provider.ErrNotFound)
	}
	return slices.Clone(members), nil
}
