// Package effector is the Effector (design/workstreams/agent/LLD/effector.md): every change a
// run makes on GitHub goes through it. It has one implementation, Effector.Do over the op
// table in ops.go, and two front-ends generated from that table, so they can't drift: the
// CLI (`factory <op> …`, which is `factory-agent fx <op> …` through the workspace's shim) and
// a stdio MCP server (`factory mcp`) with one tool per op.
//
// API-side ops are POST /tasks/{id}/effects with an idempotency key (keys.go); git-side ops
// (push-branch, stack.push, stack.rebase) run git (or `gh stack`) in the workspace through
// the run's in-memory credential helper, then record the op through the same endpoint. A
// local pre-check of the task's effects.allow and effects.labels gives fast, clear
// refusals; the dispatcher re-checks everything and is authoritative.
//
// In a VM workspace ($FACTORY_RUNTIME=workspace) the Effector holds no credential: there is
// no FACTORY_TOKEN and no credential helper, and the workspace's gateway (HTTP_PROXY,
// HTTPS_PROXY) adds the run token to its effects and the repo's credential to its pushes,
// refusing a push to a repo the task may not write. Stack ops always take the git fallback
// there.
package effector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/patterns"
)

// Exit codes (effector.md §Exit codes).
const (
	ExitOK         = 0
	ExitUsage      = 2
	ExitNotAllowed = 3
	ExitRejected   = 4
	ExitGit        = 5
)

// DefaultAttempts is how many times one call is sent when the dispatcher can't be reached
// (a network error), always with the same idempotency key.
const DefaultAttempts = 3

// requestTimeout bounds one POST: the provider adapter retries GitHub for up to a minute
// before it answers.
const requestTimeout = 3 * time.Minute

// StateFile is the idempotency state's name in $FACTORY_OUT.
const StateFile = ".effector-seq"

// Error is a failed op: its exit code, a message for people (and agents), and the
// dispatcher's problem when it answered with one.
type Error struct {
	Exit    int
	Msg     string
	Problem *Problem
	// Extra is more detail for the JSON error (validation problems, git output).
	Extra map[string]any
	Err   error
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error { return e.Err }

// Problem is the dispatcher's answer to a refused or failed effect.
type Problem struct {
	Status     int                    `json:"status"`
	Code       string                 `json:"code,omitempty"`
	Title      string                 `json:"title,omitempty"`
	Detail     string                 `json:"detail,omitempty"`
	Fields     []apiclient.FieldError `json:"fields,omitempty"`
	RetryAfter int                    `json:"retryAfterSeconds,omitempty"`
}

func usagef(format string, a ...any) *Error {
	return &Error{Exit: ExitUsage, Msg: fmt.Sprintf(format, a...)}
}

func notAllowedf(format string, a ...any) *Error {
	return &Error{Exit: ExitNotAllowed, Msg: fmt.Sprintf(format, a...)}
}

// exitCode is err's exit code: an *Error's, else ExitRejected.
func exitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Exit
	}
	return ExitRejected
}

// API is the part of the dispatcher client the Effector uses.
type API interface {
	CreateEffect(ctx context.Context, id apiclient.TaskID, body apiclient.CreateEffectJSONRequestBody, reqEditors ...apiclient.RequestEditorFn) (*http.Response, error)
}

// maxResponse bounds the dispatcher's answer.
const maxResponse = 1 << 20

