package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/filestore"
)

const repoRoot = "../.."

// TestServeStoreKinds: every stored kind outside v1alpha1 (a type with GetObjectMeta, the
// store.MetaObject accessor, anywhere under internal/) is registered with serve's file store,
// and nothing else is. A new kind that isn't fails here, rather than at the first write of a
// dispatcher started with --state-dir (the file store refuses kinds it can't load back). Add
// it to internalKinds, and to the file store's kind audit (internal/store/filestore
// fixtures_test.go and store.md §File store).
func TestServeStoreKinds(t *testing.T) {
	mod := modulePath(t)
	found := map[string]string{} // pkgpath.Type → file
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "GetObjectMeta" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			typ := fn.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				found[mod+"/"+filepath.ToSlash(rel)+"."+id.Name] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 8 {
		t.Fatalf("found only %d MetaObject types: the scan is broken (%v)", len(found), found)
	}
	registered := map[string]string{} // pkgpath.Type → kind
	for kind, proto := range internalKinds() {
		typ := reflect.TypeOf(proto).Elem()
		registered[typ.PkgPath()+"."+typ.Name()] = kind
		if obj, ok := proto.(store.MetaObject); !ok || obj.GetObjectMeta() == nil {
			t.Errorf("kind %s: %T is not a store.MetaObject", kind, proto)
		}
	}
	for typ, file := range found {
		if _, ok := registered[typ]; !ok {
			t.Errorf("%s (%s) is a stored kind that factory serve's file store doesn't register: add it to internalKinds (cmd/factory/serve_store.go) and to the file store's kind audit", typ, file)
		}
	}
	for typ, kind := range registered {
		if _, ok := found[typ]; !ok {
			t.Errorf("internalKinds registers %s as %s, which no longer implements GetObjectMeta", kind, typ)
		}
	}
}

func modulePath(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if m, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

// stateDirPath is a state dir path for a test: Open creates it 0700 (t.TempDir's own dirs are
// 0755, which it refuses).
func stateDirPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

// pocAdapter is the fake GitHub of the PoC catalog, with gus an org admin and docs#42 open. It
// outlives the restarts of a test, as GitHub outlives the dispatcher's.
func pocAdapter(t *testing.T) *fake.Adapter {
	t.Helper()
	c, errs := catalog.LoadDir(os.DirFS(pocDir), catalog.Options{})
	if c == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	adapter := fake.NewAdapter(fake.Options{Groups: func(name string) (v1alpha1.GroupSpec, bool) {
		g, ok := c.Groups[name]
		if !ok {
			return v1alpha1.GroupSpec{}, false
		}
		return g.Spec, true
	}})
	gh := adapter.Conn(ghProvider)
	gh.SetOrgRole("compliance-framework", "admin", "gus")
	if err := gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: docsIssue}}); err != nil {
		t.Fatal(err)
	}
	return adapter
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// providerDecisions are the work item's decisions for provider events (not factory.* ones):
// a restart that reprocessed a webhook's event would add to them.
func providerDecisions(wi apiclient.WorkItem) []string {
	var out []string
	if wi.Status == nil {
		return nil
	}
	for _, d := range wi.Status.Decisions {
		if !strings.HasPrefix(d.Event, "factory/") {
			out = append(out, d.Event+" "+d.RuleSet+"/"+d.Rule)
		}
	}
	return out
}

