package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// LabelGIFTooling is the label for recording GIFs (local-stack.md §Labels and probes).
const LabelGIFTooling = "gif-tooling"

// DefaultMinStackMemory is the memory Docker must report for has-local-stack: 8 GB (decimal,
// so a Docker Desktop VM given 8 GiB passes after the kernel's share).
const DefaultMinStackMemory = 8_000_000_000

// Labels are the label checkers local-stack.md defines, running programs through cmds.
func Labels(cmds Commands) map[string]LabelChecker {
	return map[string]LabelChecker{
		config.LabelLocalStack: LocalStack{Commands: cmds},
		LabelGIFTooling:        GIFTooling{Commands: cmds},
	}
}

// LocalStack checks has-local-stack: localDev is a local-dev checkout (Makefile, compose/,
// and the .env its Makefile includes, which is only looked at, never read), `docker info`
// answers (the daemon runs) with at least MinMemory, and `docker compose version` is v2 or
// later.
type LocalStack struct {
	Commands Commands
	// MinMemory is the memory Docker must report (MemTotal); default DefaultMinStackMemory.
	MinMemory int64
	// Stat is os.Stat by default.
	Stat func(string) (fs.FileInfo, error)
}

// Check implements LabelChecker.
func (l LocalStack) Check(ctx context.Context, cfg *config.Config) error {
	cmds, stat := l.Commands, l.Stat
	if cmds == nil {
		cmds = ExecCommands{}
	}
	if stat == nil {
		stat = os.Stat
	}
	minMem := l.MinMemory
	if minMem <= 0 {
		minMem = DefaultMinStackMemory
	}
	var problems []string
	if cfg.LocalDev == "" {
		problems = append(problems, "localDev is not set")
	} else {
		for _, f := range []struct {
			name string
			dir  bool
		}{{"Makefile", false}, {"compose", true}, {".env", false}} {
			st, err := stat(filepath.Join(cfg.LocalDev, f.name))
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("localDev %s has no %s", cfg.LocalDev, f.name))
			case f.dir != st.IsDir():
				problems = append(problems, fmt.Sprintf("localDev %s: %s has the wrong type", cfg.LocalDev, f.name))
			}
		}
	}
	if _, err := cmds.LookPath("docker"); err != nil {
		return errors.New(strings.Join(append(problems, "docker not found on PATH"), "; "))
	}
	out, err := cmds.Output(ctx, "docker", "info", "--format", "{{json .MemTotal}}")
	switch mem, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); {
	case err != nil:
		problems = append(problems, fmt.Sprintf("docker info failed (is Docker running?): %v", err))
	case perr != nil:
		problems = append(problems, fmt.Sprintf("docker info: memory %q unreadable", strings.TrimSpace(string(out))))
	case mem < minMem:
		problems = append(problems, fmt.Sprintf("Docker has %.1f GB of memory, the stack needs %.1f GB", float64(mem)/1e9, float64(minMem)/1e9))
	}
	out, err = cmds.Output(ctx, "docker", "compose", "version", "--short")
	if err != nil {
		problems = append(problems, fmt.Sprintf("docker compose version failed: %v", err))
	} else if v := strings.TrimSpace(string(out)); composeMajor(v) < 2 {
		problems = append(problems, fmt.Sprintf("docker compose %q: v2 or later needed", v))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// composeMajor is the major version of "2.29.1" or "v2.29.1"; 0 when unreadable.
func composeMajor(v string) int {
	major, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// GIFTooling checks gif-tooling: Playwright through npx (never installed on the fly: npx
// --no) with its Chromium installed (every install location `playwright install --dry-run
// chromium` lists exists), gifski, vhs and ffmpeg.
type GIFTooling struct {
	Commands Commands
	// Stat is os.Stat by default.
	Stat func(string) (fs.FileInfo, error)
}

// Check implements LabelChecker.
func (g GIFTooling) Check(ctx context.Context, _ *config.Config) error {
	cmds, stat := g.Commands, g.Stat
	if cmds == nil {
		cmds = ExecCommands{}
	}
	if stat == nil {
		stat = os.Stat
	}
	var problems []string
	run := func(name string, args ...string) ([]byte, bool) {
		if _, err := cmds.LookPath(name); err != nil {
			problems = append(problems, name+" not found on PATH")
			return nil, false
		}
		out, err := cmds.Output(ctx, name, args...)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s failed: %v", name, strings.Join(args, " "), err))
			return nil, false
		}
		return out, true
	}
	if _, ok := run("npx", "--no", "playwright", "--version"); ok {
		if out, ok := run("npx", "--no", "playwright", "install", "--dry-run", "chromium"); ok {
			locs := installLocations(string(out))
			if len(locs) == 0 {
				problems = append(problems, "playwright install --dry-run lists no browser")
			}
			for _, l := range locs {
				if _, err := stat(l); err != nil {
					problems = append(problems, "playwright browser missing: "+l+" (npx playwright install chromium)")
				}
			}
		}
	}
	run("gifski", "--version")
	run("vhs", "--version")
	run("ffmpeg", "-version")
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// installLocations reads the "Install location:" lines of `playwright install --dry-run`.
func installLocations(out string) []string {
	var locs []string
	for _, line := range strings.Split(out, "\n") {
		if _, loc, ok := strings.Cut(line, "Install location:"); ok {
			if loc = strings.TrimSpace(loc); loc != "" {
				locs = append(locs, loc)
			}
		}
	}
	return locs
}
