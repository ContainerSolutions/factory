package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/filestore"
)

// stateEpoch is when the fixture starts.
var stateEpoch = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// stateTranscript is the fixture run's transcript, before gzip.
const stateTranscript = `{"type":"thread.started"}
{"type":"item.completed","item":{"type":"command_execution","command":"make test","exit_code":2}}
{"type":"turn.completed","usage":{"input_tokens":182000,"output_tokens":9100}}
`

// stateFixture is a state dir as a dispatcher leaves it after a failed implement run: the
// first part in the snapshot (the store was closed and reopened), the rest in the journal.
// The store is returned open, as a running serve holds it, with its clock.
type stateFixture struct {
	dir string
	st  *filestore.Store
	clk *clock.Fake
}

func newStateFixture(t *testing.T) *stateFixture {
	t.Helper()
	ctx := t.Context()
	f := &stateFixture{dir: filepath.Join(t.TempDir(), "state"), clk: clock.NewFake(stateEpoch)}
	opts := store.Options{Clock: f.clk, Logger: slog.New(slog.DiscardHandler)}
	open := func() {
		t.Helper()
		st, err := filestore.Open(f.dir, opts, fileStoreKinds()...)
		if err != nil {
			t.Fatal(err)
		}
		f.st = st
		t.Cleanup(func() { _ = st.Close() })
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		f.clk.Advance(7 * time.Second)
	}
	open()

	issue := "compliance-framework/mock-docs#4"
	labeled := &v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "gh-7f3a/0", Source: "github-public",
		Type: v1alpha1.EventIssueLabeled, Subject: issue, Time: f.clk.Now(),
		Data: v1alpha1.EventData{
			Provider: "github-public", Actor: &v1alpha1.Actor{ID: "github-public:gusfcarvalho", Tier: "maintainer"},
			Object: &v1alpha1.ObjectRef{Provider: "github-public", Kind: "issue", Ref: issue},
			Repo:   "compliance-framework/mock-docs", Facts: map[string]any{"label": "factory:implement"},
		},
	}
	must(f.st.Create(ctx, labeled))
	opened := &v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "gh-81c2/0", Source: "github-public",
		Type: v1alpha1.EventIssueOpened, Subject: "compliance-framework/mock-docs#9", Time: f.clk.Now(),
		Data: v1alpha1.EventData{Provider: "github-public", Object: &v1alpha1.ObjectRef{Provider: "github-public", Kind: "issue", Ref: "compliance-framework/mock-docs#9"}},
	}
	must(f.st.Create(ctx, opened))
	must(f.st.Create(ctx, &controllers.EventStatus{
		TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindEventStatus},
		Metadata: v1alpha1.ObjectMeta{Name: labeled.ID}, WorkItem: "wi_docs4", ProcessedAt: f.clk.Now(),
	}))
	must(f.st.Create(ctx, &controllers.EventStatus{
		TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindEventStatus},
		Metadata: v1alpha1.ObjectMeta{Name: opened.ID}, ProcessedAt: f.clk.Now(),
	}))

	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: "wi_docs4", Labels: map[string]string{"queue": "docs"}},
		Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: "github-public", Ref: issue, Kind: "issue", Primary: true}}},
		Status: &v1alpha1.WorkItemStatus{
			State:    map[string]any{"phase": "implementing", "spec": map[string]any{"approved": true, "path": "docs/restart.md"}},
			Counters: map[string]int{"implement": 1},
			Reporter: &v1alpha1.Actor{ID: "github-public:gusfcarvalho"},
			Decisions: []v1alpha1.Decision{
				{At: labeled.Time, Event: labeled.ID, RuleSet: "docs", Rule: "triage-new-issues", Reason: "where: item.issue.labels lacks factory:triage"},
				{
					At: labeled.Time, Event: labeled.ID, RuleSet: "docs", Rule: "implement-on-label", Fired: true,
					Actions: []string{"dispatch implement → docs", "set phase"},
					Rails:   []v1alpha1.RailVerdict{{Rail: "killswitch", Verdict: v1alpha1.RailAllowed}, {Rail: "budget", Verdict: v1alpha1.RailAllowed}},
				},
			},
		},
	}
	must(f.st.Create(ctx, wi))
	agent := "agt_laptop"
	task := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{
			ID: "tsk_01JB7Q", WorkItem: "wi_docs4",
			Provenance: &v1alpha1.Provenance{Event: labeled.ID, RuleSet: "docs", Rule: "implement-on-label", CatalogSHA: "4615c40"},
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: "implement", Digest: "sha256:9c1e"}, Queue: "docs", Pool: "default", Provider: "github-public",
			Target: &v1alpha1.Target{Provider: "github-public", Repo: "compliance-framework/mock-docs"},
			Inputs: map[string]any{"issue": issue, "spec": "docs/restart.md"},
			Harness: &v1alpha1.TaskHarness{Request: &v1alpha1.HarnessRequest{Name: "codex", Config: &v1alpha1.HarnessConfig{
				Model: v1alpha1.Scalar("gpt-5.5"), ReasoningEffort: v1alpha1.Scalar("high"),
			}}},
		},
		Status: &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending, Agent: nil},
	}
	must(f.st.Create(ctx, task))
	other := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{ID: "tsk_01JA00", WorkItem: "wi_docs1"},
		Spec:     v1alpha1.TaskRunSpec{TaskSpec: v1alpha1.TaskSpecRef{Name: "release-docs"}, Queue: "docs"},
		Status:   &v1alpha1.TaskStatus{Phase: v1alpha1.TaskSucceeded},
	}
	must(f.st.Create(ctx, other))

	// A restart: everything so far is in the snapshot.
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	open()

	get := func(k store.Key, out store.Object) {
		t.Helper()
		if err := f.st.Get(ctx, k, out); err != nil {
			t.Fatal(err)
		}
	}
	taskKey := store.Key{Kind: v1alpha1.KindTask, Name: task.Metadata.ID}
	get(taskKey, task)
	task.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskLeased, Agent: &agent, Attempts: 1, LeaseExpiresAt: f.clk.Now().Add(10 * time.Minute)}
	task.Spec.Harness.Resolved = &v1alpha1.HarnessResolved{Name: "codex", Version: "0.50.0", Auth: "subscription", Model: "gpt-5.5", ReasoningEffort: "high"}
	must(f.st.Update(ctx, task))
	run := &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: "run_01JB7R", Labels: map[string]string{scheduler.LabelTask: task.Metadata.ID}},
		Spec:     v1alpha1.RunSpec{Task: task.Metadata.ID, Attempt: 1, Agent: agent, Owner: "gusfcarvalho"},
		Status:   &v1alpha1.RunStatus{StartedAt: f.clk.Now()},
	}
	must(f.st.Create(ctx, run))
	task.Status.Phase = v1alpha1.TaskRunning
	must(f.st.Update(ctx, task))
	runKey := store.Key{Kind: v1alpha1.KindRun, Name: run.Metadata.Name}
	get(runKey, run)
	run.Status.Progress = []v1alpha1.ProgressEvent{
		{At: f.clk.Now(), Kind: "phase", Message: "cloning compliance-framework/mock-docs"},
		{At: f.clk.Now().Add(3 * time.Minute), Kind: "check", Message: "make test", Data: map[string]any{"exit": 2}},
	}
	must(f.st.Update(ctx, run))
	f.clk.Advance(9 * time.Minute)

	cost := 0.0
	get(runKey, run)
	run.Status.EndedAt, run.Status.End = f.clk.Now(), v1alpha1.RunCompleted
	run.Status.Usage = &v1alpha1.Usage{InputTokens: 182000, OutputTokens: 9100, Turns: 23, WallClock: "9m14s"}
	question := (*string)(nil)
	run.Status.Result = &v1alpha1.Result{
		Task: task.Metadata.ID, Status: v1alpha1.ResultFailed, Outcome: "checks-failed",
		// Agent text may hold terminal escapes: the CLI drops them.
		Summary:   "Pushed the branch, but `make test` fails:\n\x1b[31mTestRestart\x1b[0m expects the stack to be down first.",
		Artifacts: []v1alpha1.Artifact{{Kind: "branch", Ref: "fx/mock-docs-4-restart"}},
		Suggest:   []v1alpha1.Suggestion{{TaskSpec: "implement", Reason: "retry once the test is fixed"}},
		Question:  question,
		Harness: &v1alpha1.ResultHarness{Name: "codex", Version: "0.50.0", Auth: "subscription",
			EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5.5", ReasoningEffort: "high"},
			Usage:           v1alpha1.Usage{InputTokens: 182000, OutputTokens: 9100, CostUSD: &cost}},
		Data: map[string]any{"checks": map[string]any{"build": "ok", "test": "failed"}},
	}
	run.Status.TranscriptRef = "transcript:" + run.Metadata.Name
	must(f.st.Update(ctx, run))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(stateTranscript)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutTranscript(ctx, run.Metadata.Name, gz.Bytes()); err != nil {
		t.Fatal(err)
	}
	get(taskKey, task)
	task.Status.Phase, task.Status.Reason = v1alpha1.TaskFailed, "run run_01JB7R: failed (checks-failed)"
	must(f.st.Update(ctx, task))
	completed := &v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "factory/run_01JB7R", Source: "factory",
		Type: v1alpha1.EventFactoryRunCompleted, Time: f.clk.Now(), WorkItem: "wi_docs4",
		Data: v1alpha1.EventData{Provider: "factory", Facts: map[string]any{"status": "failed"}},
	}
	must(f.st.Create(ctx, completed))
	return f
}

