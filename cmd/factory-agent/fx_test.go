package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
)

func TestFxGitCredential(t *testing.T) {
	s, err := gitcred.Listen(t.TempDir(), func() []gitcred.Entry {
		return []gitcred.Entry{{Protocol: "https", Host: "github.com", Repo: "acme/docs", Username: "x-access-token", Password: "tok"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	env := func(k string) string {
		if k == gitcred.EnvSocket {
			return s.Path()
		}
		return ""
	}
	var out, errOut bytes.Buffer
	code := runFx(context.Background(), []string{"git-credential", "get"}, strings.NewReader("protocol=https\nhost=github.com\npath=acme/docs.git\n\n"), &out, &errOut, env)
	if code != 0 || out.String() != "username=x-access-token\npassword=tok\n" {
		t.Errorf("get = %d %q %q", code, out.String(), errOut.String())
	}
	for _, args := range [][]string{nil, {"git-credential"}, {"pr", "open"}} {
		if code := runFx(context.Background(), args, strings.NewReader(""), &out, &errOut, env); code != 2 {
			t.Errorf("runFx %v = %d, want 2", args, code)
		}
	}
	gone := func(string) string { return filepath.Join(t.TempDir(), "nope") }
	if code := runFx(context.Background(), []string{"git-credential", "get"}, strings.NewReader("protocol=https\n"), &out, &errOut, gone); code != 5 {
		t.Errorf("a missing socket = %d, want 5", code)
	}
}

func TestNewRunnerBuildsThePipeline(t *testing.T) {
	prev := harnessAdapters["codex"]
	harnessAdapters["codex"] = func(config.Harness) harness.Adapter { return &fake.Adapter{HarnessName: "codex"} }
	t.Cleanup(func() { harnessAdapters["codex"] = prev })
	home := t.TempDir()
	cfg := &config.Config{WorkRoot: filepath.Join(home, "work"), Harnesses: map[string]config.Harness{"codex": {Models: []string{"m"}}}}
	r, err := newRunner(cfg, runnerOptions{Home: func() (string, error) { return home, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*pipeline.Pipeline); !ok {
		t.Errorf("newRunner = %T, want the pipeline", r)
	}
	cfg.Harnesses["claude-code"] = config.Harness{Models: []string{"m"}}
	if _, err := newRunner(cfg, runnerOptions{}); err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("a harness without an adapter: %v", err)
	}
}
