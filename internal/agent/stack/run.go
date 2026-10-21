package stack

import (
	"context"
	"errors"
	"time"
)

// runVars are the variables State.Env sets: what a run's environment holds while its stack
// is up.
var runVars = []string{EnvURL, EnvAPI, EnvProject, EnvVersions, EnvDir}

// RunEnv is a run's stack as the pipeline put it in the harness's environment (State.Env's
// variables, the empty ones left out), or nil when the run has none (FACTORY_STACK_URL or
// FACTORY_STACK_API unset). Inside a run, `factory-agent stack env` prints it: the run's
// stack, never another one the machine's lock may name.
func RunEnv(getenv func(string) string) map[string]string {
	if getenv(EnvURL) == "" || getenv(EnvAPI) == "" {
		return nil
	}
	out := map[string]string{}
	for _, k := range runVars {
		if v := getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// WaitURLs is Manager.Wait for a stack known only by where it answers (a run's, from
// RunEnv): o.UIURL and o.APIURL are required, o.StateDir, o.Runner and o.Output unused, and
// the other fields default as in New.
func WaitURLs(ctx context.Context, o Options, timeout time.Duration) error {
	if o.UIURL == "" || o.APIURL == "" {
		return errors.New("stack: waiting needs the stack's UI and API URLs")
	}
	return (&Manager{o: o.withDefaults()}).Wait(ctx, timeout)
}
