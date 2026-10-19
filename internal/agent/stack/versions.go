package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Org is the GitHub organisation of the CCF components.
const Org = "compliance-framework"

// The components local-dev pins through its environment, by repo name.
const (
	RepoAPI   = "api"
	RepoUI    = "ui"
	RepoAgent = "agent"
)

// StackVersions are the component versions to bring up (local-stack.md §Versions to pin),
// without a leading "v": the image tags. An empty API, UI or Agent is resolved to the
// component's latest final release (Resolve). Plugins are recorded for the skills only:
// local-dev pins the plugins in its agent configs, not through its environment.
type StackVersions struct {
	API     string            `json:"api,omitzero"`
	UI      string            `json:"ui,omitzero"`
	Agent   string            `json:"agent,omitzero"`
	Plugins map[string]string `json:"plugins,omitzero"`
}

// versionRE is a version usable as an image tag and as a make command-line value: make
// would expand a "$" in a value, so nothing outside docker's tag alphabet passes.
var versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

// cleanVersion strips a leading "v" and checks the result is a tag.
func cleanVersion(v string) (string, error) {
	t := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !versionRE.MatchString(t) {
		return "", fmt.Errorf("version %q is not an image tag", v)
	}
	return t, nil
}

// Validate checks every version is an image tag and every plugin name a repo name.
func (v StackVersions) Validate() error {
	for _, c := range []struct{ name, v string }{{RepoAPI, v.API}, {RepoUI, v.UI}, {RepoAgent, v.Agent}} {
		if c.v == "" {
			continue
		}
		if !versionRE.MatchString(c.v) {
			return fmt.Errorf("%s: version %q is not an image tag", c.name, c.v)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(v.Plugins)) {
		if !repoNameRE.MatchString(name) {
			return fmt.Errorf("plugin %q is not a repo name", name)
		}
		if !versionRE.MatchString(v.Plugins[name]) {
			return fmt.Errorf("plugin %s: version %q is not an image tag", name, v.Plugins[name])
		}
	}
	return nil
}

// set pins the component a repo is (by its name, with or without the org) to version.
// Repos that aren't api, ui, agent or a plugin (helm-charts, docs, policies) pin nothing.
func (v *StackVersions) set(repo, version string) error {
	name := path.Base(repo)
	ver, err := cleanVersion(version)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	switch {
	case name == RepoAPI:
		v.API = ver
	case name == RepoUI:
		v.UI = ver
	case name == RepoAgent:
		v.Agent = ver
	case strings.HasPrefix(name, "plugin-") && !strings.HasSuffix(name, "-policies"):
		if !repoNameRE.MatchString(name) {
			return fmt.Errorf("plugin %q is not a repo name", name)
		}
		if v.Plugins == nil {
			v.Plugins = map[string]string{}
		}
		v.Plugins[name] = ver
	}
	return nil
}

// DecodeVersions reads a versions file: StackVersions as JSON, unknown keys refused.
func DecodeVersions(b []byte) (StackVersions, error) {
	var v StackVersions
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return StackVersions{}, fmt.Errorf("versions: %w", err)
	}
	for _, p := range []*string{&v.API, &v.UI, &v.Agent} {
		if *p != "" {
			*p = strings.TrimPrefix(*p, "v")
		}
	}
	for k, p := range v.Plugins {
		v.Plugins[k] = strings.TrimPrefix(p, "v")
	}
	if err := v.Validate(); err != nil {
		return StackVersions{}, fmt.Errorf("versions: %w", err)
	}
	return v, nil
}

// TrainFacts is the part of a release train's state the stack needs: the train state JSON
// the workflows release train keeps in its tracking issue (the same block the GitHub
// provider's ccf-train extractor reads).
type TrainFacts struct {
	Month    string      `json:"month"`
	Manifest string      `json:"manifest"`
	DryRun   bool        `json:"dry_run"`
	Repos    []TrainRepo `json:"repos"`
}

// TrainRepo is one repo of a train: Versions maps each release-please package ("." for a
// single-package repo) to the version the train released; a skipped repo has none.
type TrainRepo struct {
	Name     string            `json:"name"`
	Versions map[string]string `json:"versions"`
}

const (
	trainStateOpen  = "<!-- train-state"
	trainStateClose = "-->"
)

// ParseTrainState reads the train state from a tracking issue's body.
func ParseTrainState(body string) (TrainFacts, error) {
	_, rest, ok := strings.Cut(body, trainStateOpen)
	if !ok {
		return TrainFacts{}, errors.New("the issue has no train state")
	}
	data, _, ok := strings.Cut(rest, trainStateClose)
	if !ok {
		return TrainFacts{}, errors.New("the train state is not closed")
	}
	var t TrainFacts
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &t); err != nil {
		return TrainFacts{}, fmt.Errorf("the train state: %w", err)
	}
	if t.Manifest == "" {
		return TrainFacts{}, errors.New("the train state has no manifest")
	}
	return t, nil
}

// FromTrain pins every component the train released. A dry-run train released nothing (its
// versions are only planned), so it pins nothing and every component runs its latest final
// release.
func FromTrain(t TrainFacts) (StackVersions, error) {
	var v StackVersions
	if t.DryRun {
		return v, nil
	}
	for _, r := range t.Repos {
		ver := r.Versions["."]
		if ver == "" && len(r.Versions) == 1 {
			for _, only := range r.Versions {
				ver = only
			}
		}
		if ver == "" {
			continue // skipped, or a multi-package repo (helm-charts): no image to pin
		}
		if err := v.set(r.Name, ver); err != nil {
			return StackVersions{}, fmt.Errorf("train repo %w", err)
		}
	}
	return v, nil
}

