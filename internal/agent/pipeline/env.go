package pipeline

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Harness environment variables (run-pipeline.md §Steps 7, the ccf-skills HEADLESS.md
// contract).
const (
	EnvHeadless = "FACTORY_HEADLESS"
	EnvHarness  = "FACTORY_HARNESS"
	EnvTaskID   = "FACTORY_TASK_ID"
	EnvAPI      = "FACTORY_API"
	EnvToken    = "FACTORY_TOKEN"
	EnvOut      = "FACTORY_OUT"
	EnvResult   = "FACTORY_RESULT"
	EnvGHToken  = "GH_TOKEN"
)

// EnvRuntime names the runtime a run's processes are in: unset on the host, RuntimeWorkspace
// in a VM workspace (workspace-run.md §System setup), where vmrun sets it through Options.Env.
// The Effector reads it.
const (
	EnvRuntime       = "FACTORY_RUNTIME"
	RuntimeWorkspace = "workspace"
)

// ownerAllowed are the only variables taken from the owner's environment: PATH, LANG, TERM
// and Docker's (DOCKER_HOST, DOCKER_CONTEXT, …). Everything else (credentials, the owner's
// git and gh config, proxies) stays out.
var ownerAllowed = []string{"PATH", "LANG", "TERM"}

const ownerAllowedPrefix = "DOCKER_"

// ownerEnv is the owner's environment reduced to the allow-list, as a map (the last value
// of a repeated key wins).
func ownerEnv(environ []string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		for _, a := range ownerAllowed {
			if k == a {
				out[k] = v
			}
		}
		if strings.HasPrefix(k, ownerAllowedPrefix) {
			out[k] = v
		}
	}
	return out
}

// reservedEnv are the variables the pipeline sets itself (commonEnv, and buildEnv), which
// Options.Env may not set; so are the GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> entries and
// every FACTORY_STACK_* name the stack's Up returns (reservedEnvPrefixes). A collision is New's
// error, not a silently lost value. GH_TOKEN isn't one: Options.Env may set its placeholder,
// which a job's real token replaces.
var reservedEnv = []string{
	"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "PATH",
	"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT", "GIT_ALLOW_PROTOCOL", "GIT_CONFIG_COUNT",
	"GCM_INTERACTIVE", gitcred.EnvSocket,
	EnvHeadless, EnvHarness, EnvTaskID, EnvAPI, EnvToken, EnvOut, EnvResult, EnvEffector,
}

// reservedEnvPrefixes are the reserved name prefixes: the helper's process-scoped git config,
// and the stack's variables (stack.State.Env: FACTORY_STACK_URL, _API, _PROJECT, _VERSIONS,
// _DIR), which buildEnv copies over the environment.
var reservedEnvPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "FACTORY_STACK_"}

// Options.Env may not set a variable that can change what git or the shell runs, or which
// credential helper answers: every GIT_* name but the CA ones the gateway needs
// (GIT_CONFIG_PARAMETERS, GIT_ASKPASS, GIT_SSH_COMMAND, GIT_PROXY_COMMAND, GIT_DIR,
// GIT_EXEC_PATH, …), SSH_ASKPASS, the dynamic loader's LD_* and DYLD_*, and bash's BASH_ENV
// and sh's ENV. Gateway mode's "no helper at all" and helper mode's "only the in-memory
// helper" rest on it.
var (
	controlEnvPrefixes = []string{"GIT_", "LD_", "DYLD_"}
	controlEnv         = []string{"SSH_ASKPASS", "BASH_ENV", "ENV"}
	controlEnvAllowed  = []string{"GIT_SSL_CAINFO", "GIT_SSL_CAPATH"}
)

// isControlEnv reports whether k is one of git's, the loader's or the shell's control
// variables, which Options.Env may not set.
func isControlEnv(k string) bool {
	if slices.Contains(controlEnvAllowed, k) {
		return false
	}
	if slices.Contains(controlEnv, k) {
		return true
	}
	return slices.ContainsFunc(controlEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) })
}

// checkEnv refuses an Options.Env that sets a reserved variable or a control variable
// (isControlEnv), or has a name that isn't one (empty, or holding "=" or NUL), naming the
// variable but never its value.
func checkEnv(env map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(env)) {
		switch {
		case k == "" || strings.ContainsAny(k, "=\x00"):
			return fmt.Errorf("%q is not a variable name", k)
		case slices.Contains(reservedEnv, k),
			slices.ContainsFunc(reservedEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) }):
			return fmt.Errorf("%s is set by the pipeline", k)
		case isControlEnv(k):
			return fmt.Errorf("%s controls git, the loader or the shell: Env may not set it", k)
		case strings.ContainsRune(env[k], 0):
			return fmt.Errorf("%s: the value holds a NUL byte", k)
		}
	}
	return nil
}

