// Package claudecode is the Claude Code CLI harness adapter (design/workstreams/agent/LLD/
// harness-adapters.md §Claude Code).
//
// Probe reads claude's version and tells a subscription login from an API key without reading
// either: ANTHROPIC_API_KEY (or CLAUDE_CODE_OAUTH_TOKEN) set in the worker's environment, else
// the dedicated CLAUDE_CONFIG_DIR's login file (os.Stat only; on macOS the login is in the
// Keychain, never read). Prepare writes a settings file (no marketplace plugins, the
// reasoning effort, the OS sandbox for every Bash command with its writable directories,
// unreadable paths and network allow-list, and a PreToolUse hook as a second layer that keeps
// file writes inside the run's writable directories and refuses `git push` and gh writes;
// without the sandbox the adapter refuses to run) and an MCP config with the Effector,
// both in a scratch directory of the workspace's HOME; the skills are the pipeline's plugin
// directories, passed as they are. Run starts `claude -p --output-format stream-json` with
// the resolved model, the headless preamble appended to the system prompt and the Result
// schema, streams its events into the EventSink, stops on a rate or plan limit, and kills the
// whole process group when its context ends. Repair resumes the same session with the
// validation problems.
//
// Under harness.IsolationVM (`factory-agent run` in a VM workspace, workspace-run.md
// §Harnesses under IsolationVM) the VM is the boundary: there is no Probe and no OS sandbox,
// the settings keep only the plugins, the effort and the PreToolUse hook, CLAUDE_CONFIG_DIR is
// a scratch directory of the workspace's HOME, and the credential is harness.Placeholder,
// which the gateway outside the VM replaces with the real one.
//
// Every claude flag, file, settings key and event name the adapter relies on is in flags.go,
// each marked UNVERIFIED until `make smoke-claude` runs on a maintainer machine.
package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Name is the harness's name.
const Name = config.HarnessClaudeCode

// Auth modes in a HarnessOffer.
const (
	AuthSubscription = "subscription"
	AuthAPIKey       = "apiKey"
)

// DefaultKillGrace is how long claude gets to exit after SIGTERM before its process group is
// killed.
const DefaultKillGrace = 5 * time.Second

// DefaultThrottle is how long a rate-limited agent waits when the error gives no reset
// (harness-adapters.md: else +15 min).
const DefaultThrottle = clirun.DefaultThrottle

// Options configure an Adapter. The zero value (plus Config) is production.
type Options struct {
	// Config is the local config of the claude-code harness: ConfigDir is the owner's
	// dedicated CLAUDE_CONFIG_DIR.
	Config config.Harness
	// Binary is the claude executable. Default: "claude" on PATH.
	Binary string
	// Commands runs Probe's commands. Default: clirun.ExecCommands.
	Commands clirun.Commands
	// Environ is the worker's environment: Probe's commands keep only PATH, HOME, LANG and
	// TERM of it; the auth mode is read from it (ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN)
	// and a run gets that one variable from it. Default: os.Environ.
	Environ func() []string
	// GOOS decides where the subscription login is (the Keychain on darwin) and which OS
	// sandbox claude uses. Default: runtime.GOOS.
	GOOS string
	// LookPath finds the OS sandbox's tools. Default: exec.LookPath.
	LookPath func(string) (string, error)
	// Clock stamps events and rate-limit resets. Default: clock.Real.
	Clock clock.Clock
	// KillGrace is the SIGTERM-to-SIGKILL delay on cancel. Default: DefaultKillGrace.
	KillGrace time.Duration
	// Throttle is the wait on a rate limit without a reset. Default: DefaultThrottle.
	Throttle time.Duration
	// Isolation is what bounds the run's commands. IsolationSandbox (the zero value) is the
	// owner's host: claude's OS sandbox and the owner's login, as above. IsolationVM is a VM
	// workspace (workspace-run.md §Harnesses under IsolationVM): no Probe, no OS sandbox, a
	// scratch CLAUDE_CONFIG_DIR, and a placeholder credential the gateway replaces; Config's
	// ConfigDir, GOOS, LookPath and Environ aren't used.
	Isolation harness.Isolation
	// Getuid is the run's user id: under IsolationVM, a root run sets IS_SANDBOX. Default:
	// os.Getuid.
	Getuid func() int
}

// Adapter is the claude-code harness adapter. It keeps no per-run state in memory (the
// session to resume is in the workspace), so one Adapter serves concurrent runs.
type Adapter struct {
	o Options
}

var _ harness.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = "claude"
	}
	if o.Commands == nil {
		o.Commands = clirun.ExecCommands{}
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.KillGrace <= 0 {
		o.KillGrace = DefaultKillGrace
	}
	if o.Throttle <= 0 {
		o.Throttle = DefaultThrottle
	}
	if o.Getuid == nil {
		o.Getuid = os.Getuid
	}
	return &Adapter{o: o}
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return Name }