// state runs factory state with the fixture's clock and no FACTORY_STATE_DIR; the state dir
// is replaced by <state-dir> in the output.
func (f *stateFixture) state(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := stateEnv{
		getenv: func(string) string { return "" },
		home:   func() (string, error) { return filepath.Join(filepath.Dir(f.dir), "home"), nil },
		now:    f.clk.Now,
	}
	code = runState(t.Context(), args, env, &out, &errOut)
	clean := func(s string) string { return strings.ReplaceAll(s, filepath.Dir(f.dir), "<tmp>") }
	return code, clean(out.String()), clean(errOut.String())
}

// checkStateGolden compares got with testdata/state/<name>.golden, or rewrites it with -update.
func checkStateGolden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "state", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (rerun with -update)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update and review the diff):\n%s", file, got)
	}
}

func TestStateGolden(t *testing.T) {
	f := newStateFixture(t)
	before := snapshotFiles(t, f.dir)
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"tasks", []string{"tasks"}, 0},
		{"tasks-failed", []string{"tasks", "--phase", "failed", "--spec", "implement", "--workitem", "wi_docs4"}, 0},
		{"tasks-since", []string{"tasks", "--since", "30s"}, 0},
		{"task", []string{"task", "tsk_01JB7Q"}, 0},
		{"task-other", []string{"--state-dir", f.dir, "task", "tsk_01JA00"}, 0},
		{"run", []string{"run", "run_01JB7R"}, 0},
		{"workitem", []string{"workitem", "wi_docs4"}, 0},
		{"events", []string{"events"}, 0},
		{"events-filtered", []string{"events", "--type", "issue.labeled", "--since", "1h", "--workitem", "wi_docs4"}, 0},
		{"events-help", []string{"events", "--help"}, 0}, // says dropped events aren't stored, and the way back
		{"log", []string{"log"}, 0},
		{"log-since", []string{"log", "--since", "1m"}, 0},
		{"task-not-found", []string{"task", "tsk_01JB7X"}, 1},
		{"run-not-found", []string{"run", "nope"}, 1},
		{"bad-phase", []string{"tasks", "--phase", "Done"}, exitUsage},
		{"bad-since", []string{"events", "--since", "yesterday"}, exitUsage},
		{"missing-id", []string{"workitem"}, exitUsage},
		{"unknown-command", []string{"jobs"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args[0] != "--state-dir" && args[0] != "jobs" {
				args = append([]string{args[0], "--state-dir", f.dir}, args[1:]...)
			}
			code, out, errOut := f.state(t, args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, out, errOut)
			}
			got := out
			if errOut != "" {
				got += "--- stderr\n" + errOut
			}
			checkStateGolden(t, tc.name, got)
		})
	}
	if after := snapshotFiles(t, f.dir); !reflect.DeepEqual(after, before) {
		t.Fatal("factory state changed the state dir")
	}
}

