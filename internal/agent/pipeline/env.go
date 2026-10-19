package pipeline

import (
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
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

// commonEnv is what both the pipeline's own git and the harness get: the allow-listed owner
// variables, the scratch HOME and TMPDIR, and git set up to use only the in-memory
// credential helper, never prompt, and ignore the system config.
func (r *run) commonEnv() map[string]string {
	env := ownerEnv(r.p.o.Environ())
	env["HOME"] = r.ws.Home
	env["XDG_CONFIG_HOME"] = filepath.Join(r.ws.Home, ".config")
	env["XDG_CACHE_HOME"] = filepath.Join(r.ws.Home, ".cache")
	env["TMPDIR"] = r.ws.Tmp
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_CONFIG_GLOBAL"] = filepath.Join(r.ws.Home, ".gitconfig")
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GCM_INTERACTIVE"] = "never"
	env["GIT_ALLOW_PROTOCOL"] = r.protocols
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
	if p := env["PATH"]; p != "" {
		env["PATH"] = r.ws.Bin + string(filepath.ListSeparator) + p
	} else {
		env["PATH"] = r.ws.Bin
	}
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
	cfg, err := r.writeEffectorConfig()
	if err != nil {
		return err
	}
	env[EnvEffector] = cfg
	r.ws.Env = envList(env)
	r.ws.Hosts = r.hosts()
	return nil
}

// hosts are the git host and the dispatcher's host, each once.
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
