package probe

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// scriptCommands answers each command line ("name arg…") from out, fails those in fail, and
// has no program in missing. It never runs anything.
type scriptCommands struct {
	missing map[string]bool
	out     map[string]string
	fail    map[string]bool
	calls   []string
}

func (s *scriptCommands) LookPath(name string) (string, error) {
	if s.missing[name] {
		return "", exec.ErrNotFound
	}
	return "/usr/bin/" + name, nil
}

func (s *scriptCommands) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, line)
	if s.fail[line] {
		return nil, errors.New("exit status 1")
	}
	return []byte(s.out[line]), nil
}

const (
	dockerInfo     = "docker info --format {{json .MemTotal}}"
	composeVersion = "docker compose version --short"
	pwVersion      = "npx --no playwright --version"
	pwDryRun       = "npx --no playwright install --dry-run chromium"
)

// files is a fake file system for Stat.
func files(paths ...string) func(string) (fs.FileInfo, error) {
	m := fstest.MapFS{}
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			m[strings.TrimPrefix(strings.TrimSuffix(p, "/"), "/")] = &fstest.MapFile{Mode: fs.ModeDir}
		} else {
			m[strings.TrimPrefix(p, "/")] = &fstest.MapFile{}
		}
	}
	return func(name string) (fs.FileInfo, error) { return fs.Stat(m, strings.TrimPrefix(name, "/")) }
}

func TestLocalStack(t *testing.T) {
	good := func() *scriptCommands {
		return &scriptCommands{out: map[string]string{dockerInfo: "16624349184\n", composeVersion: "2.29.1\n"}}
	}
	checkout := files("/ld/Makefile", "/ld/compose/", "/ld/.env")
	tests := []struct {
		name     string
		cmds     *scriptCommands
		localDev string
		stat     func(string) (fs.FileInfo, error)
		want     []string // problem substrings; none: passes
	}{
		{name: "ready", cmds: good(), localDev: "/ld", stat: checkout},
		{name: "v-prefixed compose", cmds: &scriptCommands{out: map[string]string{dockerInfo: "8589934592", composeVersion: "v2.40.0"}}, localDev: "/ld", stat: checkout},
		{name: "no localDev", cmds: good(), want: []string{"localDev is not set"}},
		{name: "not a checkout", cmds: good(), localDev: "/ld", stat: files("/ld/compose"), want: []string{"no Makefile", "compose has the wrong type", "no .env"}},
		{name: "no docker", cmds: &scriptCommands{missing: map[string]bool{"docker": true}}, localDev: "/ld", stat: checkout, want: []string{"docker not found"}},
		{name: "daemon down", cmds: &scriptCommands{fail: map[string]bool{dockerInfo: true}, out: map[string]string{composeVersion: "2.29.1"}}, localDev: "/ld", stat: checkout, want: []string{"is Docker running"}},
		{name: "small VM", cmds: &scriptCommands{out: map[string]string{dockerInfo: "4294967296", composeVersion: "2.29.1"}}, localDev: "/ld", stat: checkout, want: []string{"4.3 GB of memory"}},
		{name: "compose v1", cmds: &scriptCommands{out: map[string]string{dockerInfo: "16624349184", composeVersion: "1.29.2"}}, localDev: "/ld", stat: checkout, want: []string{"v2 or later"}},
		{name: "no compose plugin", cmds: &scriptCommands{out: map[string]string{dockerInfo: "16624349184"}, fail: map[string]bool{composeVersion: true}}, localDev: "/ld", stat: checkout, want: []string{"docker compose version failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := LocalStack{Commands: tt.cmds, Stat: tt.stat}.Check(context.Background(), &config.Config{LocalDev: tt.localDev})
			checkProblems(t, err, tt.want)
		})
	}
}

func TestGIFTooling(t *testing.T) {
	const dryRun = "browser: chromium version 130.0.6723.31\n  Install location:    /cache/ms-playwright/chromium-1140\n  Download url:        https://example.invalid/chromium.zip\n\n" +
		"browser: chromium-headless-shell version 130.0.6723.31\n  Install location:    /cache/ms-playwright/chromium_headless_shell-1140\n"
	good := func() *scriptCommands {
		return &scriptCommands{out: map[string]string{pwVersion: "Version 1.48.0", pwDryRun: dryRun}}
	}
	installed := files("/cache/ms-playwright/chromium-1140/", "/cache/ms-playwright/chromium_headless_shell-1140/")
	tests := []struct {
		name string
		cmds *scriptCommands
		stat func(string) (fs.FileInfo, error)
		want []string
	}{
		{name: "ready", cmds: good(), stat: installed},
		{name: "browser not installed", cmds: good(), stat: files("/cache/ms-playwright/chromium-1140/"), want: []string{"browser missing: /cache/ms-playwright/chromium_headless_shell-1140"}},
		{name: "no playwright", cmds: &scriptCommands{fail: map[string]bool{pwVersion: true}}, stat: installed, want: []string{"npx --no playwright --version failed"}},
		{name: "dry run lists nothing", cmds: &scriptCommands{out: map[string]string{pwVersion: "Version 1.48.0"}}, stat: installed, want: []string{"lists no browser"}},
		{name: "missing tools", cmds: func() *scriptCommands {
			c := good()
			c.missing = map[string]bool{"vhs": true, "gifski": true}
			c.fail = map[string]bool{"ffmpeg -version": true}
			return c
		}(), stat: installed, want: []string{"gifski not found", "vhs not found", "ffmpeg -version failed"}},
		{name: "no npx", cmds: &scriptCommands{missing: map[string]bool{"npx": true}}, stat: installed, want: []string{"npx not found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := GIFTooling{Commands: tt.cmds, Stat: tt.stat}.Check(context.Background(), &config.Config{})
			checkProblems(t, err, tt.want)
			for _, c := range tt.cmds.calls {
				if strings.HasPrefix(c, "npx") && !strings.HasPrefix(c, "npx --no ") {
					t.Errorf("npx may install: %q", c)
				}
			}
		})
	}
}

func checkProblems(t *testing.T, err error, want []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("Check = %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("Check passed, want %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("Check = %q, lacks %q", err, w)
		}
	}
}

func TestLabels(t *testing.T) {
	l := Labels(&scriptCommands{})
	if _, ok := l[config.LabelLocalStack].(LocalStack); !ok {
		t.Errorf("has-local-stack checker %T", l[config.LabelLocalStack])
	}
	if _, ok := l[LabelGIFTooling].(GIFTooling); !ok {
		t.Errorf("gif-tooling checker %T", l[LabelGIFTooling])
	}
}