// commonEnv is what both the pipeline's own git and the harness get: the allow-listed owner
// variables, then Options.Env, the scratch HOME and TMPDIR, and git set up never to prompt
// and to ignore the system and the owner's config. With GitAuthHelper git uses only the
// in-memory credential helper; with GitAuthGateway it has no helper at all (no socket, no
// helper config) and reaches the git host through Options.Env's proxy.
func (r *run) commonEnv() map[string]string {
	env := ownerEnv(r.p.o.Environ())
	for k, v := range r.p.o.Env {
		env[k] = v
	}
	env["HOME"] = r.ws.Home
	env["XDG_CONFIG_HOME"] = filepath.Join(r.ws.Home, ".config")
	env["XDG_CACHE_HOME"] = filepath.Join(r.ws.Home, ".cache")
	env["TMPDIR"] = r.ws.Tmp
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_CONFIG_GLOBAL"] = filepath.Join(r.ws.Home, ".gitconfig")
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GCM_INTERACTIVE"] = "never"
	env["GIT_ALLOW_PROTOCOL"] = r.protocols
	if r.p.o.GitAuth == GitAuthGateway {
		return env
	}
	if r.creds != nil {
		env[gitcred.EnvSocket] = r.creds.Path()
	}
	for _, kv := range gitcred.ConfigEnv(gitcred.HelperConfig(r.p.o.HelperCommand)) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

// gitEnv is the environment of the pipeline's own git commands.
func (r *run) gitEnv() []string { return envList(r.commonEnv()) }

// buildEnv is the harness's environment (run-pipeline.md §Steps 7).
func (r *run) buildEnv(context.Context) error {
	env := r.commonEnv()
	dirs := append([]string{r.ws.Bin}, r.pluginBins()...)
	if p := env["PATH"]; p != "" {
		dirs = append(dirs, p)
	}
	env["PATH"] = strings.Join(dirs, string(filepath.ListSeparator))
	env[EnvHeadless] = "1"
	env[EnvHarness] = r.adapter.Name()
	env[EnvTaskID] = r.id
	env[EnvAPI] = r.job.Dispatcher
	if r.job.AgentToken != nil {
		env[EnvToken] = r.job.AgentToken()
	}
	env[EnvOut] = r.ws.Out
	env[EnvResult] = r.ws.Result
	// gh, ccfr and lib/lisa read GitHub with the task's scoped token; writes go through the
	// Effector, and the token's scope bounds any bypass.
	if r.job.Git != nil {
		if tok := r.job.Git.Get().Token; tok != "" {
			env[EnvGHToken] = tok
		}
	}
	for k, v := range r.stackEnv {
		env[k] = v
	}
	for k, v := range r.envVars {
		env[k] = v
	}
	cfg, err := r.writeEffectorConfig()
	if err != nil {
		return err
	}
	env[EnvEffector] = cfg
	r.ws.Env = envList(env)
	r.ws.Hosts = r.hosts()
	return nil
}

// pluginBins are the bin/ directories of the materialised plugins that have one, in plugin
// name order: on PATH after the workspace's bin/, so `factory` stays the pipeline's.
func (r *run) pluginBins() []string {
	var names []string
	for _, s := range r.ws.SkillList {
		if !slices.Contains(names, s.Plugin) {
			names = append(names, s.Plugin)
		}
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		d := filepath.Join(r.ws.Skills, n, "bin")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// hosts are the git host, the dispatcher's host, when the stack is up the hosts of its UI
// and API (FACTORY_STACK_URL, FACTORY_STACK_API), and then the Task's spec.hosts, each
// once and in that order: a sandboxed harness reaches only these.
func (r *run) hosts() []string {
	var out []string
	add := func(h string) {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if r.base != nil {
		add(r.base.Hostname())
	}
	if u, err := url.Parse(r.job.Dispatcher); err == nil {
		add(u.Hostname())
	}
	for _, k := range stackURLVars {
		if v := r.stackEnv[k]; v != "" {
			if u, err := url.Parse(v); err == nil {
				add(u.Hostname())
			}
		}
	}
	for _, h := range r.taskHosts() {
		add(h)
	}
	return out
}

// taskHosts are the Task's spec.hosts the agent accepts. The dispatcher validated them when
// it compiled the Task, but the wire is not trusted: past MaxWorkspaceHosts entries, and
// any entry ValidateHost refuses, are skipped with a warning, never failing the run.
func (r *run) taskHosts() []string {
	hosts := r.task.Spec.Hosts
	if n := len(hosts); n > v1alpha1.MaxWorkspaceHosts {
		r.log.Warn("the task lists too many hosts: ignoring the rest",
			"hosts", n, "max", v1alpha1.MaxWorkspaceHosts)
		hosts = hosts[:v1alpha1.MaxWorkspaceHosts]
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if err := v1alpha1.ValidateHost(h); err != nil {
			r.log.Warn("skipping an invalid task host", "err", err)
			continue
		}
		out = append(out, h)
	}
	return out
}

// stackURLVars are the stack's variables whose hosts the harness must reach (local-stack.md).
var stackURLVars = []string{"FACTORY_STACK_URL", "FACTORY_STACK_API"}

// envList is env as sorted KEY=value entries.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
