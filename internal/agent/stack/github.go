package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultGitHubAPI is GitHub's REST API.
const DefaultGitHubAPI = "https://api.github.com"

// maxGitHubResponse bounds a GitHub response.
const maxGitHubResponse = 4 << 20

// GitHub reads public repos through GitHub's REST API: anonymously (CCF's repos are
// public), or with Token when set. It only reads.
type GitHub struct {
	// BaseURL is the REST API; default DefaultGitHubAPI.
	BaseURL string
	// Token, when set, is sent as a bearer token (higher rate limits). Never logged.
	Token string
	// HTTPClient; default a client with a 30 s timeout.
	HTTPClient *http.Client
}

var _ GitHubReader = GitHub{}

// IssueBody implements GitHubReader.
func (g GitHub) IssueBody(ctx context.Context, ref IssueRef) (string, error) {
	var issue struct {
		Body string `json:"body"`
	}
	p := "/repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Repo) + "/issues/" + strconv.Itoa(ref.Number)
	if err := g.get(ctx, p, &issue); err != nil {
		return "", err
	}
	return issue.Body, nil
}

// LatestRelease implements GitHubReader: GitHub's latest release is the newest that is
// neither a draft nor a prerelease.
func (g GitHub) LatestRelease(ctx context.Context, owner, repo string) (string, error) {
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := g.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/releases/latest", &rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("%s/%s: the latest release has no tag", owner, repo)
	}
	return rel.TagName, nil
}

func (g GitHub) get(ctx context.Context, p string, out any) error {
	base := g.BaseURL
	if base == "" {
		base = DefaultGitHubAPI
	}
	hc := g.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubResponse+1))
	if err != nil {
		return fmt.Errorf("GET %s: %w", p, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p, resp.Status)
	}
	if len(b) > maxGitHubResponse {
		return fmt.Errorf("GET %s: the response is over %d bytes", p, maxGitHubResponse)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GET %s: %w", p, err)
	}
	return nil
}
