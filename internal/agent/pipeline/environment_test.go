package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/internal/fakegateway"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// recordEnv is an Environment that records its calls in a shared log and the environment Up got.
type recordEnv struct {
	record func(string)
	vars   map[string]string
	upErr  error

	mu    sync.Mutex
	upEnv []string
}

func (e *recordEnv) Up(_ context.Context, ws harness.Workspace, _ v1alpha1.Task) (map[string]string, error) {
	e.mu.Lock()
	e.upEnv = ws.Env
	e.mu.Unlock()
	e.record("up")
	return e.vars, e.upErr
}

func (e *recordEnv) Down(ctx context.Context) error {
	if _, ok := ctx.Deadline(); ok {
		e.record("down with a deadline")
		return nil
	}
	e.record("down")
	return nil
}

// On the host (no Environment) a task's environment isn't brought up: the step warns, notes it
// in the transcript, and the run goes on.
func TestRunEnvironmentOnTheHost(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*v1alpha1.Task){
		"spec.environment": func(task *v1alpha1.Task) { task.Spec.Environment = &v1alpha1.Environment{Up: "exit 1\n"} },
		"spec.versions":    func(task *v1alpha1.Task) { task.Spec.Versions = map[string]string{"ui": "v1.0.0"} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			transcripts := &recordTranscripts{}
			f.opts.Transcripts = transcripts
			var mu sync.Mutex
			var steps []string
			f.opts.OnStep = func(name string, _ time.Duration, err error) {
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("step %s: %v", name, err)
				}
				steps = append(steps, name)
			}
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult)}}
			task := f.task()
			set(&task)
			out, err := f.pipeline().Run(context.Background(), f.job(task, time.Now()), &sink{})
			if err != nil || out.Status != runner.StatusDone {
				t.Fatalf("outcome %+v %v", out, err)
			}
			mu.Lock()
			if want := []string{"workspace", "credentials", "clone", "environment", "skills", "schema", "prompt", "env", "prepare", "harness", "finish"}; !slices.Equal(steps, want) {
				t.Errorf("steps %v, want %v", steps, want)
			}
			mu.Unlock()
			if len(transcripts.got) != 1 {
				t.Fatalf("%d transcripts", len(transcripts.got))
			}
			if !slices.ContainsFunc(readTranscript(t, transcripts.got[0]), func(l map[string]any) bool { return l["type"] == "factory.environment-skipped" }) {
				t.Error("the transcript has no factory.environment-skipped line")
			}
			if !strings.Contains(f.logs.String(), "which only a VM workspace brings up: skipping it") {
				t.Errorf("no warning in the log:\n%s", f.logs.String())
			}
			f.checkClean("")
		})
	}
}

