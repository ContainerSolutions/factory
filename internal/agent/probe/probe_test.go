package probe

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func testConfig() *config.Config {
	return &config.Config{
		Labels: []string{"macos", "fast-disk"},
		Harnesses: map[string]config.Harness{
			"codex":       {Home: "/c", Models: []string{"gpt-5-codex"}, Efforts: []string{"low", "high"}, Default: "gpt-5-codex"},
			"claude-code": {ConfigDir: "/cc", Models: []string{"claude-sonnet-5-5"}},
		},
	}
}

type failProber struct{}

func (failProber) Probe(context.Context, config.Harness) (v1alpha1.HarnessOffer, error) {
	return v1alpha1.HarnessOffer{}, errors.New("not signed in")
}

func TestRun(t *testing.T) {
	cfg := testConfig()
	rep := Run(context.Background(), cfg, Options{
		Harnesses: map[string]Prober{"claude-code": failProber{}},
	})
	want := v1alpha1.AgentCapabilities{
		Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, DefaultModel: "gpt-5-codex", ReasoningEffort: []string{"low", "high"},
		}},
		Labels: []string{"macos", "fast-disk"},
	}
	if !reflect.DeepEqual(rep.Capabilities, want) {
		t.Errorf("capabilities\n got %+v\nwant %+v", rep.Capabilities, want)
	}
	wantDropped := []Dropped{
		{Kind: "harness", Name: "claude-code", Reason: "not signed in"},
	}
	if !reflect.DeepEqual(rep.Dropped, wantDropped) {
		t.Errorf("dropped %+v", rep.Dropped)
	}
	if ok, why := rep.Ready(); !ok {
		t.Errorf("not ready: %v", why)
	}
	// The probe never changes the config's slices.
	rep.Capabilities.Harnesses[0].Models[0] = "changed"
	if cfg.Harnesses["codex"].Models[0] != "gpt-5-codex" {
		t.Error("the offer shares the config's models")
	}
}

func TestReadyWithoutHarness(t *testing.T) {
	rep := Run(context.Background(), &config.Config{Harnesses: map[string]config.Harness{"codex": {}}},
		Options{Harnesses: map[string]Prober{"codex": failProber{}}})
	if ok, why := rep.Ready(); ok || len(why) != 1 || !strings.Contains(why[0], "no harness") {
		t.Errorf("Ready = %v %v", ok, why)
	}
}

type fakeCommands struct{ missing map[string]bool }

func (f *fakeCommands) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", exec.ErrNotFound
	}
	return "/usr/bin/" + name, nil
}

func TestBaseTooling(t *testing.T) {
	tests := []struct {
		name    string
		cmds    *fakeCommands
		wantBad map[string]string // tool → problem substring
	}{
		{"all present", &fakeCommands{}, nil},
		{"git missing", &fakeCommands{missing: map[string]bool{"git": true}}, map[string]string{"git": "not found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := BaseTooling{Commands: tt.cmds}.Check(context.Background())
			if len(tools) != len(BaseTools()) {
				t.Fatalf("tools %+v", tools)
			}
			for _, tool := range tools {
				want, bad := tt.wantBad[tool.Name]
				if tool.OK == bad || (bad && !strings.Contains(tool.Problem, want)) {
					t.Errorf("%s: %+v, want bad=%v %q", tool.Name, tool, bad, want)
				}
			}
			rep := Report{Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}}, Tooling: tools}
			if ok, _ := rep.Ready(); ok != (len(tt.wantBad) == 0) {
				t.Errorf("Ready = %v", ok)
			}
		})
	}
}