// snapshotFiles returns every file under dir with its bytes.
func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		out[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStateTranscript(t *testing.T) {
	f := newStateFixture(t)
	code, out, errOut := f.state(t, "run", "run_01JB7R", "--transcript", "--state-dir", f.dir)
	if code != 0 || out != stateTranscript || errOut != "" {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

// TestStateJSON: --json prints the stored objects, which decode back to what the store holds.
func TestStateJSON(t *testing.T) {
	f := newStateFixture(t)
	ctx := t.Context()
	decode := func(t *testing.T, args []string, out any) {
		t.Helper()
		code, stdout, stderr := f.state(t, append(args, "--json", "--state-dir", f.dir)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		dec := json.NewDecoder(strings.NewReader(stdout))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			t.Fatalf("%v\n%s", err, stdout)
		}
	}
	list := func(t *testing.T, kind string, out any) {
		t.Helper()
		if _, err := f.st.List(ctx, kind, store.ListOptions{}, out); err != nil {
			t.Fatal(err)
		}
	}
	same := func(t *testing.T, what string, got, want any) {
		t.Helper()
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if !bytes.Equal(g, w) {
			t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
		}
	}

	t.Run("tasks", func(t *testing.T) {
		var got, want []v1alpha1.Task
		decode(t, []string{"tasks"}, &got)
		list(t, v1alpha1.KindTask, &want)
		sortTasks(want)
		same(t, "tasks", got, want)
	})
	t.Run("task", func(t *testing.T) {
		var got taskView
		decode(t, []string{"task", "tsk_01JB7Q"}, &got)
		var want taskView
		if err := f.st.Get(ctx, store.Key{Kind: v1alpha1.KindTask, Name: "tsk_01JB7Q"}, &want.Task); err != nil {
			t.Fatal(err)
		}
		list(t, v1alpha1.KindRun, &want.Runs)
		var wi v1alpha1.WorkItem
		if err := f.st.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: "wi_docs4"}, &wi); err != nil {
			t.Fatal(err)
		}
		want.Decision = &wi.Status.Decisions[1]
		same(t, "task", got, want)
	})
	t.Run("empty lists", func(t *testing.T) {
		for _, args := range [][]string{{"tasks", "--phase", "Cancelled"}, {"events", "--type", "pr.merged"}, {"task", "tsk_01JA00"}} {
			code, stdout, stderr := f.state(t, append(args, "--json", "--state-dir", f.dir)...)
			if code != 0 || (!strings.HasPrefix(stdout, "[]") && !strings.Contains(stdout, `"runs": []`)) {
				t.Errorf("%v: exit %d\n%s%s", args, code, stdout, stderr)
			}
		}
	})
	t.Run("run", func(t *testing.T) {
		var got, want v1alpha1.Run
		decode(t, []string{"run", "run_01JB7R"}, &got)
		if err := f.st.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_01JB7R"}, &want); err != nil {
			t.Fatal(err)
		}
		same(t, "run", got, want)
	})
	t.Run("workitem", func(t *testing.T) {
		var got, want workItemView
		decode(t, []string{"workitem", "wi_docs4"}, &got)
		if err := f.st.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: "wi_docs4"}, &want.WorkItem); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.List(ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskWorkItem: "wi_docs4"}}, &want.Tasks); err != nil {
			t.Fatal(err)
		}
		same(t, "workitem", got, want)
	})
	t.Run("events", func(t *testing.T) {
		var got, want []v1alpha1.Event
		decode(t, []string{"events"}, &got)
		list(t, v1alpha1.KindEvent, &want)
		if len(got) != len(want) {
			t.Fatalf("%d events, want %d", len(got), len(want))
		}
		byID := map[string]v1alpha1.Event{}
		for _, ev := range want {
			byID[ev.ID] = ev
		}
		for _, ev := range got {
			same(t, ev.ID, ev, byID[ev.ID])
		}
	})
	t.Run("log", func(t *testing.T) {
		var got []logLine
		decode(t, []string{"log"}, &got)
		journal, err := os.ReadFile(filepath.Join(f.dir, "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(journal)), "\n")
		if len(got) != len(lines) {
			t.Fatalf("%d log lines, the journal has %d", len(got), len(lines))
		}
		for i, line := range lines {
			var want logLine
			if err := json.Unmarshal([]byte(line), &want); err != nil {
				t.Fatal(err)
			}
			same(t, "line "+want.V, got[i], want)
		}
	})
}

func TestStateNoDir(t *testing.T) {
	var out, errOut bytes.Buffer
	home := t.TempDir()
	for _, tc := range []struct {
		name, env string
		args      []string
		want      string
	}{
		{"default", "", []string{"tasks"}, "no state dir at " + filepath.Join(home, ".factory", "state") + " (the default)"},
		{"env", filepath.Join(home, "env"), []string{"events"}, "no state dir at " + filepath.Join(home, "env") + " ($FACTORY_STATE_DIR)"},
		{"flag", filepath.Join(home, "env"), []string{"log", "--state-dir", filepath.Join(home, "flag")}, "no state dir at " + filepath.Join(home, "flag") + " (--state-dir)"},
		{"reset", "", []string{"reset", "--yes"}, "no state dir at " + filepath.Join(home, ".factory", "state")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			errOut.Reset()
			env := stateEnv{
				getenv: func(k string) string {
					if k == envStateDir {
						return tc.env
					}
					return ""
				},
				home: func() (string, error) { return home, nil },
				now:  func() time.Time { return stateEpoch },
			}
			code := runState(t.Context(), tc.args, env, &out, &errOut)
			if code != 1 || !strings.Contains(errOut.String(), tc.want) ||
				!strings.Contains(errOut.String(), "pass --state-dir DIR or set FACTORY_STATE_DIR") ||
				!strings.Contains(errOut.String(), "deploy/poc/run.sh uses ${FACTORY_STATE_DIR:-$HOME/.factory/state}") {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut.String())
			}
		})
	}
}

