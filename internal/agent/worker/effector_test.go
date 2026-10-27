package worker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/effector"
	agentharness "github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/internal/fakegh"
	"github.com/ContainerSolutions/factory/internal/agent/owner"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/worker"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// TestEffectorAgainstRealDispatcher runs the Effector against the real dispatcher's
// Effector API (`factory serve --dev-fake-providers`, the PoC catalog) while the worker
// holds the plan-epic task's lease: allowed ops are applied, a --key replays, the local
// pre-check refuses what effects.allow and effects.labels don't allow, and without it the
// dispatcher refuses the same (exit 4).
func TestEffectorAgainstRealDispatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs factory serve")
	}
	ctx := context.Background()
	gh := fakegh.New(t, "gus", 0)
	gh.SetInterval(1)
	d := startServe(t, gh.URL)
	body, err := os.ReadFile(filepath.Join("testdata", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(d.URL+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	plain, _ := cliClient(t, d.URL)
	hold := make(chan struct{})
	stub := &stubRunner{Hold: hold, Outcome: runner.Outcome{Result: &v1alpha1.Result{
		Status: v1alpha1.ResultDone, Outcome: "single", Summary: "one issue",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data:    map[string]any{"plan": []any{}},
	}}}
	started := stub.Started()
	w, err := worker.New(worker.Options{
		Dispatcher: d.URL, Pool: "local", Host: "eff", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending:   v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
		JoinToken: func() (string, error) { return joinSecret, nil },
		Owner:     &owner.DeviceFlow{API: plain, GitHubURL: gh.URL},
		Runner:    stub,
		LeaseWait: 5 * time.Second,
		Logger:    slog.New(slog.NewJSONHandler(&logBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var job *runner.Job
	select {
	case job = <-started:
	case err := <-done:
		t.Fatalf("Run returned: %v\n%s", err, d.logs)
	case <-time.After(30 * time.Second):
		t.Fatal("no lease")
	}
	task := job.Lease.Task
	cfg := pipeline.EffectorConfigFor(task, agentharness.Workspace{})
	e := effector.New(effector.Options{API: job.API, TaskID: job.TaskID(), Config: &cfg})
	do := func(op string, args map[string]any) (map[string]any, error) { return e.Do(ctx, op, args) }

	// The fake provider holds only what the run creates (the webhook's issue is not an
	// object there), so the ops act on issues the task creates in its repo.
	var refs []string
	for _, title := range []string{"Translate the docs", "Translate the intro"} {
		created, err := do("issue_create", map[string]any{"title": title, "body": "Part of the epic."})
		if err != nil {
			t.Fatalf("issue create: %v\n%s", err, d.logs)
		}
		ref, _ := created["ref"].(string)
		if !strings.HasPrefix(ref, "compliance-framework/docs#") || !strings.HasPrefix(created["effectId"].(string), "eff_") {
			t.Fatalf("issue create printed %v", created)
		}
		refs = append(refs, ref)
	}
	if _, err := do("issue_sub_add", map[string]any{"parent": refs[0], "child": refs[1]}); err != nil {
		t.Fatalf("sub-issue add: %v", err)
	}
	first, err := do("comment", map[string]any{"target": refs[0], "body": "Planned: " + refs[1], "key": "plan-summary"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	again, err := do("comment", map[string]any{"target": refs[0], "body": "Planned: " + refs[1], "key": "plan-summary"})
	if err != nil || again["effectId"] != first["effectId"] || again["url"] != first["url"] {
		t.Errorf("a replayed key: %v %v / %v", again, err, first)
	}
	// Local refusals: pr.merge isn't in effects.allow, plan-epic allows no labels.
	for _, c := range []struct {
		op   string
		args map[string]any
	}{{"pr_merge", map[string]any{"pr": "compliance-framework/docs#1"}}, {"label_add", map[string]any{"labels": []any{"docs"}}}} {
		_, err := do(c.op, c.args)
		var fe *effector.Error
		if !errors.As(err, &fe) || fe.Exit != effector.ExitNotAllowed {
			t.Errorf("%s: %v", c.op, err)
		}
	}
	// Without the local policy the dispatcher refuses them itself.
	bare := effector.New(effector.Options{API: job.API, TaskID: job.TaskID()})
	for _, c := range []struct {
		op   string
		args map[string]any
		code string
	}{
		{"pr_merge", map[string]any{"pr": "compliance-framework/docs#1"}, "effect-not-allowed"},
		{"label_add", map[string]any{"labels": []any{"docs"}}, "label-not-allowed"},
		{"comment", map[string]any{"body": "x", "target": "compliance-framework/api#1"}, "target-not-allowed"},
	} {
		_, err := bare.Do(ctx, c.op, c.args)
		var fe *effector.Error
		if !errors.As(err, &fe) || fe.Exit != effector.ExitRejected || fe.Problem == nil || fe.Problem.Code != c.code {
			t.Errorf("%s without the pre-check: %v %+v", c.op, err, fe)
		}
	}
	close(hold)
	eventually(t, "the task to succeed", func() bool {
		cli, admin := cliClient(t, d.URL)
		r, err := cli.GetTaskWithResponse(ctx, job.TaskID(), admin)
		return err == nil && r.JSON200 != nil && r.JSON200.Status.Phase == v1alpha1.TaskSucceeded
	})
	w.Drain()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run didn't return after Drain")
	}
	d.stop(t)
}
