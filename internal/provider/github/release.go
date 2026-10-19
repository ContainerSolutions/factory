package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// maxReleasePages bounds the releases read to find a previous tag (100 per page).
const maxReleasePages = 10

// tagPattern splits a release tag into a component prefix and a semantic version:
// "v1.4.0" → ("", "1.4.0"), "mock-app-v0.2.0" → ("mock-app-", "0.2.0") (release-please's
// component tags).
var tagPattern = regexp.MustCompile(`^(.*?)v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$`)

// parseTag returns a tag's prefix and its canonical semver ("v1.4.0-rc.1").
func parseTag(tag string) (prefix, version string, ok bool) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil || !semver.IsValid("v"+m[2]) {
		return "", "", false
	}
	return m[1], "v" + m[2], true
}

// core returns major, minor and patch of a valid semver.
func core(v string) [3]int64 {
	var out [3]int64
	c := strings.TrimPrefix(semver.Canonical(v), "v")
	if i := strings.IndexAny(c, "-+"); i >= 0 {
		c = c[:i]
	}
	for i, p := range strings.SplitN(c, ".", 3) {
		out[i], _ = strconv.ParseInt(p, 10, 64)
	}
	return out
}

// bump is the most significant of major, minor and patch that differs from from to to, or
// "none" (as the CEL semver.bump). With no from, it is "major": a first release documents
// everything.
func bump(from, to string) string {
	if from == "" {
		return "major"
	}
	a, b := core(from), core(to)
	for i, name := range [3]string{"major", "minor", "patch"} {
		if a[i] != b[i] {
			return name
		}
	}
	return "none"
}

// releaseFacts is event.release: tag, and for a semver tag previousTag (the previous final
// release of the same component by semver, if any) and semver{major, minor, patch,
// prerelease, bump}. prerelease is GitHub's flag or a prerelease version.
func (c *Conn) releaseFacts(ctx context.Context, repo string, rel *ghRelease) (map[string]any, error) {
	facts := map[string]any{"tag": rel.TagName}
	prefix, v, ok := parseTag(rel.TagName)
	if !ok {
		return facts, nil
	}
	prev, prevV, err := c.previousFinal(ctx, repo, prefix, v)
	if err != nil {
		return nil, fmt.Errorf("previous release of %s: %w", repo, err)
	}
	if prev != "" {
		facts["previousTag"] = prev
	}
	n := core(v)
	facts["semver"] = map[string]any{
		"major": n[0], "minor": n[1], "patch": n[2],
		"prerelease": rel.Prerelease || semver.Prerelease(v) != "",
		"bump":       bump(prevV, v),
	}
	return facts, nil
}

// previousFinal finds the newest final (not draft, not prerelease) release of repo whose tag
// has prefix and a version below v.
func (c *Conn) previousFinal(ctx context.Context, repo, prefix, v string) (tag, version string, err error) {
	tok, err := c.repoToken(ctx, repo, map[string]string{"contents": "read"})
	if err != nil {
		return "", "", err
	}
	err = c.list(ctx, tok, "repos/"+repo+"/releases", nil, maxReleasePages, func(item json.RawMessage) (bool, error) {
		var r ghRelease
		if err := json.Unmarshal(item, &r); err != nil {
			return false, fmt.Errorf("decode release: %w", err)
		}
		if r.Draft || r.Prerelease {
			return true, nil
		}
		p, rv, ok := parseTag(r.TagName)
		if !ok || p != prefix || semver.Prerelease(rv) != "" || semver.Compare(rv, v) >= 0 {
			return true, nil
		}
		if version == "" || semver.Compare(rv, version) > 0 {
			tag, version = r.TagName, rv
		}
		return true, nil
	})
	if err != nil {
		return "", "", err
	}
	return tag, version, nil
}
