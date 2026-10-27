package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/server"
)

// Context content (contracts.md §Refinements, "Context content"): the untrusted text of a
// task's context items, which the API server fences and labels.

var _ server.ContentReader = (*Conn)(nil)

const (
	// contentMediaType is GitHub's text: Markdown.
	contentMediaType = "text/markdown"
	// maxContentBytes caps what ReadContent returns for a whole issue or PR; later comments
	// are left out, with a note.
	maxContentBytes = 512 << 10
	contentScheme   = "github://"
)

// factoryMarkers matches the factory's hidden markers (projection and effect) anywhere in a
// text, whoever wrote them.
var factoryMarkers = regexp.MustCompile(`<!--\s*factory:[\s\S]*?-->`)

// stripMarkers removes the factory's markers from text.
func stripMarkers(text string) string {
	return strings.TrimSpace(factoryMarkers.ReplaceAllString(text, ""))
}

// contentRef is a parsed context ref.
type contentRef struct {
	repo   string
	number int
	// kind is issue or pr when the ref says (a content ref's issues/ or pull/), else "".
	kind string
	// part is "" (the whole object: title, body and comments), "body", "comment" or "review".
	part string
	id   int64
}

// parseContentRef reads an object ref ("owner/name#42") or a content ref
// ("github://owner/name/issues/42#body", "…/pull/42#body", "…/issues/42#comment-<id>",
// "…/pull/42#review-<id>", as the normaliser writes them).
func parseContentRef(ref string) (contentRef, bool) {
	rest, ok := strings.CutPrefix(ref, contentScheme)
	if !ok {
		repo, n, err := splitRef(ref)
		return contentRef{repo: repo, number: n}, err == nil
	}
	path, frag, ok := strings.Cut(rest, "#")
	parts := strings.Split(path, "/")
	if !ok || len(parts) != 4 {
		return contentRef{}, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n < 1 || !validRepo(parts[0]+"/"+parts[1]) {
		return contentRef{}, false
	}
	cr := contentRef{repo: parts[0] + "/" + parts[1], number: n}
	switch parts[2] {
	case "issues":
		cr.kind = provider.KindIssue
	case "pull":
		cr.kind = provider.KindPR
	default:
		return contentRef{}, false
	}
	idOf := func(prefix string) bool {
		s, ok := strings.CutPrefix(frag, prefix)
		id, err := strconv.ParseInt(s, 10, 64)
		cr.id = id
		return ok && err == nil && id > 0
	}
	switch {
	case frag == "body":
		cr.part = "body"
	case idOf("comment-"):
		cr.part, cr.kind = "comment", "" // conversation comments are issue comments on PRs too
	case cr.kind == provider.KindPR && idOf("review-"):
		cr.part = "review"
	default:
		return contentRef{}, false
	}
	return cr, true
}

// ReadContent implements server.ContentReader: the untrusted text a context item names, as
// Markdown, read with the read identity's token (Observe's permissions). An object ref gives
// the issue's or PR's title, body and conversation comments, each comment headed by its
// author, with the factory's own projection comments left out; a content ref gives that body,
// comment or review. The factory's hidden markers are stripped from every text, so an agent
// never sees (or copies) one. Author is the login that wrote the object, comment or review.
// A missing object, or one of another kind than the ref says, is ErrNotFound; a ref that
// names no GitHub object or content gives no content (the item stays reference-only).
func (c *Conn) ReadContent(ctx context.Context, ref v1alpha1.ObjectRef) (server.Content, error) {
	out, err := c.readContent(ctx, ref)
	if err != nil {
		return server.Content{}, fmt.Errorf("github %s: read content of %s: %w", c.name, ref.Ref, err)
	}
	return out, nil
}

func (c *Conn) readContent(ctx context.Context, ref v1alpha1.ObjectRef) (server.Content, error) {
	if err := ctx.Err(); err != nil {
		return server.Content{}, err
	}
	if ref.Provider != "" && ref.Provider != c.name {
		return server.Content{}, fmt.Errorf("object is on provider %q: %w", ref.Provider, provider.ErrInvalid)
	}
	cr, ok := parseContentRef(ref.Ref)
	if !ok {
		return server.Content{}, nil
	}
	if cr.kind == "" && (ref.Kind == provider.KindIssue || ref.Kind == provider.KindPR) && cr.part == "" {
		cr.kind = ref.Kind
	}
	tok, err := c.repoToken(ctx, cr.repo, observePerms)
	if err != nil {
		return server.Content{}, err
	}
	issuePath := fmt.Sprintf("repos/%s/issues/%d", cr.repo, cr.number)
	switch cr.part {
	case "comment":
		var cm struct {
			ghWritten
			IssueURL string `json:"issue_url"`
		}
		if _, err := c.minter.Get(ctx, tok, fmt.Sprintf("repos/%s/issues/comments/%d", cr.repo, cr.id), nil, &cm); err != nil {
			return server.Content{}, err
		}
		if !strings.HasSuffix(cm.IssueURL, fmt.Sprintf("/issues/%d", cr.number)) {
			return server.Content{}, fmt.Errorf("comment %d is not on #%d: %w", cr.id, cr.number, provider.ErrNotFound)
		}
		return server.Content{MediaType: contentMediaType, Text: stripMarkers(deref(cm.Body)), Author: cm.User.Login}, nil
	case "review":
		var rv ghWritten
		if _, err := c.minter.Get(ctx, tok, fmt.Sprintf("repos/%s/pulls/%d/reviews/%d", cr.repo, cr.number, cr.id), nil, &rv); err != nil {
			return server.Content{}, err
		}
		return server.Content{MediaType: contentMediaType, Text: stripMarkers(deref(rv.Body)), Author: rv.User.Login}, nil
	}
	var o ghObject
	if _, err := c.minter.Get(ctx, tok, issuePath, nil, &o); err != nil {
		return server.Content{}, err
	}
	if cr.kind != "" && cr.kind != o.kind() {
		return server.Content{}, fmt.Errorf("%s is a %s, not a %s: %w", ref.Ref, o.kind(), cr.kind, provider.ErrNotFound)
	}
	if cr.part == "body" {
		return server.Content{MediaType: contentMediaType, Text: stripMarkers(o.body()), Author: o.User.Login}, nil
	}
	bots, err := c.factoryBots(ctx)
	if err != nil {
		return server.Content{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", strings.TrimSpace(o.Title))
	if body := stripMarkers(o.body()); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	left := 0
	err = c.list(ctx, tok, issuePath+"/comments", nil, maxCommentPages, func(item json.RawMessage) (bool, error) {
		var cm ghWritten
		if err := json.Unmarshal(item, &cm); err != nil {
			return false, fmt.Errorf("decode comment: %w", err)
		}
		body := deref(cm.Body)
		if isMarker(body) && bots[strings.ToLower(cm.User.Login)] == c.projectIdentity() {
			return true, nil // the factory's projection: state, not conversation
		}
		text := stripMarkers(body)
		if text == "" {
			return true, nil
		}
		section := fmt.Sprintf("\n---\n\n@%s commented:\n\n%s\n", cm.User.Login, text)
		if left > 0 || b.Len()+len(section) > maxContentBytes {
			left++
			return true, nil
		}
		b.WriteString(section)
		return true, nil
	})
	if err != nil {
		return server.Content{}, fmt.Errorf("list comments: %w", err)
	}
	if left > 0 {
		fmt.Fprintf(&b, "\n---\n\n(%d more comments not shown)\n", left)
	}
	return server.Content{MediaType: contentMediaType, Text: b.String(), Author: o.User.Login}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
