package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/tokens"
)

// maxCommentPages bounds the comments Project reads to find a marker (100 per page).
const maxCommentPages = 30

// projectIdentity is the identity that writes the markers and whose markers Observe reads:
// the author App (EventIdentity), else the read identity.
func (c *Conn) projectIdentity() string { return c.readIdentity }

// projectPerms is the minimal write permission for comments and labels on an issue or a PR
// (both are the issues API; on a PR, pull_requests: write is enough).
func projectPerms(kind string) map[string]string {
	if kind == provider.KindPR {
		return map[string]string{"pull_requests": "write"}
	}
	return map[string]string{"issues": "write"}
}

// Project implements provider.Conn (github-provider.md §Observe, list and projection): as the
// author identity, reading with observePerms and writing with projectPerms on the object's
// repo, it
//
//  1. ensures the projection's labels and removes the labels the work item's previous marker
//     owned that the projection no longer has (unless another work item's marker on the
//     object owns them too); labels nobody projected, a human's kill switch included, are
//     never touched;
//  2. upserts the work item's marker comment: the oldest marker the App wrote for the work
//     item is edited when its body differs, else one is created.
//
// Labels go first, so a failed marker write is retried with the old ownership still on
// record. Projecting the same Projection again makes no write. Releases, deployments and
// repos carry no marker: projecting onto them does nothing. A missing object, or a ref of the
// other kind, is ErrNotFound.
func (c *Conn) Project(ctx context.Context, ref v1alpha1.ObjectRef, p provider.Projection) error {
	if err := c.project(ctx, ref, p); err != nil {
		return fmt.Errorf("github %s: project %s onto %s: %w", c.name, p.WorkItemID, ref.Ref, err)
	}
	return nil
}

func (c *Conn) project(ctx context.Context, ref v1alpha1.ObjectRef, p provider.Projection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.Provider != "" && ref.Provider != c.name {
		return fmt.Errorf("object is on provider %q: %w", ref.Provider, provider.ErrInvalid)
	}
	switch ref.Kind {
	case KindRepo, KindRelease, KindDeployment:
		c.log.DebugContext(ctx, "nothing to project onto", "ref", ref.Ref, "kind", ref.Kind)
		return nil
	case "", provider.KindIssue, provider.KindPR:
	default:
		return fmt.Errorf("kind %q: %w", ref.Kind, provider.ErrInvalid)
	}
	body, err := formatMarker(p, c.uiBase, true)
	if err != nil {
		return err
	}
	if len(body) > MaxMarkerBytes {
		c.log.WarnContext(ctx, "projection marker too large: state kept in the dispatcher only",
			"workItem", p.WorkItemID, "ref", ref.Ref, "bytes", len(body), "max", MaxMarkerBytes)
		if body, err = formatMarker(p, c.uiBase, false); err != nil {
			return err
		}
		if len(body) > MaxMarkerBytes {
			return fmt.Errorf("marker is %d bytes without state (max %d): %w", len(body), MaxMarkerBytes, provider.ErrInvalid)
		}
	}
	repo, number, err := splitRef(ref.Ref)
	if err != nil {
		return err
	}
	// Read with the read token (an issue ref may name a PR, which a pull_requests-only token
	// couldn't tell from a missing issue); write with the kind's write token.
	readTok, err := c.repoToken(ctx, repo, observePerms)
	if err != nil {
		return err
	}
	issuePath := fmt.Sprintf("repos/%s/issues/%d", repo, number)
	var issue struct {
		Labels      []ghLabel       `json:"labels"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if _, err := c.minter.Get(ctx, readTok, issuePath, nil, &issue); err != nil {
		return err
	}
	kind := provider.KindIssue
	if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
		kind = provider.KindPR
	}
	if ref.Kind != "" && ref.Kind != kind {
		return fmt.Errorf("%s is a %s, not a %s: %w", ref.Ref, kind, ref.Kind, provider.ErrNotFound)
	}
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return err
	}
	comments, err := c.issueComments(ctx, readTok, issuePath)
	if err != nil {
		return err
	}
	tok, err := c.writeToken(ctx, repo, projectPerms(kind))
	if err != nil {
		return err
	}
	markers := c.markers(ctx, comments, bots)
	var mine *parsedMarker
	othersOwn := map[string]bool{} // label names are case-insensitive on GitHub: lower-case keys
	for i, m := range markers {
		if m.p.WorkItemID == p.WorkItemID {
			mine = &markers[i]
			continue
		}
		for _, l := range m.p.Labels {
			othersOwn[strings.ToLower(l)] = true
		}
	}
	current := map[string]bool{}
	for _, l := range issue.Labels {
		current[strings.ToLower(l.Name)] = true
	}
	want := ownedLabels(p.Labels)
	wanted := map[string]bool{}
	var add []string
	for _, l := range want {
		wanted[strings.ToLower(l)] = true
		if !current[strings.ToLower(l)] {
			add = append(add, l)
		}
	}
	if len(add) > 0 {
		if _, err := c.minter.Do(ctx, tok, http.MethodPost, issuePath+"/labels", nil, map[string]any{"labels": add}, http.StatusOK, nil); err != nil {
			return fmt.Errorf("add labels %v: %w", add, err)
		}
	}
	if mine != nil {
		for _, l := range mine.p.Labels {
			if k := strings.ToLower(l); wanted[k] || othersOwn[k] || !current[k] {
				continue
			}
			_, err := c.minter.Do(ctx, tok, http.MethodDelete, issuePath+"/labels/"+url.PathEscape(l), nil, nil, http.StatusOK, nil)
			if err != nil && !errors.Is(err, provider.ErrNotFound) { // already gone
				return fmt.Errorf("remove label %q: %w", l, err)
			}
		}
	}

	switch {
	case mine == nil:
		if _, err := c.minter.Do(ctx, tok, http.MethodPost, issuePath+"/comments", nil, map[string]string{"body": body}, http.StatusCreated, nil); err != nil {
			return fmt.Errorf("create marker: %w", err)
		}
	case mine.body != body:
		path := fmt.Sprintf("repos/%s/issues/comments/%d", repo, mine.id)
		if _, err := c.minter.Do(ctx, tok, http.MethodPatch, path, nil, map[string]string{"body": body}, http.StatusOK, nil); err != nil {
			return fmt.Errorf("update marker: %w", err)
		}
	}
	return nil
}

// issueComments lists an issue's or a PR's conversation comments (up to maxCommentPages).
func (c *Conn) issueComments(ctx context.Context, tok, issuePath string) ([]markerComment, error) {
	var out []markerComment
	err := c.list(ctx, tok, issuePath+"/comments", nil, maxCommentPages, func(item json.RawMessage) (bool, error) {
		var cm ghComment
		if err := json.Unmarshal(item, &cm); err != nil {
			return false, fmt.Errorf("decode comment: %w", err)
		}
		out = append(out, markerComment{id: cm.ID, login: cm.User.Login, body: cm.Body})
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	return out, nil
}

// writeToken mints (or reuses) the projecting identity's token on repo with perms.
func (c *Conn) writeToken(ctx context.Context, repo string, perms map[string]string) (string, error) {
	id := c.projectIdentity()
	cred, err := c.minter.Token(ctx, provider.GitCredentialRequest{Identity: id, Repo: repo, Permissions: perms, TTL: tokens.MaxTTL})
	c.record(id, err)
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}