// Options configure an Effector.
type Options struct {
	// API is the dispatcher client: authenticated as the agent on the host, or without a
	// credential in a VM workspace, where the gateway adds the run token. nil refuses API-side
	// ops (and the records of git-side ones) with a usage error.
	API API
	// TaskID is the run's task ($FACTORY_TASK_ID).
	TaskID string
	// Config is the run's policy and layout ($FACTORY_EFFECTOR); nil skips the local
	// pre-check (the dispatcher still checks).
	Config *pipeline.EffectorConfig
	// State is the idempotency state file ($FACTORY_OUT/.effector-seq); "" keeps the state
	// in memory, for this process only.
	State string
	// Result is $FACTORY_RESULT, where `factory result` copies a valid Result.
	Result string
	// Dir is where git ops run: Config.RepoDir, else the working directory.
	Dir string
	// Git is the git executable (default "git", from PATH).
	Git string
	// Env is git's environment (default os.Environ()): the VM workspace run's, with no
	// credential helper, a placeholder GH_TOKEN, and the gateway's proxy and CA variables.
	Env []string
	// Attempts per call on network errors (default DefaultAttempts).
	Attempts int
	// Sleep waits between attempts (default: a timer, honouring ctx).
	Sleep func(ctx context.Context, d time.Duration) error
}

// Effector runs effect ops for one task.
type Effector struct {
	o      Options
	keys   *keyStore
	labels patterns.Pattern
	lerr   error
	// gitMu serialises the git-side ops of one process (the MCP server may get concurrent
	// calls): two pushes or rebases in one checkout at once would trip over each other.
	gitMu sync.Mutex
}

// New returns an Effector.
func New(o Options) *Effector {
	if o.Git == "" {
		o.Git = "git"
	}
	if o.Attempts <= 0 {
		o.Attempts = DefaultAttempts
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Dir == "" && o.Config != nil {
		o.Dir = o.Config.RepoDir
	}
	if o.TaskID == "" && o.Config != nil {
		o.TaskID = o.Config.Task
	}
	e := &Effector{o: o, keys: &keyStore{path: o.State}}
	if c := o.Config; c != nil && len(c.Labels) > 0 {
		e.labels, e.lerr = patterns.Compile(c.Labels)
	}
	return e
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FromEnv builds the Effector of a run from its environment: FACTORY_API, FACTORY_TASK_ID,
// FACTORY_OUT, FACTORY_RESULT, FACTORY_EFFECTOR and FACTORY_RUNTIME. Requests carry no
// Authorization: the gateway adds the run token. A nil client uses a default one, which
// honours HTTP_PROXY (the workspace's gateway).
//
// It fails closed on the runtime: FACTORY_RUNTIME must be pipeline.RuntimeWorkspace (the
// host runtime is gone), and a FACTORY_TOKEN is refused, since the workspace holds no
// credential. Both are usage errors that name the variable, never its value.
func FromEnv(getenv func(string) string, client *http.Client) (*Effector, error) {
	o := Options{TaskID: getenv(pipeline.EnvTaskID), Result: getenv(pipeline.EnvResult)}
	if getenv(pipeline.EnvRuntime) != pipeline.RuntimeWorkspace {
		return nil, usagef("$%s must be %q: the Effector runs only in a VM workspace", pipeline.EnvRuntime, pipeline.RuntimeWorkspace)
	}
	if getenv(pipeline.EnvToken) != "" {
		return nil, usagef("$%s is set in the workspace runtime ($%s=%s), which holds no credential: the gateway adds the run token",
			pipeline.EnvToken, pipeline.EnvRuntime, pipeline.RuntimeWorkspace)
	}
	if out := getenv(pipeline.EnvOut); out != "" {
		o.State = filepath.Join(out, StateFile)
	}
	if p := getenv(pipeline.EnvEffector); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read $%s: %w", pipeline.EnvEffector, err)
		}
		var c pipeline.EffectorConfig
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("parse $%s: %w", pipeline.EnvEffector, err)
		}
		o.Config = &c
	}
	if base := getenv(pipeline.EnvAPI); base != "" {
		if client == nil {
			client = &http.Client{Timeout: requestTimeout}
		}
		api, err := apiclient.NewClientWithResponses(strings.TrimRight(base, "/")+"/api/v1", apiclient.WithHTTPClient(client))
		if err != nil {
			return nil, fmt.Errorf("the dispatcher client: %w", err)
		}
		o.API = api
	}
	return New(o), nil
}

