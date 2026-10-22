// Package codex is the Codex CLI harness adapter (design/workstreams/agent/LLD/
// harness-adapters.md §Codex).
//
// Probe reads codex's version and how the owner's dedicated CODEX_HOME is signed in
// (subscription or API key) without reading the login itself. Prepare lays out a scratch
// CODEX_HOME in the workspace: a symlink to the owner's auth file (never a copy; the token is
// never read here), a generated config.toml (approval policy, sandbox tuning, the Effector's
// MCP server), AGENTS.md with the headless preamble, and the task's skills flattened from the
// pipeline's plugin layout. Run starts `codex exec --json` with the resolved model and
// effort in the workspace-write sandbox, from the checkout when it is writable and from out/
// when it isn't, streams its JSONL into the EventSink, stops on
// a rate or plan limit, and kills the whole process group when its context ends. Repair
// resumes the same session with the validation problems.
//
// Under harness.IsolationVM (`factory-agent run` in a VM workspace, workspace-run.md
// §Harnesses under IsolationVM) the VM is the boundary: there is no Probe and no OS sandbox
// (`--sandbox danger-full-access`, no [sandbox_workspace_write]), codex runs from the
// pipeline's WorkDir, and the scratch CODEX_HOME holds a placeholder auth.json
// (placeholderAuth): the account's identifiers in an unsigned id_token, harness.Placeholder
// as the tokens, and a fresh last_refresh, so codex never refreshes. The gateway outside the
// VM replaces the bearer and the account header with the owner's, and refreshes the real
// token itself.
//
// Every codex flag, config key and event name the adapter relies on is in flags.go, each
// marked UNVERIFIED until `make smoke-codex` runs on a maintainer machine.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/clirun"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Name is the harness's name.
const Name = config.HarnessCodex

// Auth modes in a HarnessOffer.
const (
	AuthSubscription = "subscription"
	AuthAPIKey       = "apiKey"
)

// DefaultThrottle is how long a rate-limited agent waits when the error gives no reset
// (harness-adapters.md: else +15 min).
const DefaultThrottle = clirun.DefaultThrottle

// DefaultKillGrace is how long codex gets to exit after SIGTERM before its process group is
// killed.
const DefaultKillGrace = 5 * time.Second

// Options configure an Adapter. The zero value (plus Config) is production.
type Options struct {
	// Config is the local config of the codex harness: Home is the owner's dedicated
	// CODEX_HOME.
	Config config.Harness
	// Binary is the codex executable. Default: "codex" on PATH.
	Binary string
	// Commands runs Probe's commands. Default: clirun.ExecCommands.
	Commands clirun.Commands
	// Environ is the environment Probe's commands start from (only PATH, HOME, LANG and
	// TERM are kept). Default: os.Environ.
	Environ func() []string
	// Clock stamps events and rate-limit resets. Default: clock.Real.
	Clock clock.Clock
	// KillGrace is the SIGTERM-to-SIGKILL delay on cancel. Default: DefaultKillGrace.
	KillGrace time.Duration
	// Throttle is the wait on a rate limit without a reset. Default: DefaultThrottle.
	Throttle time.Duration
	// Isolation is what bounds the run's commands. IsolationSandbox (the zero value) is the
	// owner's host: codex's workspace-write sandbox and the owner's login, as above.
	// IsolationVM is a VM workspace (workspace-run.md §Harnesses under IsolationVM): no
	// Probe, no OS sandbox, and a placeholder auth.json built from Account; Config's Home and
	// Environ aren't used.
	Isolation harness.Isolation
	// Account is the ChatGPT account codex runs as under IsolationVM: the run bundle's
	// codex/account.json, identifiers only (P1 §5). Its id_token claims pick codex's
	// workspace, which fails without the real ones (Phase 0, Q5). Required under
	// IsolationVM; unused on the host.
	Account *runbundle.CodexAccount
}

// Adapter is the codex harness adapter. It keeps no per-run state in memory (the session to
// resume is in the workspace), so one Adapter serves concurrent runs.
type Adapter struct {
	o Options
}

