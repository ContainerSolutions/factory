package pipeline

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/gitcred"
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

// EnvRuntime names the runtime a run's processes are in: RuntimeWorkspace, a VM workspace
// (workspace-run.md §System setup), where vmrun sets it through Options.Env. The Effector
// reads it and refuses any other value.
const (
	EnvRuntime       = "FACTORY_RUNTIME"
	RuntimeWorkspace = "workspace"
)

// ownerAllowed are the only variables taken from the process's environment: PATH (the VM
// harness's PATH comes from it, since Options.Env may not set PATH), LANG and TERM.
// Everything else (credentials, git and gh config, proxies, DOCKER_CONFIG) comes through
// Options.Env or stays out.
var ownerAllowed = []string{"PATH", "LANG", "TERM"}

// ownerEnv is the owner's environment reduced to the allow-list, as a map (the last value
// of a repeated key wins).
func ownerEnv(environ []string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && slices.Contains(ownerAllowed, k) {
			out[k] = v
		}
	}
	return out
}

// reservedEnv are the variables the pipeline sets itself (commonEnv, and buildEnv), which
// Options.Env may not set; so are the GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> entries
// (reservedEnvPrefixes). A collision is New's
// error, not a silently lost value. GH_TOKEN isn't one: Options.Env may set its placeholder,
// which a job's real token replaces.
var reservedEnv = []string{
	"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "PATH",
	"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT", "GIT_ALLOW_PROTOCOL", "GIT_CONFIG_COUNT",
	"GCM_INTERACTIVE", gitcred.EnvSocket,
	EnvHeadless, EnvHarness, EnvTaskID, EnvAPI, EnvToken, EnvOut, EnvResult, EnvEffector,
}

// reservedEnvPrefixes are the reserved name prefixes: the helper's process-scoped git config.
var reservedEnvPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

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
	env[EnvOut] = r.ws.Out
	env[EnvResult] = r.ws.Result
	for k, v := range r.envVars {
		env[k] = v
	}
	cfg, err := r.writeEffectorConfig()
	if err != nil {
		return err
	}
	env[EnvEffector] = cfg
	r.ws.Env = envList(env)
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

// envList is env as sorted KEY=value entries.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