// Capabilities implements harness.Adapter: the effort mapping and claude's features.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{
		Harness:       Name,
		EffortMapping: maps.Clone(effortMapping),
		Features:      []string{"structured-output", "mcp", "plugin-dir", "pre-tool-use-hook"},
	}
}

// Probe implements harness.Adapter: claude's OS sandbox is supported here, claude's version
// is at least MinVersion, and the auth mode of cfg.ConfigDir. Models, default and efforts are
// the owner's config (claude doesn't list what a plan allows). No credential is read: the
// auth mode comes from variables being set and files existing. Without the sandbox the
// harness isn't offered, so the worker takes no claude-code task.
func (a *Adapter) Probe(ctx context.Context, cfg config.Harness) (v1alpha1.HarnessOffer, error) {
	if vm, err := a.vm(); err != nil || vm {
		if err == nil {
			err = errors.New("claude-code: Probe doesn't run in a VM workspace: the supervisor probes on the agent host")
		}
		return v1alpha1.HarnessOffer{}, err
	}
	if err := a.sandboxSupport(); err != nil {
		return v1alpha1.HarnessOffer{}, err
	}
	dir := cfg.ConfigDir
	if err := checkConfigDir(dir); err != nil {
		return v1alpha1.HarnessOffer{}, err
	}
	out, err := a.o.Commands.Output(ctx, probeEnv(a.o.Environ(), dir), a.o.Binary, versionArgs...)
	if err != nil {
		return v1alpha1.HarnessOffer{}, fmt.Errorf("claude %s: %w", strings.Join(versionArgs, " "), err)
	}
	version, ok := clirun.ParseVersion(out)
	if !ok {
		return v1alpha1.HarnessOffer{}, errors.New("claude: no version in `claude --version`'s output")
	}
	if clirun.CompareVersions(version, MinVersion) < 0 {
		return v1alpha1.HarnessOffer{}, fmt.Errorf("claude %s is older than %s, the oldest this agent supports: upgrade it", version, MinVersion)
	}
	auth, err := a.authMode(dir)
	if err != nil {
		return v1alpha1.HarnessOffer{}, err
	}
	efforts := slices.Clone(cfg.Efforts)
	if len(efforts) == 0 {
		efforts = config.Efforts()
	}
	efforts = slices.DeleteFunc(efforts, func(e string) bool { _, ok := effortMapping[e]; return !ok })
	return v1alpha1.HarnessOffer{
		Name: Name, Version: version, Auth: auth,
		Models: slices.Clone(cfg.Models), DefaultModel: cfg.Default, ReasoningEffort: efforts,
	}, nil
}

// vm reports whether the adapter runs under IsolationVM; an unknown Isolation is an error,
// never a run without its sandbox.
func (a *Adapter) vm() (bool, error) {
	switch a.o.Isolation {
	case harness.IsolationSandbox:
		return false, nil
	case harness.IsolationVM:
		return true, nil
	}
	return false, fmt.Errorf("claude-code: unknown isolation %s", a.o.Isolation)
}

// checkConfigDir: the dedicated config dir is an absolute, existing directory.
func checkConfigDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("claude-code: configDir %q must be an absolute directory", dir)
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("claude-code: configDir %s doesn't exist: "+loginHint, dir)
	case err != nil:
		return fmt.Errorf("claude-code: configDir: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("claude-code: configDir %s is not a directory", dir)
	}
	return nil
}

// authMode is apiKey when ANTHROPIC_API_KEY is set in the worker's environment, else
// subscription when a long-lived subscription token is set or the config dir holds a login
// (its credentials file; on macOS, where the login is in the Keychain, its state file). Only
// variable presence and os.Stat are used: no credential is read.
func (a *Adapter) authMode(dir string) (string, error) {
	env := a.o.Environ()
	if clirun.LookupEnv(env, envAPIKey) != "" {
		return AuthAPIKey, nil
	}
	if clirun.LookupEnv(env, envOAuthToken) != "" {
		return AuthSubscription, nil
	}
	if isFile(filepath.Join(dir, credentialsFile)) {
		return AuthSubscription, nil
	}
	if a.o.GOOS == "darwin" && isFile(filepath.Join(dir, stateFile)) {
		return AuthSubscription, nil
	}
	return "", fmt.Errorf("claude-code is not signed in in %[1]s: "+loginHint, dir)
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// probeEnv is Probe's environment: PATH, HOME, LANG and TERM from the worker's, and
// CLAUDE_CONFIG_DIR. No credential reaches `claude --version`.
func probeEnv(environ []string, dir string) []string {
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains([]string{"PATH", "HOME", "LANG", "TERM"}, k) {
			env = append(env, kv)
		}
	}
	return append(env, envConfigDir+"="+dir)
}
