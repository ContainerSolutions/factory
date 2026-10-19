package github_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/github"
)

// TestMarkerFormat pins the LLD's example.
func TestMarkerFormat(t *testing.T) {
	p := provider.Projection{
		WorkItemID: "wi_01JA",
		State:      map[string]any{"release": "drafting", "train": "compliance-framework/workflows#123"},
		Counters:   map[string]int{"release-docs": 1},
		Links:      []v1alpha1.Link{{Provider: "github-public", Ref: "compliance-framework/docs#57"}},
	}
	got, err := github.FormatMarker(p, "https://factory.example/", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- factory:wi v=1 id=wi_01JA state=release:drafting,train:compliance-framework%2Fworkflows%23123 runs=release-docs:1 links=github-public:compliance-framework%2Fdocs%2357 -->\n" +
		"🏭 Tracked by CCF Factory — [view](https://factory.example/workitems/wi_01JA)"
	if got != want {
		t.Errorf("marker =\n%s\nwant\n%s", got, want)
	}
	if got, _ := github.FormatMarker(p, "", true); strings.Contains(got, "[view]") {
		t.Errorf("no UI base, still a link: %s", got)
	}
	if got, _ := github.FormatMarker(p, "", false); strings.Contains(got, "state=") {
		t.Errorf("without state: %s", got)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	for name, p := range map[string]provider.Projection{
		"id only": {WorkItemID: "wi_1"},
		"everything": {
			WorkItemID: "wi_01JA9ZX",
			State: map[string]any{
				"release":  "drafting",
				"plan":     map[string]any{"parts": []any{"a", "b"}, "size": float64(2), "ok": true, "none": nil},
				"count":    float64(3),
				"done":     false,
				"empty":    "",
				"tricky":   "a b,c:d=e%f-->g<h>&\"' ü 🏭",
				"b64:look": "b64:not-base64",
				"list":     []any{"x", float64(1)},
				"":         "empty key",
			},
			Counters: map[string]int{"release-docs": 1, "docs review": 12, "zero": 0},
			Links: []v1alpha1.Link{
				{Provider: "github-public", Ref: "compliance-framework/docs#57", Kind: "issue", Primary: true},
				{Provider: "github-public", Ref: "compliance-framework/docs#90", Kind: "pr"},
				{Provider: "slack-ccf", Ref: "C123/1700000000.000100"},
				{Provider: "github-public", Ref: "compliance-framework/docs", Primary: true},
			},
			Labels: []string{"factory:release", "factory:hold, maybe"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := github.FormatMarker(p, "https://factory.example", true)
			if err != nil {
				t.Fatal(err)
			}
			head, _, _ := strings.Cut(body, "\n")
			if strings.Count(head, "-->") != 1 || !strings.HasSuffix(head, " -->") {
				t.Errorf("the header must end the HTML comment once: %s", head)
			}
			got, ok, err := github.ParseMarker(body)
			if err != nil || !ok {
				t.Fatalf("ParseMarker = %v, %v", ok, err)
			}
			want := p
			if want.Labels != nil {
				want.Labels = []string{"factory:hold, maybe", "factory:release"} // sorted
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip:\n got %#v\nwant %#v", got, want)
			}
			again, _ := github.FormatMarker(got, "https://factory.example", true)
			if again != body {
				t.Errorf("not stable:\n%s\n%s", body, again)
			}
		})
	}
}

func TestMarkerParse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok, err    bool
	}{
		{"not a marker", "LGTM", false, false},
		{"effect marker", "done <!-- factory:effect k -->", false, false},
		{"other version", "<!-- factory:wi v=2 id=wi_1 -->", false, true},
		{"no version", "<!-- factory:wi id=wi_1 -->", false, true},
		{"no end", "<!-- factory:wi v=1 id=wi_1", false, true},
		{"no id", "<!-- factory:wi v=1 state=a:b -->", false, true},
		{"bad state", "<!-- factory:wi v=1 id=wi_1 state=nocolon -->", false, true},
		{"bad b64", "<!-- factory:wi v=1 id=wi_1 state=a:b64:%%% -->", false, true},
		{"bad count", "<!-- factory:wi v=1 id=wi_1 runs=a:x -->", false, true},
		{"bad link", "<!-- factory:wi v=1 id=wi_1 links=a:b:c:d -->", false, true},
		{"bad escape", "<!-- factory:wi v=1 id=wi_%zz -->", false, true},
		{"unknown field", "<!-- factory:wi v=1 id=wi_1 future=x -->", true, false},
		{"empty field", "<!-- factory:wi v=1 id=wi_1 state= -->", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := github.ParseMarker(tc.body)
			if ok != tc.ok || (err != nil) != tc.err {
				t.Errorf("ParseMarker = %v, %v; want ok %v, error %v", ok, err, tc.ok, tc.err)
			}
		})
	}
	if _, err := github.FormatMarker(provider.Projection{}, "", true); err == nil {
		t.Error("a projection without a work item formatted")
	}
	if _, err := github.FormatMarker(provider.Projection{WorkItemID: "wi_1", State: map[string]any{"f": func() {}}}, "", true); err == nil {
		t.Error("a state value that isn't JSON formatted")
	}
}
