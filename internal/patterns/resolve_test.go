package patterns

import (
	"errors"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func mustCompile(t *testing.T, v any) Pattern {
	t.Helper()
	if v == nil {
		return nil
	}
	p, err := Compile(v)
	if err != nil {
		t.Fatalf("Compile(%#v): %v", v, err)
	}
	return p
}

var claude5 = []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5"}

func TestResolve(t *testing.T) {
	type side struct {
		harness       string
		model, effort any // nil = unset (matches anything)
	}
	tests := []struct {
		name       string
		constraint []side
		request    side
		prefer     []string
		offer      Offer
		want       Resolved
		wantReason Reason
	}{
		// The rows of the LLD's table, in order.
		{
			name:       "lld 1: highest version, tie broken by string order",
			constraint: []side{{harness: "claude-code", model: "claude-*-5-*"}},
			request:    side{harness: "claude-code", model: "claude-{sonnet,opus}-5-*"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"low", "medium", "high"}},
			want:       Resolved{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "medium"},
		},
		{
			name:       "lld 1: the agent's default wins over the highest version",
			constraint: []side{{harness: "claude-code", model: "claude-*-5-*"}},
			request:    side{harness: "claude-code", model: "claude-{sonnet,opus}-5-*"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}, DefaultModel: "claude-sonnet-5-5"},
			want:       Resolved{Harness: "claude-code", Model: "claude-sonnet-5-5", Effort: "medium"},
		},
		{
			name:       "lld 2: prefer wins over the highest version",
			constraint: []side{{harness: "codex", model: "gpt-5*"}},
			request:    side{harness: "codex", model: "gpt-5*"},
			prefer:     []string{"gpt-5-codex"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex", "gpt-5.1-codex"}, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "medium"},
		},
		{
			name:       "lld 3: request outside the constraint",
			constraint: []side{{harness: "claude-code", model: "claude-{sonnet,haiku}-5-*"}},
			request:    side{harness: "claude-code", model: "claude-opus-5-*"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}},
			wantReason: ReasonModel,
		},
		{
			name:       "lld 4: requested effort outside the constraint",
			constraint: []side{{harness: "codex", effort: []string{"low", "medium"}}},
			request:    side{harness: "codex", effort: "high"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "medium", "high"}},
			wantReason: ReasonEffort,
		},
		{
			name:       "lld 5: effort * without medium takes the first offered",
			constraint: []side{{harness: "codex", effort: "*"}},
			request:    side{harness: "codex", effort: "*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "high"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "low"},
		},
		{
			name:       "lld 6: harness not allowed",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "claude-code"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}},
			wantReason: ReasonHarness,
		},
		{
			name:       "lld 7: regex constraint",
			constraint: []side{{harness: "codex", model: `re:^gpt-5(\.\d+)?-codex$`}},
			request:    side{harness: "codex", model: "*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex", "gpt-5-mini"}, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "medium"},
		},

		// Beyond the table.
		{
			name:       "lld 2 without prefer: highest version",
			constraint: []side{{harness: "codex", model: "gpt-5*"}},
			request:    side{harness: "codex", model: "gpt-5*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex", "gpt-5.1-codex"}, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5.1-codex", Effort: "medium"},
		},
		{
			name:       "prefer wins over the default; misses and out-of-constraint entries are skipped",
			constraint: []side{{harness: "claude-code", model: "claude-{sonnet,opus}-5-*"}},
			request:    side{harness: "claude-code"},
			prefer:     []string{"claude-haiku-5-5", "claude-opus-4-1", "claude-opus-5-5"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}, DefaultModel: "claude-sonnet-5-5"},
			want:       Resolved{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "medium"},
		},
		{
			name:       "prefer is exact, not a pattern",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex"},
			prefer:     []string{"gpt-5*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex", "gpt-5.1-codex"}, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5.1-codex", Effort: "medium"},
		},
		{
			name:       "a default outside the candidates is ignored",
			constraint: []side{{harness: "claude-code", model: "claude-opus-*"}},
			request:    side{harness: "claude-code"},
			offer:      Offer{Harness: "claude-code", Models: append([]string{"claude-opus-4-1"}, claude5...), Efforts: []string{"medium"}, DefaultModel: "claude-sonnet-5-5"},
			want:       Resolved{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "medium"},
		},
		{
			name:       "a request list's earlier element wins over the default",
			constraint: []side{{harness: "claude-code"}},
			request:    side{harness: "claude-code", model: []string{"claude-opus-*", "claude-sonnet-*"}},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}, DefaultModel: "claude-sonnet-5-5"},
			want:       Resolved{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "medium"},
		},
		{
			name:       "a request list element matching nothing offered falls through to the next",
			constraint: []side{{harness: "claude-code"}},
			request:    side{harness: "claude-code", model: []string{"claude-mythos-*", "claude-{sonnet,haiku}-*"}},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "claude-code", Model: "claude-haiku-5-5", Effort: "medium"},
		},
		{
			name:       "within one list element, the default then the highest version decide",
			constraint: []side{{harness: "claude-code"}},
			request:    side{harness: "claude-code", model: []string{"claude-*-5-*"}},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}, DefaultModel: "claude-sonnet-5-5"},
			want:       Resolved{Harness: "claude-code", Model: "claude-sonnet-5-5", Effort: "medium"},
		},
		{
			name:       "effort list takes the first element offered",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex", effort: []string{"xhigh", "high", "low"}},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "medium", "high"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "high"},
		},
		{
			name:       "effort list within the constraint",
			constraint: []side{{harness: "codex", effort: []string{"low", "medium"}}},
			request:    side{harness: "codex", effort: []string{"high", "low"}},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "medium", "high"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "low"},
		},
		{
			name:       "effort * takes medium when offered",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex", effort: "*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "medium", "high"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "medium"},
		},
		{
			name:       "effort * takes medium only when the constraint allows it",
			constraint: []side{{harness: "codex", effort: "{high,xhigh}"}},
			request:    side{harness: "codex", effort: "*"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "medium", "xhigh", "high"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "xhigh"},
		},
		{
			name:       "the first constraint for the harness applies",
			constraint: []side{{harness: "claude-code", model: "x"}, {harness: "codex", model: "gpt-5-codex"}, {harness: "codex", model: "*"}},
			request:    side{harness: "codex"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex", "gpt-5.1-codex"}, Efforts: []string{"medium"}},
			want:       Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "medium"},
		},
		{
			name:       "the offer is for another harness",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex"},
			offer:      Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"medium"}},
			wantReason: ReasonHarness,
		},
		{
			name:       "no constraints",
			request:    side{harness: "codex"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}, Efforts: []string{"medium"}},
			wantReason: ReasonHarness,
		},
		{
			name:       "the offer has no models",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex"},
			offer:      Offer{Harness: "codex", Efforts: []string{"medium"}},
			wantReason: ReasonModel,
		},
		{
			name:       "the offer has no efforts",
			constraint: []side{{harness: "codex"}},
			request:    side{harness: "codex"},
			offer:      Offer{Harness: "codex", Models: []string{"gpt-5-codex"}},
			wantReason: ReasonEffort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cs []Constraint
			for _, c := range tt.constraint {
				cs = append(cs, Constraint{Harness: c.harness, Models: mustCompile(t, c.model), Effort: mustCompile(t, c.effort)})
			}
			req := Request{Harness: tt.request.harness, Model: mustCompile(t, tt.request.model), Effort: mustCompile(t, tt.request.effort), Prefer: tt.prefer}

			got, err := Resolve(cs, req, tt.offer)
			if tt.wantReason != "" {
				var nm *NoMatchError
				if !errors.As(err, &nm) || nm.Reason != tt.wantReason {
					t.Fatalf("Resolve = %+v, %v; want a NoMatchError(%s)", got, err, tt.wantReason)
				}
				if !errors.Is(err, ErrNoMatch) {
					t.Errorf("errors.Is(%v, ErrNoMatch) = false", err)
				}
				if nm.Harness != req.Harness {
					t.Errorf("Harness = %q, want %q", nm.Harness, req.Harness)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveDoesNotModifyInputs(t *testing.T) {
	offer := Offer{Harness: "codex", Models: []string{"gpt-5-mini", "gpt-5-codex", "gpt-5.1-codex"}, Efforts: []string{"high", "low", "medium"}}
	req := Request{Harness: "codex", Model: mustCompile(t, []string{"gpt-5-codex", "*"}), Effort: mustCompile(t, []string{"low", "*"})}
	if _, err := Resolve([]Constraint{{Harness: "codex"}}, req, offer); err != nil {
		t.Fatal(err)
	}
	if offer.Models[0] != "gpt-5-mini" || offer.Efforts[0] != "high" {
		t.Errorf("offer was modified: %+v", offer)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign
	}{
		{"claude-sonnet-5-5", "claude-opus-5-5", 0},
		{"claude-sonnet-5-5", "claude-sonnet-5-1", 1},
		{"claude-sonnet-5-10", "claude-sonnet-5-9", 1},
		{"claude-sonnet-5-0", "claude-sonnet-4-5", 1},
		{"gpt-5.1-codex", "gpt-5-codex", 1},
		{"gpt-5.1-codex", "gpt-5.1", 0},
		{"gpt-5", "gpt-4o", 1}, // "4o" is not a numeric group
		{"gpt-4o", "gpt-4o-mini", 0},
		{"model-007", "model-7", 0},
		{"model-0", "model-00", 0},
		{"m-123456789012345678901234567890", "m-123456789012345678901234567889", 1},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet-20240620", 1},
		{"no-digits", "", 0},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if sign(got) != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
		}
		if sign(compareVersions(tt.b, tt.a)) != -tt.want {
			t.Errorf("compareVersions(%q, %q) is not antisymmetric", tt.b, tt.a)
		}
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

func TestSubsumes(t *testing.T) {
	known := []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5"}
	tests := []struct {
		name               string
		cHarness, rHarness string
		constraint, req    any
		wantOK             bool
		wantWitness        string
	}{
		{name: "overlap", cHarness: "claude-code", rHarness: "claude-code", constraint: "claude-{sonnet,opus}-5-*", req: "claude-*-5-*", wantOK: true, wantWitness: "claude-sonnet-5-5"},
		{name: "disjoint", cHarness: "claude-code", rHarness: "claude-code", constraint: "claude-{sonnet,haiku}-5-*", req: "claude-opus-5-*"},
		{name: "unknown model", cHarness: "claude-code", rHarness: "claude-code", constraint: "*", req: "claude-mythos-1"},
		{name: "unset request", cHarness: "claude-code", rHarness: "claude-code", constraint: "claude-opus-*", wantOK: true, wantWitness: "claude-opus-5-5"},
		{name: "other harness", cHarness: "codex", rHarness: "claude-code", constraint: "*", req: "*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Constraint{Harness: tt.cHarness, Models: mustCompile(t, tt.constraint)}
			r := Request{Harness: tt.rHarness, Model: mustCompile(t, tt.req)}
			ok, w := Subsumes(c, r, known)
			if ok != tt.wantOK || w != tt.wantWitness {
				t.Errorf("Subsumes = %v, %q; want %v, %q", ok, w, tt.wantOK, tt.wantWitness)
			}
		})
	}
}

func TestFromAPI(t *testing.T) {
	cs, err := ConstraintsFrom([]v1alpha1.HarnessConstraint{
		{Name: "codex", Models: v1alpha1.Scalar(`re:^gpt-5(\.\d+)?-codex$`), ReasoningEffort: v1alpha1.List("low", "medium")},
		{Name: "claude-code"}, // unset fields match anything
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := RequestFrom(v1alpha1.HarnessRequest{Name: "codex", Config: &v1alpha1.HarnessConfig{
		Model:           v1alpha1.Scalar("gpt-5*"),
		ReasoningEffort: v1alpha1.Scalar("*"),
		Prefer:          []string{"gpt-4"}, // not offered: falls through to the default
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Prefer) != 1 || req.Prefer[0] != "gpt-4" {
		t.Errorf("Prefer = %v", req.Prefer)
	}
	apiOffer := v1alpha1.HarnessOffer{
		Name: "codex", Version: "0.50.0", Auth: "subscription",
		Models: []string{"gpt-5-mini", "gpt-5-codex", "gpt-5.1-codex"}, DefaultModel: "gpt-5-codex", // not the highest version
		ReasoningEffort: []string{"low", "medium", "high"},
	}
	got, err := Resolve(cs, req, OfferFrom(apiOffer))
	if err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.HarnessResolved{Name: "codex", Version: "0.50.0", Auth: "subscription", Model: "gpt-5-codex", ReasoningEffort: "medium"}
	if r := got.HarnessResolved(apiOffer); r != want {
		t.Errorf("HarnessResolved = %+v, want %+v", r, want)
	}

	claude, err := RequestFrom(v1alpha1.HarnessRequest{Name: "claude-code"}) // no config
	if err != nil {
		t.Fatal(err)
	}
	got, err = Resolve(cs, claude, Offer{Harness: "claude-code", Models: claude5, Efforts: []string{"high"}})
	if err != nil || got.Model != "claude-haiku-5-5" || got.Effort != "high" {
		t.Errorf("Resolve without config = %+v, %v", got, err)
	}
}

func TestFromAPIErrors(t *testing.T) {
	var pe *PatternError
	if _, err := ConstraintsFrom([]v1alpha1.HarnessConstraint{{Name: "codex"}, {Name: "x", ReasoningEffort: v1alpha1.Scalar("re:(")}}); !errors.As(err, &pe) {
		t.Errorf("ConstraintsFrom: want a PatternError, got %v", err)
	}
	if _, err := ConstraintFrom(v1alpha1.HarnessConstraint{Name: "x", Models: v1alpha1.List()}); !errors.As(err, &pe) {
		t.Errorf("ConstraintFrom: want a PatternError, got %v", err)
	}
	if _, err := RequestFrom(v1alpha1.HarnessRequest{Name: "x", Config: &v1alpha1.HarnessConfig{Model: v1alpha1.Scalar("{")}}); !errors.As(err, &pe) {
		t.Errorf("RequestFrom model: want a PatternError, got %v", err)
	}
	if _, err := RequestFrom(v1alpha1.HarnessRequest{Name: "x", Config: &v1alpha1.HarnessConfig{ReasoningEffort: v1alpha1.Scalar("")}}); !errors.As(err, &pe) {
		t.Errorf("RequestFrom effort: want a PatternError, got %v", err)
	}
}