// FromRelease pins the one component a release is: repo is "api" or "compliance-framework/api",
// tag "v0.22.0".
func FromRelease(repo, tag string) (StackVersions, error) {
	var v StackVersions
	if err := v.set(repo, tag); err != nil {
		return StackVersions{}, err
	}
	return v, nil
}

// Source says which versions a stack runs: a train (its tracking issue, "owner/repo#n"), or
// one release (Repo at Tag). The zero Source is every component's latest final release.
type Source struct {
	Train     string
	Repo, Tag string
}

// IsZero reports whether s names nothing.
func (s Source) IsZero() bool { return s.Train == "" && s.Repo == "" }

// String is s for messages.
func (s Source) String() string {
	switch {
	case s.Train != "":
		return "train " + s.Train
	case s.Repo != "":
		return s.Repo + "@" + s.Tag
	}
	return "latest releases"
}

// ParseRelease reads "repo@tag".
func ParseRelease(s string) (Source, error) {
	repo, tag, ok := strings.Cut(s, "@")
	if !ok || repo == "" || tag == "" {
		return Source{}, fmt.Errorf("release %q: want <repo>@<tag>", s)
	}
	return Source{Repo: repo, Tag: tag}, nil
}

// SourceFromInputs reads a task's inputs (run-pipeline.md §Steps 4): `train` is the train's
// tracking issue ("compliance-framework/workflows#123", release-docs); `verifyAgainst` is
// {train: <ref>} or {repo, tag} (docs-review). train wins when both are set.
func SourceFromInputs(in map[string]any) (Source, error) {
	if t, ok := in["train"].(string); ok && t != "" {
		return Source{Train: t}, nil
	}
	va, ok := in["verifyAgainst"].(map[string]any)
	if !ok || len(va) == 0 {
		return Source{}, errors.New("the task's inputs name no versions (train or verifyAgainst)")
	}
	if t, ok := va["train"].(string); ok && t != "" {
		return Source{Train: t}, nil
	}
	repo, _ := va["repo"].(string)
	tag, _ := va["tag"].(string)
	if repo == "" || tag == "" {
		return Source{}, errors.New("verifyAgainst: want {train} or {repo, tag}")
	}
	return Source{Repo: repo, Tag: tag}, nil
}

// repoNameRE is a GitHub owner or repo name.
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// IssueRef is a GitHub issue: owner/repo#number.
type IssueRef struct {
	Owner, Repo string
	Number      int
}

// ParseIssueRef reads "owner/repo#n".
func ParseIssueRef(s string) (IssueRef, error) {
	repo, num, ok := strings.Cut(s, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.Atoi(num)
	if !ok || !ok2 || err != nil || n <= 0 || !repoNameRE.MatchString(owner) || !repoNameRE.MatchString(name) {
		return IssueRef{}, fmt.Errorf("train %q: want owner/repo#number", s)
	}
	return IssueRef{Owner: owner, Repo: name, Number: n}, nil
}

// GitHubReader reads what the versions come from: the train's tracking issue and the
// components' latest final releases. GitHub is the real one; tests fake it.
type GitHubReader interface {
	IssueBody(ctx context.Context, ref IssueRef) (string, error)
	// LatestRelease is the tag of the repo's latest final release (no draft, no prerelease).
	LatestRelease(ctx context.Context, owner, repo string) (string, error)
}

// Resolve works out the versions of src: the train's or the release's pins, then the latest
// final release of every component left unpinned.
func Resolve(ctx context.Context, src Source, gh GitHubReader) (StackVersions, error) {
	var (
		v   StackVersions
		err error
	)
	switch {
	case src.Train != "":
		ref, perr := ParseIssueRef(src.Train)
		if perr != nil {
			return StackVersions{}, perr
		}
		body, berr := gh.IssueBody(ctx, ref)
		if berr != nil {
			return StackVersions{}, fmt.Errorf("train %s: %w", src.Train, berr)
		}
		t, terr := ParseTrainState(body)
		if terr != nil {
			return StackVersions{}, fmt.Errorf("train %s: %w", src.Train, terr)
		}
		v, err = FromTrain(t)
	case src.Repo != "":
		v, err = FromRelease(src.Repo, src.Tag)
	}
	if err != nil {
		return StackVersions{}, err
	}
	return FillLatest(ctx, v, gh)
}

// FillLatest sets every unpinned API, UI and Agent to its latest final release.
func FillLatest(ctx context.Context, v StackVersions, gh GitHubReader) (StackVersions, error) {
	for _, c := range []struct {
		repo string
		p    *string
	}{{RepoAPI, &v.API}, {RepoUI, &v.UI}, {RepoAgent, &v.Agent}} {
		if *c.p != "" {
			continue
		}
		tag, err := gh.LatestRelease(ctx, Org, c.repo)
		if err != nil {
			return StackVersions{}, fmt.Errorf("the latest %s release: %w", c.repo, err)
		}
		ver, err := cleanVersion(tag)
		if err != nil {
			return StackVersions{}, fmt.Errorf("the latest %s release: %w", c.repo, err)
		}
		*c.p = ver
	}
	return v, v.Validate()
}