var _ harness.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = "codex"
	}
	if o.Commands == nil {
		o.Commands = clirun.ExecCommands{}
	}
	if o.Environ == nil {
		o.Environ = os.Environ
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
	return &Adapter{o: o}
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return Name }

// Capabilities implements harness.Adapter: the LLD's effort mapping (max → high, xhigh for
// the models that take it) and codex's features.
func (a *Adapter) Capabilities() v1alpha1.AdapterCapabilities {
	return v1alpha1.AdapterCapabilities{
		Harness:       Name,
		EffortMapping: maps.Clone(effortMapping),
		Features:      []string{"structured-output", "mcp", "skills-dir"},
	}
}

// Probe implements harness.Adapter: codex's version (at least MinVersion) and the login of
// cfg.Home. The login is told apart by `codex login status`'s wording and by the auth file's
// presence (os.Stat); neither the file nor the command's output is logged or kept. Models,
// default and efforts are the owner's config (codex doesn't list what a plan allows).
func (a *Adapter) Probe(ctx context.Context, cfg config.Harness) (v1alpha1.HarnessOffer, error) {
	if vm, err := a.vm(); err != nil || vm {
		if err == nil {
			err = errors.New("codex: Probe doesn't run in a VM workspace")
		}
		return v1alpha1.HarnessOffer{}, err
	}
	if cfg.Home == "" || !filepath.IsAbs(cfg.Home) {
		return v1alpha1.HarnessOffer{}, fmt.Errorf("codex: home %q must be an absolute directory", cfg.Home)
	}
	env := probeEnv(a.o.Environ(), cfg.Home)
	out, err := a.o.Commands.Output(ctx, env, a.o.Binary, versionArgs...)
	if err != nil {
		return v1alpha1.HarnessOffer{}, fmt.Errorf("codex %s: %w", strings.Join(versionArgs, " "), err)
	}
	version, ok := clirun.ParseVersion(out)
	if !ok {
		return v1alpha1.HarnessOffer{}, errors.New("codex: no version in `codex --version`'s output")
	}
	if clirun.CompareVersions(version, MinVersion) < 0 {
		return v1alpha1.HarnessOffer{}, fmt.Errorf("codex %s is older than %s, the oldest this agent supports: upgrade it", version, MinVersion)
	}
	auth, err := a.loginMode(ctx, env, cfg.Home)
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
	return false, fmt.Errorf("codex: unknown isolation %s", a.o.Isolation)
}

// loginMode is subscription or apiKey. The dedicated home must hold the auth file (it is
// what Prepare links to): a keyring-only login can't be shared with the scratch home.
func (a *Adapter) loginMode(ctx context.Context, env []string, home string) (string, error) {
	notSignedIn := fmt.Errorf("codex is not signed in in %s: run `CODEX_HOME=%s %s` once", home, home, loginCommand)
	fi, err := os.Stat(filepath.Join(home, authFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", notSignedIn
	case err != nil:
		return "", fmt.Errorf("codex login: %w", err)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("codex login: %s is not a regular file", filepath.Join(home, authFile))
	}
	out, err := a.o.Commands.Output(ctx, env, a.o.Binary, loginStatusArgs...)
	if err != nil {
		// The output may describe the credential: it isn't repeated.
		return "", notSignedIn
	}
	switch s := string(out); {
	case strings.Contains(s, loginSubscriptionMarker):
		return AuthSubscription, nil
	case strings.Contains(s, loginAPIKeyMarker):
		return AuthAPIKey, nil
	}
	return "", errors.New("codex login: `codex login status` names neither a ChatGPT login nor an API key")
}

// probeEnv is Probe's environment: PATH, HOME, LANG and TERM from the owner's, and
// CODEX_HOME. Nothing else (no OPENAI_API_KEY) can change what the login status says.
func probeEnv(environ []string, home string) []string {
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains([]string{"PATH", "HOME", "LANG", "TERM"}, k) {
			env = append(env, kv)
		}
	}
	return append(env, "CODEX_HOME="+home)
}
