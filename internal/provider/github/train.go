package github

import (
	"encoding/json"
	"strings"
)

// The ccf-train extractor reads the state CCF's release train (compliance-framework/workflows,
// internal/train) keeps in its tracking issue's body, as hidden JSON:
//
//	<!-- train-state
//	{"month":"2026-11","manifest":"repos.yaml","status":"finished","repos":[…]}
//	-->
//
// The train marshals the JSON with encoding/json, which escapes "<" and ">", so the JSON can't
// contain the closing "-->".
const (
	trainStateOpen  = "<!-- train-state"
	trainStateClose = "-->"
)

// trainState is the part of the train's State the facts carry.
type trainState struct {
	Month    string `json:"month"`
	Manifest string `json:"manifest"`
	// DryRun is written with omitempty, so a real train omits it.
	DryRun bool `json:"dry_run"`
	Repos  []struct {
		Name string `json:"name"`
		// From is the repo's release-please manifest (package → version) before the train,
		// i.e. its previous release; Versions what the train released (or plans, dry).
		From     map[string]string `json:"from"`
		Versions map[string]string `json:"versions"`
	} `json:"repos"`
}

// trainFacts is event.train for a tracking issue body: {month, manifest, dryRun (false when
// absent), repos[]{name, versions, bump}} in stage order, repos [] when absent. ok is false for
// a body without a train state, with one that isn't valid JSON, or with no manifest.
//
// A repo's bump is the largest of its packages' bumps from the previous release (From) to the
// released version: major, minor, patch or none; "major" for a package with no previous
// version (its first release), "none" for a repo that released nothing.
func trainFacts(body string) (map[string]any, bool) {
	_, rest, ok := strings.Cut(body, trainStateOpen)
	if !ok {
		return nil, false
	}
	data, _, ok := strings.Cut(rest, trainStateClose)
	if !ok {
		return nil, false
	}
	var s trainState
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &s); err != nil || s.Manifest == "" {
		return nil, false
	}
	repos := make([]any, 0, len(s.Repos))
	for _, r := range s.Repos {
		versions := make(map[string]any, len(r.Versions))
		b := "none"
		for pkg, to := range r.Versions {
			versions[pkg] = to
			b = maxBump(b, packageBump(r.From[pkg], to))
		}
		repos = append(repos, map[string]any{"name": r.Name, "versions": versions, "bump": b})
	}
	return map[string]any{"month": s.Month, "manifest": s.Manifest, "dryRun": s.DryRun, "repos": repos}, true
}

// packageBump is one package's bump; an unparsable version counts as major (the docs then
// cover everything).
func packageBump(from, to string) string {
	_, tv, ok := parseTag(to)
	if !ok {
		return "major"
	}
	if from == "" {
		return bump("", tv)
	}
	_, fv, ok := parseTag(from)
	if !ok {
		return "major"
	}
	return bump(fv, tv)
}

var bumpRank = map[string]int{"none": 0, "patch": 1, "minor": 2, "major": 3}

func maxBump(a, b string) string {
	if bumpRank[b] > bumpRank[a] {
		return b
	}
	return a
}