// TestServeStateDirRestart: with --state-dir, a restart keeps everything. A fake agent leases
// the plan-epic task and heartbeats; the dispatcher stops (its store compacts) and starts again
// on the same dir and the same GitHub. The rebuild finds the stored work item and leaves it as
// the file has it, though the marker on GitHub differs; the agent's token and lease still
// work, so it heartbeats, uploads its transcript and completes; the task succeeds, its run's
// Result and transcript are readable, and factory.run.completed dispatches implement. No
// event is processed twice: the webhook's decisions and tasks are the same across both
// restarts, and the third start still serves the run and its transcript.
func TestServeStateDirRestart(t *testing.T) {
	ctx := context.Background()
	gh := fakeGitHubUsers(t)
	adapter := pocAdapter(t)
	cfg := testConfig(t, gh.URL)
	cfg.StateDir = stateDirPath(t)
	cli := bearer(testCLIToken)
	start := func() *served {
		t.Helper()
		s := runServed(t, cfg, serveDeps{Getenv: getenvOf(testEnv()), Fake: adapter})
		s.gh = adapter.Conn(ghProvider)
		return s
	}
	stop := func(s *served) {
		t.Helper()
		if err := s.stop(); err != nil {
			t.Fatalf("shutdown: %v\n%s", err, s.logs)
		}
	}
	getWorkItem := func(s *served, id string) apiclient.WorkItem {
		t.Helper()
		res, err := s.client(t).GetWorkItemWithResponse(ctx, id, cli)
		if err != nil || res.JSON200 == nil {
			t.Fatalf("get work item %s: %v %s %s", id, err, res.Status(), res.Body)
		}
		return res.JSON200.WorkItem
	}
	listTasks := func(s *served) []apiclient.Task {
		t.Helper()
		res, err := s.client(t).ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, cli)
		if err != nil || res.JSON200 == nil {
			t.Fatalf("list tasks: %v %s", err, res.Status())
		}
		return res.JSON200.Items
	}
	taskIDs := func(ts []apiclient.Task) []string {
		var ids []string
		for _, tk := range ts {
			ids = append(ids, tk.Metadata.ID+" "+tk.Spec.TaskSpec.Name)
		}
		slices.Sort(ids)
		return ids
	}

	// 1. The webhook, an agent, a lease and a heartbeat.
	s := start()
	c := s.client(t)
	body, err := os.ReadFile(filepath.Join("testdata", "serve", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(s.public+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %s", res.Status)
	}
	reg, err := c.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: "restart", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
	}, enrolled(t, ctx, c, "restart"))
	if err != nil || reg.JSON201 == nil {
		t.Fatalf("register: %v %s %s", err, reg.Status(), reg.Body)
	}
	agentID, agent := reg.JSON201.AgentId, bearer(reg.JSON201.AgentToken)
	wait := "10s"
	var lease *apiclient.Lease
	eventuallyT(t, "a lease", func() bool {
		res, err := c.LeaseTaskWithResponse(ctx, agentID, &apiclient.LeaseTaskParams{Wait: &wait}, agent)
		if err != nil || (res.StatusCode() != http.StatusOK && res.StatusCode() != http.StatusNoContent) {
			t.Fatalf("lease: %v %s %s", err, res.Status(), res.Body)
		}
		lease = res.JSON200
		return lease != nil
	})
	task, wiID := lease.Task.Metadata.ID, lease.Task.Metadata.WorkItem
	if lease.Task.Spec.TaskSpec.Name != "plan-epic" || lease.Lease.Run == nil {
		t.Fatalf("leased %s (run %v), want plan-epic with a run", lease.Task.Spec.TaskSpec.Name, lease.Lease.Run)
	}
	runID := *lease.Lease.Run
	heartbeat := func(s *served, msg string) {
		t.Helper()
		hb, err := s.client(t).HeartbeatLeaseWithResponse(ctx, lease.Lease.Id, apiclient.HeartbeatRequest{
			Progress: []v1alpha1.ProgressEvent{{At: time.Now().UTC(), Kind: "step", Message: msg}},
		}, agent)
		if err != nil || hb.JSON200 == nil || hb.JSON200.Cancel {
			t.Fatalf("heartbeat %q: %v %s %s", msg, err, hb.Status(), hb.Body)
		}
	}
	heartbeat(s, "planning")
	// The work-item controller projects the marker onto the issue.
	eventuallyT(t, "the marker on the issue", func() bool {
		o, _ := s.gh.Object(docsIssue)
		return len(o.Projections) == 1 && o.Projections[0].WorkItemID == wiID
	})
	wiBefore := getWorkItem(s, wiID)
	decisions := providerDecisions(wiBefore)
	tasksBefore := taskIDs(listTasks(s))
	if len(decisions) == 0 || len(tasksBefore) != 1 {
		t.Fatalf("before the restart: decisions %v, tasks %v", decisions, tasksBefore)
	}
	stop(s)
	// Close compacted: the snapshot holds the state and the journal is empty.
	if fi, err := os.Stat(filepath.Join(cfg.StateDir, "journal.jsonl")); err != nil || fi.Size() != 0 {
		t.Fatalf("journal after shutdown: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "snapshot.json")); err != nil {
		t.Fatalf("no snapshot after shutdown: %v", err)
	}

	// While the dispatcher is down, the marker on GitHub changes: the file stays
	// authoritative for the work items it holds.
	if err := adapter.Conn(ghProvider).Update(docsIssue, func(o *provider.Observation) {
		o.Projections[0].State = map[string]any{"from-marker": "yes"}
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Restart on the same dir: the agent carries on.
	s = start()
	logs := s.logs.String()
	for _, want := range []string{`"msg":"store: loaded the state dir"`, `"Lease":1`, `"AgentToken":1`, `"EventStatus":1`, `"tornLineDropped":false`} {
		if !strings.Contains(logs, want) {
			t.Errorf("start-up log has no %s:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, `"msg":"work items rebuilt"`) || !strings.Contains(logs, `"created":0,"existing":1`) {
		t.Errorf("the rebuild didn't find the stored work item:\n%s", logs)
	}
	if wi := getWorkItem(s, wiID); !reflect.DeepEqual(wi.Status.State, wiBefore.Status.State) {
		t.Errorf("work item state after the restart = %v, want the stored %v (the file wins over the marker)", wi.Status.State, wiBefore.Status.State)
	}
	c = s.client(t)
	heartbeat(s, "still planning, across the restart")
	transcript := gzipped(t, `{"kind":"message","text":"planned"}`+"\n")
	up, err := c.UploadTranscriptWithBodyWithResponse(ctx, runID, "application/gzip", bytes.NewReader(transcript), agent)
	if err != nil || up.JSON200 == nil {
		t.Fatalf("upload transcript: %v %s %s", err, up.Status(), up.Body)
	}
	ref := up.JSON200.TranscriptRef
	done, err := c.CompleteLeaseWithResponse(ctx, lease.Lease.Id, apiclient.CompleteRequest{TranscriptRef: &ref, Result: v1alpha1.Result{
		Task: task, Status: v1alpha1.ResultDone, Outcome: "decomposed", Summary: "two sub-issues, across a restart",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data: map[string]any{"plan": []any{
			map[string]any{"issue": "compliance-framework/docs#43", "title": "Translate the intro"},
			map[string]any{"issue": "compliance-framework/docs#44", "title": "Translate the guides"},
		}},
	}}, agent)
	if err != nil || done.StatusCode() != http.StatusNoContent {
		t.Fatalf("complete: %v %s %s", err, done.Status(), done.Body)
	}
	checkRun := func(s *served) {
		t.Helper()
		c := s.client(t)
		gt, err := c.GetTaskWithResponse(ctx, task, cli)
		if err != nil || gt.JSON200 == nil || gt.JSON200.Status.Phase != v1alpha1.TaskSucceeded {
			t.Fatalf("task: %v %s %s", err, gt.Status(), gt.Body)
		}
		gr, err := c.GetRunWithResponse(ctx, runID, cli)
		if err != nil || gr.JSON200 == nil || gr.JSON200.Run.Status == nil || gr.JSON200.Run.Status.Result == nil ||
			gr.JSON200.Run.Status.Result.Summary != "two sub-issues, across a restart" || gr.JSON200.Run.Status.TranscriptRef != ref {
			t.Fatalf("run: %v %s %s", err, gr.Status(), gr.Body)
		}
		var progress []string
		for _, p := range gr.JSON200.Run.Status.Progress {
			progress = append(progress, p.Message)
		}
		if !slices.Contains(progress, "planning") || !slices.Contains(progress, "still planning, across the restart") {
			t.Errorf("run progress %v, want both heartbeats", progress)
		}
		tr, err := c.GetTranscriptWithResponse(ctx, runID, cli)
		if err != nil || tr.StatusCode() != http.StatusOK || !bytes.Equal(tr.Body, transcript) {
			t.Fatalf("transcript: %v %s (%d bytes, want %d)", err, tr.Status(), len(tr.Body), len(transcript))
		}
	}
	checkRun(s)
	// factory.run.completed, an event after the restart, dispatches implement: the rule
	// controller has gone through the stored events by then, and processed none again.
	eventuallyT(t, "the implement task from factory.run.completed", func() bool {
		for _, tk := range listTasks(s) {
			if tk.Spec.TaskSpec.Name == "implement" {
				return true
			}
		}
		return false
	})
	if got := providerDecisions(getWorkItem(s, wiID)); !slices.Equal(got, decisions) {
		t.Errorf("decisions for the webhook's event after the restart = %v, want %v (processed again)", got, decisions)
	}
	tasksAfter := taskIDs(listTasks(s))
	if len(tasksAfter) != 2 || !slices.Contains(tasksAfter, tasksBefore[0]) {
		t.Errorf("tasks after the restart = %v, want %v and the implement task", tasksAfter, tasksBefore)
	}
	stop(s)

	// 3. Another restart: the run, its transcript and the tasks are all there, and nothing
	// is dispatched again.
	s = start()
	checkRun(s)
	if got := taskIDs(listTasks(s)); !slices.Equal(got, tasksAfter) {
		t.Errorf("tasks after the second restart = %v, want %v", got, tasksAfter)
	}
	if got := providerDecisions(getWorkItem(s, wiID)); !slices.Equal(got, decisions) {
		t.Errorf("decisions after the second restart = %v, want %v", got, decisions)
	}
	stop(s)
	for _, secret := range []string{testCLIToken, testJoinToken, "oauth-secret-1", "session-key-0123456789", reg.JSON201.AgentToken} {
		if strings.Contains(s.logs.String(), secret) {
			t.Errorf("a secret is in the logs: %q", secret)
		}
		if grepDir(t, cfg.StateDir, secret) {
			t.Errorf("a secret is in the state dir: %q", secret)
		}
	}
}

// grepDir reports whether any file under dir contains s.
func grepDir(t *testing.T, dir, s string) bool {
	t.Helper()
	hit := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hit = hit || bytes.Contains(b, []byte(s))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hit
}

// Without --state-dir serve keeps its store in memory, as before.
func TestServeMemoryStore(t *testing.T) {
	s := startServe(t, testConfig(t, fakeGitHubUsers(t).URL))
	if !strings.Contains(s.logs.String(), `"msg":"store: in memory; state is lost on restart (--state-dir keeps it)"`) {
		t.Errorf("no in-memory store log:\n%s", s.logs)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
}

// A state dir that can't be opened fails serve at start, saying why.
func TestServeStateDirOpenErrors(t *testing.T) {
	journalLine := func(v int) string {
		b, _ := json.Marshal(map[string]any{"v": strconv.Itoa(v), "at": time.Now().UTC(), "op": "create", "kind": "Task", "name": "tsk_1", "obj": map[string]any{}})
		return string(b) + "\n"
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  []string
	}{
		{"others can read it", func(t *testing.T, dir string) {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}, []string{"state dir is not private", "chmod 700"}},
		{"another dispatcher holds it", func(t *testing.T, dir string) {
			st, err := filestore.Open(dir, store.Options{Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
		}, []string{"locked by another store"}},
		{"unknown format", func(t *testing.T, dir string) {
			writePrivate(t, dir, "snapshot.json", `{"format":99,"storeVersion":"0","objects":[]}`)
		}, []string{"unknown state format", "format 99", "move the state dir aside"}},
		{"corrupt journal", func(t *testing.T, dir string) {
			writePrivate(t, dir, "journal.jsonl", "not json\n"+journalLine(1))
		}, []string{"state is corrupt", "journal.jsonl line 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, "http://127.0.0.1:1")
			cfg.StateDir = stateDirPath(t)
			tc.setup(t, cfg.StateDir)
			err := runServe(context.Background(), cfg, serveDeps{Getenv: getenvOf(testEnv())}, slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("runServe = nil, want the state dir's error")
			}
			for _, w := range append(tc.want, "--state-dir "+cfg.StateDir) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("runServe = %v, want %q", err, w)
				}
			}
		})
	}
}

func writePrivate(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
