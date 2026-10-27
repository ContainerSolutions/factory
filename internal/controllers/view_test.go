package controllers

import (
	"reflect"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/rules"
)

// TestBuildViewDeploy: item.deploy over a stack's merged PRs.
func TestBuildViewDeploy(t *testing.T) {
	const (
		issue = docsRepo + "#70"
		pr1   = docsRepo + "#81"
		pr2   = docsRepo + "#82"
		pr3   = docsRepo + "#83"
	)
	deployed := func(sha string, contains bool) *provider.DeployObservation {
		return &provider.DeployObservation{State: "success", SHA: sha, URL: "https://pages.test/" + sha, ContainsMerge: contains}
	}
	// A linked object: a merged PR (with its deployment), an open PR or an issue.
	type obj struct {
		kind, ref string
		merged    bool
		deploy    *provider.DeployObservation
		at        time.Duration // observed at t0 + at
	}
	tests := []struct {
		name string
		objs []obj
		want *rules.DeployView
	}{
		{"one PR merged and deployed", []obj{
			{kind: provider.KindIssue, ref: issue},
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s1", true)},
		}, &rules.DeployView{State: "success", SHA: "s1", URL: "https://pages.test/s1", ContainsMerge: true}},
		{"two merged, one deployed", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr2, merged: true, deploy: deployed("s1", false)},
		}, &rules.DeployView{State: "success", SHA: "s1", URL: "https://pages.test/s1", ContainsMerge: false}},
		{"two merged, one without a deployment", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr2, merged: true},
		}, &rules.DeployView{State: "success", SHA: "s1", URL: "https://pages.test/s1", ContainsMerge: false}},
		{"both deployed: the latest deployment seen", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr2, merged: true, deploy: deployed("s2", true), at: time.Minute},
		}, &rules.DeployView{State: "success", SHA: "s2", URL: "https://pages.test/s2", ContainsMerge: true}},
		{"the latest deployment seen, whatever the link order", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s2", true), at: time.Minute},
			{kind: provider.KindPR, ref: pr2, merged: true, deploy: deployed("s1", true)},
		}, &rules.DeployView{State: "success", SHA: "s2", URL: "https://pages.test/s2", ContainsMerge: true}},
		{"an unmerged PR is ignored", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr2, merged: true, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr3, deploy: deployed("s3", false), at: time.Minute},
		}, &rules.DeployView{State: "success", SHA: "s1", URL: "https://pages.test/s1", ContainsMerge: true}},
		{"no merged PR", []obj{
			{kind: provider.KindIssue, ref: issue, deploy: deployed("s1", true)},
			{kind: provider.KindPR, ref: pr1, deploy: deployed("s1", false)},
		}, nil},
		{"merged PRs, none deployed yet", []obj{
			{kind: provider.KindPR, ref: pr1, merged: true},
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wi := &v1alpha1.WorkItem{Metadata: v1alpha1.ObjectMeta{Name: "wi_1"}}
			obs := observations{byLink: map[string]provider.Observation{}, at: map[string]time.Time{}, gone: map[string]bool{}}
			for i, o := range tt.objs {
				l := v1alpha1.Link{Provider: ghProvider, Kind: o.kind, Ref: o.ref, Primary: i == 0}
				wi.Spec.Links = append(wi.Spec.Links, l)
				ob := provider.Observation{Ref: v1alpha1.ObjectRef{Provider: ghProvider, Kind: o.kind, Ref: o.ref}, State: provider.StateOpen, Deploy: o.deploy}
				if o.kind == provider.KindPR {
					ob.PR = &provider.PRObservation{Head: "abc"}
				}
				if o.merged {
					ob.State, ob.PR.MergedAt, ob.PR.MergeCommit = provider.StateMerged, t0, "m-"+o.ref
				}
				obs.byLink[cacheKey(l)], obs.at[cacheKey(l)] = ob, t0.Add(o.at)
			}
			if got := buildView(wi, nil, obs).Deploy; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("item.deploy = %+v, want %+v", got, tt.want)
			}
		})
	}
}