func TestStateReset(t *testing.T) {
	f := newStateFixture(t)
	dst := f.dir + "." + f.clk.Now().UTC().Format("20060102T150405Z")

	code, out, errOut := f.state(t, "reset", "--state-dir", f.dir)
	if code != exitUsage || out != "" || !strings.Contains(errOut, "pass --yes") {
		t.Fatalf("without --yes: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	code, out, errOut = f.state(t, "reset", "--yes", "--state-dir", f.dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "a dispatcher has <tmp>/state open (LOCK is locked): stop it first") {
		t.Fatalf("while open: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if _, err := os.Stat(f.dir); err != nil {
		t.Fatalf("the dir moved while open: %v", err)
	}

	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	want := snapshotFiles(t, f.dir)
	code, out, errOut = f.state(t, "reset", "--yes", "--state-dir", f.dir)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	checkStateGolden(t, "reset", out)
	if _, err := os.Stat(f.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state dir is still there: %v", err)
	}
	got := map[string]string{}
	for path, data := range snapshotFiles(t, dst) {
		got[strings.Replace(path, dst, f.dir, 1)] = data
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the moved dir differs:\n got %v\nwant %v", keys(got), keys(want))
	}
	// The moved dir still reads.
	if code, _, errOut := f.state(t, "tasks", "--state-dir", dst); code != 0 {
		t.Fatalf("tasks on the moved dir: exit %d: %s", code, errOut)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestStateCopyWithTornLine reads a copy of the dir (made world-readable by the copy) whose
// journal ends in half a line, as a copy taken mid-append does: it reads, warns about both,
// and leaves the files as they were.
func TestStateCopyWithTornLine(t *testing.T) {
	f := newStateFixture(t)
	cp := filepath.Join(filepath.Dir(f.dir), "copy")
	if err := os.CopyFS(cp, os.DirFS(f.dir)); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(cp, "journal.jsonl")
	jf, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jf.WriteString(`{"v":"15","at":"2026-10-21T09:10:38Z","op":"upd`); err != nil {
		t.Fatal(err)
	}
	if err := jf.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, cp)
	code, out, errOut := f.state(t, "log", "--state-dir", cp)
	if code != 0 || !strings.Contains(out, "7 changes since the snapshot (versions 8 to 14)") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"which group or others can access", "a dispatcher refuses to open it", "journal.jsonl: the last line is incomplete"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if !reflect.DeepEqual(snapshotFiles(t, cp), before) {
		t.Fatal("factory state changed the copy")
	}
}