// In gateway mode, as the VM workspace runs it, the environment step calls Up after clone
// and before skills, with the pipeline's own environment; what Up returns reaches the
// harness. An Up that fails ends the run failed{environment-error} before the harness, and Down
// runs anyway, after Finished, with no deadline of its own.
func TestRunEnvironmentGatewayMode(t *testing.T) {
	t.Parallel()
	errUp := errors.New("compose up failed")
	for _, tt := range []struct {
		name   string
		upErr  error
		status string
		reason string
		events []string
	}{
		{name: "up", status: runner.StatusDone,
			events: []string{"up", "step environment", "step skills", "step schema", "step prompt", "step env", "step prepare", "step harness", "step finish", "finished", "down"}},
		{name: "up fails", upErr: errUp, status: runner.StatusFailed, reason: runner.ReasonEnvironmentError,
			events: []string{"up", "step environment", "step finish", "finished", "down"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, b := f.prepare(f.job(f.task(), time.Now()), pipeline.BundleExtras{})
			gw := fakegateway.Start(t, fakegateway.Config{GitUpstream: f.git.ts.URL, Repos: map[string]fakegateway.Repo{
				"acme/docs": {Token: gitSecret, RW: true}, "acme/private-ro": {Token: readSecret}}})
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			writeFile(t, caFile, string(gw.CA().CertPEM()))
			root := filepath.Join(t.TempDir(), "work")
			f.opts.WorkRoot, f.opts.Root, f.opts.GitAuth, f.opts.GitBaseURL = "", root, pipeline.GitAuthGateway, ""
			f.opts.Env = map[string]string{"HTTPS_PROXY": gw.URL(), "GIT_SSL_CAINFO": caFile, "SSL_CERT_FILE": caFile}
			f.opts.Prompts, f.opts.Schemas, f.opts.Skills = pipeline.BundlePrompts{B: b}, pipeline.BundleSchemas{B: b}, pipeline.BundleSkills(b)
			f.opts.Transcripts = &recordTranscripts{}

			var mu sync.Mutex
			var events []string
			record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
			env := &recordEnv{record: record, upErr: tt.upErr,
				vars: map[string]string{"FACTORY_STACK_URL": "http://localhost:8000", "FACTORY_VERSION_UI": "v1.0.0"}}
			f.opts.Environment = env
			f.opts.OnStep = func(name string, _ time.Duration, err error) {
				if (err != nil) != (tt.upErr != nil && name == "environment") {
					t.Errorf("step %s: %v", name, err)
				}
				if name != "workspace" && name != "credentials" && name != "clone" {
					record("step " + name)
				}
			}
			f.opts.Finished = func(runner.Outcome) { record("finished") }
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult)}}

			job := f.gatewayJob()
			job.Lease.Task.Spec.Environment = &v1alpha1.Environment{Up: "docker compose up -d\n"}
			out, err := f.pipeline().Run(context.Background(), job, &sink{})
			if err != nil || out.Status != tt.status || out.Reason != tt.reason {
				t.Fatalf("outcome %+v %+v %v", out, out.Result, err)
			}
			mu.Lock()
			if !slices.Equal(events, tt.events) {
				t.Errorf("events\n%q\nwant\n%q", events, tt.events)
			}
			mu.Unlock()

			// Up got the pipeline's own environment: the gateway's, the scratch HOME, no token.
			upEnv := envMap(env.upEnv)
			if upEnv["HTTPS_PROXY"] != gw.URL() || upEnv["HOME"] != filepath.Join(root, "home") {
				t.Errorf("Up's environment %v", upEnv)
			}
			for _, k := range []string{pipeline.EnvToken, pipeline.EnvGHToken, pipeline.EnvHeadless} {
				if _, ok := upEnv[k]; ok {
					t.Errorf("Up's environment has %s", k)
				}
			}
			if tt.upErr != nil {
				if out.Result == nil || out.Result.Summary != "environment-error: environment: compose up failed" {
					t.Errorf("result %+v", out.Result)
				}
				if len(f.adapter.Prepared()) != 0 {
					t.Error("the harness was prepared after a failed environment")
				}
				return
			}
			hEnv := envMap(f.adapter.Prepared()[0].Env)
			for k, v := range env.vars {
				if hEnv[k] != v {
					t.Errorf("the harness's %s=%q, want %q", k, hEnv[k], v)
				}
			}
		})
	}
}

// blockEnv is an Environment whose Up blocks until its context ends.
type blockEnv struct{ started chan struct{} }

func (e blockEnv) Up(ctx context.Context, _ harness.Workspace, _ v1alpha1.Task) (map[string]string, error) {
	close(e.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockEnv) Down(context.Context) error { return nil }

// A limit that cancels the environment step (the wall clock) ends the run for that reason, not
// environment-error.
func TestRunEnvironmentWallClockLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clk := clock.NewFake(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	f.opts.Clock = clk
	env := blockEnv{started: make(chan struct{})}
	f.opts.Environment = env
	task := f.task()
	task.Spec.Environment = &v1alpha1.Environment{Up: "sleep 3600\n"}
	done := make(chan runner.Outcome, 1)
	go func() {
		out, err := f.pipeline().Run(context.Background(), f.job(task, clk.Now()), &sink{})
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	select {
	case <-env.started:
	case <-time.After(slow):
		t.Fatal("the environment didn't start")
	}
	clk.Advance(16 * time.Minute)
	select {
	case out := <-done:
		if out.Status != runner.StatusFailed || out.Reason != runner.ReasonLimit {
			t.Errorf("outcome %+v %+v", out, out.Result)
		}
	case <-time.After(slow):
		t.Fatal("the wall-clock limit didn't stop the environment")
	}
	f.checkClean("")
}