// Do runs the op named by its MCP tool name (pr_open) or CLI words (pr open) with its
// arguments (the MCP tool's input). The result is the JSON the CLI prints.
func (e *Effector) Do(ctx context.Context, op string, args map[string]any) (map[string]any, error) {
	o, ok := lookup(op)
	if !ok {
		return nil, usagef("unknown op %q", op)
	}
	return e.do(ctx, o, args, nil)
}

func (e *Effector) do(ctx context.Context, o *Op, args map[string]any, stdin io.Reader) (map[string]any, error) {
	a, err := o.parse(args, stdin)
	if err != nil {
		return nil, err
	}
	return o.run(ctx, e, a)
}

// precheck is the local pre-check (effector.md §Behaviour 1): op in effects.allow, and every
// label the effect carries (labelArgs) matching effects.labels, as the dispatcher checks them. It is fast
// feedback, not security: without a Config it passes.
func (e *Effector) precheck(op v1alpha1.EffectOp, args map[string]any) error {
	c := e.o.Config
	if c == nil {
		return nil
	}
	if !slices.Contains(c.Allow, string(op)) {
		allowed := "none"
		if len(c.Allow) > 0 {
			allowed = strings.Join(c.Allow, ", ")
		}
		return notAllowedf("%s is not allowed for this task (effects.allow: %s); the dispatcher would refuse it too", op, allowed)
	}
	keys, ok := labelArgs[op]
	if !ok {
		return nil
	}
	globs := "none"
	if len(c.Labels) > 0 {
		globs = strings.Join(c.Labels, ", ")
	}
	for _, k := range keys {
		for _, l := range stringsArg(args, k) {
			if e.lerr != nil || e.labels == nil || !e.labels.Match(l) {
				return notAllowedf("label %q (%s) is not allowed for this task (effects.labels: %s); the dispatcher would refuse it too", l, k, globs)
			}
		}
	}
	return nil
}

// labelArgs lists, per op, every arg that carries a label the op can write: the
// dispatcher's table (internal/server/effects.go, factory#43), so the pre-check refuses
// exactly the labels it would. link-duplicate's `duplicate` label is the adapter's own,
// authorised with the op.
var labelArgs = map[v1alpha1.EffectOp][]string{
	v1alpha1.OpLabelAdd:    {"labels", "label"},
	v1alpha1.OpLabelRemove: {"labels", "label"},
	v1alpha1.OpCreateIssue: {"labels"},
	v1alpha1.OpPROpen:      {"labels"},
	// The Q5 fallback: added when review requirements block the merge.
	v1alpha1.OpPRMerge: {"mergeLabel"},
}

