package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
)

func TestProbe(t *testing.T) {
	t.Parallel()
	signedIn := ownerHome(t)
	empty := t.TempDir()
	keyring := t.TempDir()
	if err := os.Mkdir(filepath.Join(keyring, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := func(home string) config.Harness {
		return config.Harness{Home: home, Models: []string{"gpt-5-codex", "gpt-5"}, Efforts: []string{"low", "high"}, Default: "gpt-5-codex"}
	}
	tests := []struct {
		name     string
		plan     fakePlan
		cfg      config.Harness
		wantAuth string
		wantErr  string
	}{
		{"subscription", fakePlan{Version: "codex-cli 0.80.1", Login: "Logged in using ChatGPT"}, cfg(signedIn), AuthSubscription, ""},
		{"API key", fakePlan{Version: "codex-cli 1.2.0-alpha.3", Login: "Logged in using an API key - sk-proj-***ABCDE"}, cfg(signedIn), AuthAPIKey, ""},
		{"too old", fakePlan{Version: "codex-cli 0.46.0", Login: "Logged in using ChatGPT"}, cfg(signedIn), "", "older than " + MinVersion},
		{"no version", fakePlan{Version: "codex-cli dev", Login: "Logged in using ChatGPT"}, cfg(signedIn), "", "no version"},
		{"no auth file", fakePlan{Login: "Logged in using ChatGPT"}, cfg(empty), "", "not signed in"},
		{"auth file isn't a file", fakePlan{Login: "Logged in using ChatGPT"}, cfg(keyring), "", "not a regular file"},
		{"login status fails", fakePlan{Login: "Not logged in", LoginExit: 1}, cfg(signedIn), "", "not signed in"},
		{"unknown login", fakePlan{Login: "Logged in somehow"}, cfg(signedIn), "", "neither"},
		{"no home", fakePlan{}, config.Harness{Models: []string{"m"}}, "", "absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := newFakeCodex(t, tt.plan)
			a := New(Options{Binary: fc.Binary, Environ: func() []string {
				return []string{"PATH=" + os.Getenv("PATH"), "HOME=/Users/owner", "OPENAI_API_KEY=sk-should-not-pass"}
			}})
			off, err := a.Probe(context.Background(), tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Probe = %+v, %v; want an error with %q", off, err, tt.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), "sk-") {
					t.Errorf("the error repeats the login: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if off.Name != "codex" || off.Auth != tt.wantAuth || off.DefaultModel != "gpt-5-codex" ||
				!slices.Equal(off.Models, []string{"gpt-5-codex", "gpt-5"}) || !slices.Equal(off.ReasoningEffort, []string{"low", "high"}) {
				t.Errorf("offer %+v", off)
			}
			if !strings.HasPrefix(tt.plan.Version, "codex-cli "+off.Version) {
				t.Errorf("version %q from %q", off.Version, tt.plan.Version)
			}
		})
	}
}

func TestProbeDefaultsEfforts(t *testing.T) {
	t.Parallel()
	fc := newFakeCodex(t, fakePlan{Login: "Logged in using ChatGPT"})
	off, err := New(Options{Binary: fc.Binary}).Probe(context.Background(), config.Harness{Home: ownerHome(t), Models: []string{"gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(off.ReasoningEffort, config.Efforts()) {
		t.Errorf("efforts %v", off.ReasoningEffort)
	}
}

func TestProbeEnv(t *testing.T) {
	env := probeEnv([]string{"PATH=/bin", "HOME=/h", "OPENAI_API_KEY=sk-x", "CODEX_HOME=/elsewhere", "LANG=C"}, "/dedicated")
	if !slices.Equal(env, []string{"PATH=/bin", "HOME=/h", "LANG=C", "CODEX_HOME=/dedicated"}) {
		t.Errorf("probe env %q", env)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"0.72.0", "0.72.0", 0}, {"0.80.1", "0.72.0", 1}, {"0.9.0", "0.72.0", -1}, {"1.0.0", "0.99.99", 1}} {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%s, %s) = %d", tt.a, tt.b, got)
		}
	}
}
