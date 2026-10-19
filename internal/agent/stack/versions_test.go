package stack

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// trainBody is a real tracking issue body from the workflows release train (the GitHub
// provider's fixture, issues.closed-train).
const trainBody = "Release train 2026-11 over `repos.yaml`: **finished**.\n\n| Stage | Repo | Status |\n| --- | --- | --- |\n| 1 | api | released |\n\n" +
	"<!-- train-state\n" +
	`{"month":"2026-11","manifest":"repos.yaml","status":"finished","repos":[` +
	`{"name":"api","stage":1,"phase":"released","from":{".":"1.3.2"},"versions":{".":"1.4.0"},"tags":["v1.4.0"],"release_pr":412},` +
	`{"name":"agent","stage":2,"phase":"released","from":{".":"0.9.1"},"versions":{".":"0.9.2"},"tags":["v0.9.2"]},` +
	`{"name":"plugin-github-repositories","stage":2,"phase":"released","from":{".":"0.8.1"},"versions":{".":"0.9.0"}},` +
	`{"name":"plugin-github-repositories-policies","stage":2,"phase":"released","versions":{".":"0.7.1"}},` +
	`{"name":"helm-charts","stage":3,"phase":"released","from":{"charts/ccf":"2.1.0","charts/agent":"1.0.0"},"versions":{"charts/ccf":"3.0.0","charts/agent":"1.0.1"}},` +
	`{"name":"ui","stage":3,"phase":"skipped","detail":"skipped by admin","from":{".":"2.0.0"}}]}` +
	"\n-->\n"

// fakeGitHub serves issue bodies and latest releases.
type fakeGitHub struct {
	bodies map[string]string // owner/repo#n
	latest map[string]string // repo
	calls  []string
}

func (f *fakeGitHub) IssueBody(_ context.Context, ref IssueRef) (string, error) {
	k := fmt.Sprintf("%s/%s#%d", ref.Owner, ref.Repo, ref.Number)
	f.calls = append(f.calls, "issue "+k)
	b, ok := f.bodies[k]
	if !ok {
		return "", errors.New("404 Not Found")
	}
	return b, nil
}

func (f *fakeGitHub) LatestRelease(_ context.Context, owner, repo string) (string, error) {
	f.calls = append(f.calls, "latest "+owner+"/"+repo)
	t, ok := f.latest[repo]
	if !ok {
		return "", errors.New("404 Not Found")
	}
	return t, nil
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		bodies: map[string]string{"compliance-framework/workflows#123": trainBody},
		latest: map[string]string{"api": "v1.3.2", "ui": "v2.13.0", "agent": "v0.9.1"},
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		src     Source
		want    StackVersions
		wantErr string
	}{
		{
			name: "train: released components pinned, the skipped ui at its latest",
			src:  Source{Train: "compliance-framework/workflows#123"},
			want: StackVersions{API: "1.4.0", UI: "2.13.0", Agent: "0.9.2", Plugins: map[string]string{"plugin-github-repositories": "0.9.0"}},
		},
		{
			name: "release",
			src:  Source{Repo: "compliance-framework/ui", Tag: "v2.14.0-rc.1"},
			want: StackVersions{API: "1.3.2", UI: "2.14.0-rc.1", Agent: "0.9.1"},
		},
		{
			name: "plugin release",
			src:  Source{Repo: "plugin-aws", Tag: "v1.2.0"},
			want: StackVersions{API: "1.3.2", UI: "2.13.0", Agent: "0.9.1", Plugins: map[string]string{"plugin-aws": "1.2.0"}},
		},
		{name: "nothing: every latest", want: StackVersions{API: "1.3.2", UI: "2.13.0", Agent: "0.9.1"}},
		{name: "unknown train", src: Source{Train: "compliance-framework/workflows#9"}, wantErr: "404"},
		{name: "bad train ref", src: Source{Train: "workflows"}, wantErr: "owner/repo#number"},
		{name: "make injection", src: Source{Repo: "api", Tag: "$(shell id)"}, wantErr: "not an image tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(context.Background(), tt.src, newFakeGitHub())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("versions\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestFromTrainDryRun(t *testing.T) {
	tf, err := ParseTrainState(strings.Replace(trainBody, `"status":"finished"`, `"status":"finished","dry_run":true`, 1))
	if err != nil {
		t.Fatal(err)
	}
	v, err := FromTrain(tf)
	if err != nil || !reflect.DeepEqual(v, StackVersions{}) {
		t.Errorf("a dry run pins %+v, %v", v, err)
	}
}

func TestParseTrainState(t *testing.T) {
	for _, body := range []string{"no state", "<!-- train-state {", "<!-- train-state {} -->", "<!-- train-state nope -->"} {
		if _, err := ParseTrainState(body); err == nil {
			t.Errorf("ParseTrainState(%q) passed", body)
		}
	}
}

func TestSourceFromInputs(t *testing.T) {
	tests := []struct {
		in      map[string]any
		want    Source
		wantErr bool
	}{
		{in: map[string]any{"train": "compliance-framework/workflows#123"}, want: Source{Train: "compliance-framework/workflows#123"}},
		{in: map[string]any{"verifyAgainst": map[string]any{"train": "compliance-framework/workflows#7"}}, want: Source{Train: "compliance-framework/workflows#7"}},
		{in: map[string]any{"verifyAgainst": map[string]any{"repo": "compliance-framework/api", "tag": "v0.22.0"}}, want: Source{Repo: "compliance-framework/api", Tag: "v0.22.0"}},
		{in: map[string]any{"train": "a/b#1", "verifyAgainst": map[string]any{"repo": "x", "tag": "y"}}, want: Source{Train: "a/b#1"}},
		{in: map[string]any{"verifyAgainst": map[string]any{"repo": "api"}}, wantErr: true},
		{in: map[string]any{}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := SourceFromInputs(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("SourceFromInputs(%v) = %+v, %v", tt.in, got, err)
		}
	}
}

func TestDecodeVersions(t *testing.T) {
	v, err := DecodeVersions([]byte(`{"api":"v0.22.0","plugins":{"plugin-aws":"v1.0.0"}}`))
	if err != nil || v.API != "0.22.0" || v.Plugins["plugin-aws"] != "1.0.0" || v.UI != "" {
		t.Errorf("DecodeVersions = %+v, %v", v, err)
	}
	for _, bad := range []string{`{"api":"1;rm"}`, `{"apy":"1"}`, `{"plugins":{"../x":"1"}}`, `[`} {
		if _, err := DecodeVersions([]byte(bad)); err == nil {
			t.Errorf("DecodeVersions(%s) passed", bad)
		}
	}
}

func TestParseRelease(t *testing.T) {
	if s, err := ParseRelease("compliance-framework/api@v0.22.0"); err != nil || s.Repo != "compliance-framework/api" || s.Tag != "v0.22.0" {
		t.Errorf("ParseRelease = %+v, %v", s, err)
	}
	for _, bad := range []string{"api", "@v1", "api@"} {
		if _, err := ParseRelease(bad); err == nil {
			t.Errorf("ParseRelease(%q) passed", bad)
		}
	}
}