// stringsArg is args[k] as labels: a string, or a list of strings.
func stringsArg(args map[string]any, k string) []string {
	switch v := args[k].(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		return v
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// apply sends one effect (an API op, or the record of a git op) and returns the
// dispatcher's answer as the CLI's output. A network error is retried with the same key.
func (e *Effector) apply(ctx context.Context, op v1alpha1.EffectOp, target string, args map[string]any, key string) (map[string]any, error) {
	if e.o.API == nil || e.o.TaskID == "" {
		return nil, usagef("%s needs $%s and $%s: factory runs inside a factory task", op, pipeline.EnvAPI, pipeline.EnvTaskID)
	}
	k, commit, err := e.keys.reserve(e.o.TaskID, op, target, args, key)
	if err != nil {
		return nil, err
	}
	body := apiclient.EffectRequest{Op: op, Args: args, IdempotencyKey: k}
	if target != "" {
		body.Target = &target
	}
	var (
		res  *http.Response
		resp []byte
	)
	for attempt := 1; ; attempt++ {
		res, err = e.o.API.CreateEffect(ctx, e.o.TaskID, body)
		if err == nil {
			resp, err = io.ReadAll(io.LimitReader(res.Body, maxResponse))
			_ = res.Body.Close()
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, &Error{Exit: ExitRejected, Msg: fmt.Sprintf("%s: %v", op, ctx.Err()), Err: ctx.Err()}
		}
		if attempt >= e.o.Attempts {
			return nil, &Error{Exit: ExitRejected, Err: err, Extra: map[string]any{"idempotencyKey": k},
				Msg: fmt.Sprintf("%s: the dispatcher can't be reached after %d attempts (%v); running the same command again replays it safely", op, attempt, err)}
		}
		if serr := e.o.Sleep(ctx, time.Duration(attempt)*250*time.Millisecond); serr != nil {
			return nil, &Error{Exit: ExitRejected, Msg: fmt.Sprintf("%s: %v", op, serr), Err: serr}
		}
	}
	if res.StatusCode != http.StatusOK {
		return nil, rejection(op, k, res, resp)
	}
	var ok apiclient.EffectResponse
	if err := json.Unmarshal(resp, &ok); err != nil || ok.EffectId == "" {
		return nil, &Error{Exit: ExitRejected, Msg: fmt.Sprintf("%s: the dispatcher's answer isn't an effect: %.200s", op, resp),
			Extra: map[string]any{"idempotencyKey": k}}
	}
	if err := commit(); err != nil {
		return nil, err
	}
	out := map[string]any{"effectId": ok.EffectId, "idempotencyKey": k}
	if u := ok.Url; u != nil && *u != "" {
		out["url"] = *u
		if ref := refFromURL(*u); ref != "" {
			out["ref"] = ref
		}
	}
	return out, nil
}

// rejection maps a non-2xx answer to a clear Error (exit 4).
func rejection(op v1alpha1.EffectOp, key string, res *http.Response, body []byte) *Error {
	status := res.StatusCode
	p := &Problem{Status: status}
	var raw apiclient.Problem
	if json.Unmarshal(body, &raw) == nil {
		p.Title, p.Fields = raw.Title, raw.Errors
		if raw.Code != nil {
			p.Code = *raw.Code
		}
		if raw.Detail != nil {
			p.Detail = *raw.Detail
		}
	}
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
		p.RetryAfter = s
	}
	detail := p.Detail
	if detail == "" {
		detail = p.Title
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	for _, f := range p.Fields {
		detail += fmt.Sprintf(" (%s: %s)", f.Field, f.Message)
	}
	var msg string
	switch status {
	case http.StatusBadRequest:
		msg = fmt.Sprintf("%s was refused as invalid: %s", op, detail)
	case http.StatusUnauthorized:
		msg = fmt.Sprintf("%s: the dispatcher refused the run's credentials: %s", op, detail)
	case http.StatusForbidden:
		msg = fmt.Sprintf("%s is not allowed: %s", op, detail)
	case http.StatusNotFound:
		msg = fmt.Sprintf("%s: not found: %s", op, detail)
	case http.StatusTooManyRequests:
		wait := "later"
		if p.RetryAfter > 0 {
			wait = fmt.Sprintf("in %ds", p.RetryAfter)
		}
		msg = fmt.Sprintf("%s is rate limited: %s; retry %s with the same arguments (the call replays, it won't duplicate)", op, detail, wait)
	default:
		msg = fmt.Sprintf("%s failed: the dispatcher answered %d: %s", op, status, detail)
	}
	return &Error{Exit: ExitRejected, Msg: msg, Problem: p, Extra: map[string]any{"idempotencyKey": key}}
}

// refFromURL turns a GitHub issue or pull request URL into owner/name#n ("" otherwise).
func refFromURL(u string) string {
	rest := u
	for _, p := range []string{"https://", "http://"} {
		rest = strings.TrimPrefix(rest, p)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 || (parts[3] != "pull" && parts[3] != "issues") {
		return ""
	}
	n, _, _ := strings.Cut(parts[4], "#")
	if _, err := strconv.Atoi(n); err != nil {
		return ""
	}
	return parts[1] + "/" + parts[2] + "#" + n
}
