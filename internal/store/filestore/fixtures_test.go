package filestore

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/rules"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
)

var jsonUnmarshal = json.Unmarshal

// withInternalKinds registers the internal kinds the dispatcher stores, with their real types.
func withInternalKinds() Option {
	return func(c *config) {
		for kind, proto := range map[string]store.Object{
			auth.KindSession:            &auth.Session{},
			scheduler.KindLease:         &scheduler.Lease{},
			scheduler.KindAgentToken:    &scheduler.AgentToken{},
			controllers.KindDelivery:    &controllers.Delivery{},
			controllers.KindEventStatus: &controllers.EventStatus{},
			controllers.KindDebounce:    &controllers.DebounceRecord{},
			controllers.KindTaskPrompt:  &controllers.TaskPrompt{},
			controllers.KindKillSwitch:  &controllers.KillSwitch{},
		} {
			WithKind(kind, proto)(c)
		}
	}
}

// kindAudit is every kind the store holds, with why its stored form carries no secret. A kind
// that reaches the disk without an entry here fails the tests: add one (after checking), or
// keep the secret out of the store.
var kindAudit = map[string]string{
	v1alpha1.KindEvent:          "normalised provider events: ids, refs, actors, facts and the untrusted raw payload; webhook payloads carry no credentials",
	v1alpha1.KindWorkItem:       "links, rule state, counters and decisions; no credentials",
	v1alpha1.KindTask:           "spec and status; spec.credentials describes the git token to mint (repo, permissions, TTL) and llm is always agent-local: the tokens themselves are minted per lease and never stored",
	v1alpha1.KindRun:            "attempt, agent, progress events and the Result; the agent reports no credentials",
	v1alpha1.KindAgent:          "pool, owner login, capabilities and lending; the agent's token is the AgentToken kind, hashed",
	v1alpha1.KindEffect:         "the provider write asked and its result (URL or error); the identity is a name, its credentials stay in the provider adapter",
	v1alpha1.KindBudgetCounter:  "run, token and USD counts",
	v1alpha1.KindApproval:       "the question, approvers group and decisions (logins)",
	auth.KindSession:            "named by the SHA-256 of the session id (never the cookie value), holds the GitHub login and no GitHub token",
	scheduler.KindLease:         "who holds which task until when and the usage deltas; git credentials are minted per lease and never stored, only their expiry",
	scheduler.KindAgentToken:    "the agent token as a sha256: hash of its secret part, never the token",
	controllers.KindDelivery:    "the raw webhook as received, minus Authorization, Cookie and Proxy-Authorization (github ingress droppedHeaders); body and headers are dropped once handled. X-Hub-Signature-256 is an HMAC of the body, not the secret",
	controllers.KindEventStatus: "an event id, a work item name and a time",
	controllers.KindDebounce:    "a rule's held actions (dispatch, set, notify...): catalog values and event facts, no credentials",
	controllers.KindTaskPrompt:  "the rendered prompt: catalog templates and untrusted provider text; no credential is templated into prompts",
	controllers.KindKillSwitch:  "a pause scope, target, actor and time",
}

// tokenMarkers are the prefixes of the credentials the dispatcher handles.
var tokenMarkers = []string{"ghs_", "gho_", "ghp_", "github_pat_", "sk-ant-", "-----BEGIN", "x-access-token:"}

// storedFile is one state file's bytes.
type storedFile struct {
	path string
	data []byte
}

// readStored returns the snapshot and journal of dir (the transcripts are agent output, not
// store objects).
func readStored(t *testing.T, dir string) []storedFile {
	t.Helper()
	var out []storedFile
	for _, f := range []string{snapshotFile, journalFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, storedFile{path: filepath.Join(dir, f), data: b})
	}
	return out
}

// storedKinds returns the kinds in the files.
func storedKinds(t *testing.T, files []storedFile) map[string]bool {
	t.Helper()
	kinds := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f.path, snapshotFile) {
			var snap struct {
				Objects []snapshotObject `json:"objects"`
			}
			if err := json.Unmarshal(f.data, &snap); err != nil {
				t.Fatalf("%s: %v", f.path, err)
			}
			for _, o := range snap.Objects {
				kinds[o.Kind] = true
			}
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var jl journalLine
			if err := json.Unmarshal(sc.Bytes(), &jl); err != nil {
				t.Fatalf("%s: %v", f.path, err)
			}
			kinds[jl.Kind] = true
		}
	}
	return kinds
}

func checkKindAudit(t *testing.T, files []storedFile) {
	t.Helper()
	kinds := storedKinds(t, files)
	if len(kinds) == 0 {
		t.Fatal("nothing stored")
	}
	for k := range kinds {
		if kindAudit[k] == "" {
			t.Errorf("kind %s reaches the disk with no entry in kindAudit: check that its stored form holds no credential and add a line saying why", k)
		}
	}
	for _, k := range builtinKinds {
		if kindAudit[k] == "" {
			t.Errorf("built-in kind %s has no entry in kindAudit", k)
		}
	}
}

// findTokens returns the token markers in data: in the raw bytes, in every JSON string
// (unescaped) and in every JSON string that is base64 (the encoding of []byte fields such as
// a Delivery's body).
func findTokens(data []byte) []string {
	var hits []string
	check := func(where string, b []byte) {
		for _, m := range tokenMarkers {
			if bytes.Contains(b, []byte(m)) {
				hits = append(hits, where+" "+m)
			}
		}
	}
	check("raw", data)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			check("string", []byte(v))
			if b, err := base64.StdEncoding.DecodeString(v); err == nil {
				check("base64", b)
			}
		case map[string]any:
			for k, x := range v {
				check("key", []byte(k))
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, doc := range bytes.Split(data, []byte("\n")) {
		var v any
		if json.Unmarshal(doc, &v) == nil {
			walk(v)
		}
	}
	var whole any
	if json.Unmarshal(data, &whole) == nil {
		walk(whole)
	}
	return hits
}

func checkNoTokens(t *testing.T, files []storedFile) {
	t.Helper()
	for _, f := range files {
		if hits := findTokens(f.data); len(hits) > 0 {
			t.Errorf("%s holds token-shaped strings: %v", f.path, hits)
		}
	}
}

// TestSecretsFixture stores a real-shaped object of every audited kind, as the dispatcher
// builds them, and scans the journal and the snapshot.
func TestSecretsFixture(t *testing.T) {
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	files := readStored(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files = append(files, readStored(t, dir)...)

	kinds := storedKinds(t, files)
	for k := range kindAudit {
		if !kinds[k] {
			t.Errorf("audited kind %s is not in the fixture: add a real-shaped one to fixtures()", k)
		}
	}
	checkKindAudit(t, files)
	checkNoTokens(t, files)

	// The scanner finds what it looks for, also inside a base64 body.
	planted := delivery("gh-planted")
	planted.Spec.Headers.Set("X-Leak", "token ghs_0123456789abcdef")
	planted.Spec.Body = []byte(`{"token":"github_pat_11ABC"}`)
	b, err := json.Marshal(planted)
	if err != nil {
		t.Fatal(err)
	}
	hits := strings.Join(findTokens(b), ",")
	if !strings.Contains(hits, "ghs_") || !strings.Contains(hits, "base64 github_pat_") {
		t.Fatalf("scanner missed the planted tokens: %q", hits)
	}
}

// ---- fixtures --------------------------------------------------------------------------------

func run(name string) *v1alpha1.Run {
	return &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: map[string]string{scheduler.LabelTask: "tsk_1"}},
		Spec:     v1alpha1.RunSpec{Task: "tsk_1", Attempt: 1, Agent: "agt_1", Owner: "octocat"},
	}
}

func task(id, workItem string, phase v1alpha1.TaskPhase) *v1alpha1.Task {
	return &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{
			ID: id, WorkItem: workItem,
			Provenance: &v1alpha1.Provenance{Event: "gh-1/0", RuleSet: "docs", Rule: "triage", CatalogSHA: "0123abcd"},
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: v1alpha1.TaskSpecRef{Name: "implement"}, Queue: "docs", Pool: "default", Provider: "github-public",
			Target: &v1alpha1.Target{Provider: "github-public", Repo: "compliance-framework/docs"},
			Credentials: &v1alpha1.TaskCredentials{
				Git: &v1alpha1.GitCredentialSpec{Provider: "github-public", Identity: "author", Repo: "compliance-framework/docs"},
				LLM: controllers.LLMAgentLocal,
			},
		},
		Status: &v1alpha1.TaskStatus{Phase: phase},
	}
}

func lease(name string) *scheduler.Lease {
	return &scheduler.Lease{
		TypeMeta: v1alpha1.TypeMeta{Kind: scheduler.KindLease},
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: map[string]string{scheduler.LabelTask: "tsk_1", scheduler.LabelAgent: "agt_1"}},
		Spec: scheduler.LeaseSpec{
			Task: "tsk_1", Agent: "agt_1", Pool: "default", Queue: "docs", Auth: "subscription",
			IssuedAt: epoch, TTL: 10 * time.Minute, Heartbeat: 30 * time.Second,
		},
		Status: scheduler.LeaseStatus{ExpiresAt: epoch.Add(10 * time.Minute), CredentialExpiresAt: epoch.Add(time.Hour)},
	}
}

func delivery(id string) *controllers.Delivery {
	h := http.Header{}
	h.Set("X-GitHub-Event", "issues")
	h.Set("X-GitHub-Delivery", strings.TrimPrefix(id, "gh-"))
	h.Set("X-Hub-Signature-256", "sha256=6c9a7d8f0e1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5")
	h.Set("User-Agent", "GitHub-Hookshot/abc123")
	return &controllers.Delivery{
		TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindDelivery},
		Metadata: v1alpha1.ObjectMeta{Name: id},
		Spec: provider.RawEvent{
			ID: id, Provider: "github-public", Type: "issues", Headers: h, ReceivedAt: epoch,
			Body: []byte(`{"action":"opened","issue":{"number":4,"title":"Restart the stack","user":{"login":"octocat"}},"repository":{"full_name":"compliance-framework/mock-docs"}}`),
		},
		Status: controllers.DeliveryStatus{Phase: controllers.DeliveryReceived},
	}
}

// fixtures returns one real-shaped object of every kind the dispatcher stores.
func fixtures() []store.Object {
	costUSD := 0.42
	return []store.Object{
		&v1alpha1.Event{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "gh-1/0", Source: "github-public",
			Type: v1alpha1.EventIssueOpened, Subject: "compliance-framework/mock-docs#4", Time: epoch,
			Data: v1alpha1.EventData{
				Provider: "github-public", Actor: &v1alpha1.Actor{ID: "github-public:octocat"},
				Object: &v1alpha1.ObjectRef{Provider: "github-public", Kind: "issue", Ref: "compliance-framework/mock-docs#4"},
				Repo:   "compliance-framework/mock-docs", Facts: map[string]any{"labels": []any{"docs"}},
				Raw: json.RawMessage(`{"action":"opened","issue":{"number":4}}`),
			},
		},
		&v1alpha1.WorkItem{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
			Metadata: v1alpha1.ObjectMeta{Name: "wi-1", Labels: map[string]string{"queue": "docs"}},
			Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: "github-public", Ref: "compliance-framework/mock-docs#4", Kind: "issue", Primary: true}}},
			Status: &v1alpha1.WorkItemStatus{
				State:     map[string]any{"phase": "triage"},
				Decisions: []v1alpha1.Decision{{At: epoch, Event: "gh-1/0", RuleSet: "docs", Rule: "triage", Fired: true, Actions: []string{"dispatch"}}},
			},
		},
		task("tsk_1", "wi-1", v1alpha1.TaskPending),
		task("tsk_2", "wi-1", v1alpha1.TaskRunning),
		run("run_1"),
		run("run_gone"),
		&v1alpha1.Agent{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent},
			Metadata: v1alpha1.ObjectMeta{Name: "agt_1"},
			Spec: v1alpha1.AgentSpec{Pool: "default", Owner: "octocat", Capabilities: v1alpha1.AgentCapabilities{
				Harnesses: []v1alpha1.HarnessOffer{{Name: "codex", Version: "0.50.0", Auth: "subscription", Models: []string{"gpt-5"}}},
			}},
			Status: &v1alpha1.AgentStatus{Phase: "Ready", LastHeartbeat: epoch},
		},
		&v1alpha1.Effect{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
			Metadata: v1alpha1.ObjectMeta{Name: "eff_1"},
			Spec: v1alpha1.EffectSpec{
				Task: "tsk_1", Op: v1alpha1.OpComment, IdempotencyKey: "tsk_1/comment/1", Identity: "author",
				Target: v1alpha1.ObjectRef{Provider: "github-public", Ref: "compliance-framework/mock-docs#4"}, Request: map[string]any{"body": "On it."},
			},
			Status: &v1alpha1.EffectStatus{Result: &v1alpha1.EffectResult{URL: "https://github.com/compliance-framework/mock-docs/issues/4#issuecomment-1"}, At: epoch},
		},
		&v1alpha1.BudgetCounter{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter},
			Metadata: v1alpha1.ObjectMeta{Name: "budget.global.all.2026-01-01"},
			Status:   v1alpha1.BudgetCounterStatus{Runs: 1, Tokens: 1200, USD: costUSD},
		},
		&v1alpha1.Approval{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindApproval},
			Metadata: v1alpha1.ApprovalMeta{ObjectMeta: v1alpha1.ObjectMeta{Name: "apr_1"}, WorkItem: "wi-1"},
			Spec:     v1alpha1.ApprovalSpec{Name: "implement", Approvers: "maintainers"},
			Status:   &v1alpha1.ApprovalStatus{Phase: v1alpha1.ApprovalPending},
		},
		&auth.Session{
			TypeMeta:  v1alpha1.TypeMeta{Kind: auth.KindSession},
			Metadata:  v1alpha1.ObjectMeta{Name: "ses_9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
			User:      auth.User{Provider: "github-public", Login: "octocat", Name: "The Octocat", AvatarURL: "https://avatars.githubusercontent.com/u/583231"},
			ExpiresAt: epoch.Add(12 * time.Hour),
		},
		lease("lse_1"),
		&scheduler.AgentToken{
			TypeMeta:  v1alpha1.TypeMeta{Kind: scheduler.KindAgentToken},
			Metadata:  v1alpha1.ObjectMeta{Name: "agt_1"},
			Hash:      "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
			ExpiresAt: epoch.Add(30 * 24 * time.Hour), RenewedAt: epoch,
		},
		delivery("gh-1"),
		&controllers.EventStatus{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindEventStatus},
			Metadata: v1alpha1.ObjectMeta{Name: "gh-1/0"}, WorkItem: "wi-1", ProcessedAt: epoch,
		},
		&controllers.DebounceRecord{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindDebounce},
			Metadata: v1alpha1.ObjectMeta{Name: "deb_1"},
			Spec: controllers.DebounceSpec{
				Key: "triage/compliance-framework/mock-docs#4", WorkItem: "wi-1", Event: "gh-1/0", RuleSet: "docs", Rule: "triage",
				Until: epoch.Add(time.Minute), Then: []rules.ActionIntent{{Set: &rules.Set{Key: "phase", Value: "triage"}}},
			},
		},
		&controllers.TaskPrompt{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindTaskPrompt},
			Metadata: v1alpha1.ObjectMeta{Name: "tsk_1"},
			Text:     "Implement compliance-framework/mock-docs#4: Restart the stack.\n<untrusted>issue body</untrusted>",
			Digest:   "sha256:0d1e2f",
		},
		&controllers.KillSwitch{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindKillSwitch},
			Metadata: v1alpha1.ObjectMeta{Name: controllers.KillSwitchName(controllers.ScopeQueue, "docs")},
			Scope:    controllers.ScopeQueue, Target: "docs", By: "github-public:octocat", At: epoch,
		},
	}
}

// writeMix writes creates, updates and deletes across every kind, transcripts, and holds a
// lock, advancing the clock between writes.
func writeMix(t *testing.T, s *Store, clk *clock.Fake) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range fixtures() {
		must(s.Create(ctx, o))
		clk.Advance(time.Second)
	}
	get := func(k store.Key, out store.Object) {
		t.Helper()
		must(s.Get(ctx, k, out))
	}

	// A task through its phases to a terminal one (retention from its last write).
	var tk v1alpha1.Task
	get(store.Key{Kind: v1alpha1.KindTask, Name: "tsk_1"}, &tk)
	for _, p := range []v1alpha1.TaskPhase{v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskSucceeded} {
		tk.Status.Phase = p
		must(s.Update(ctx, &tk))
		clk.Advance(time.Minute)
	}
	// A work item gains a link (a field index).
	var wi v1alpha1.WorkItem
	get(store.Key{Kind: v1alpha1.KindWorkItem, Name: "wi-1"}, &wi)
	wi.Spec.Links = append(wi.Spec.Links, v1alpha1.Link{Provider: "github-public", Ref: "compliance-framework/mock-docs#5", Kind: "pr"})
	must(s.Update(ctx, &wi))
	// An Event (no metadata) is updated unconditionally.
	var ev v1alpha1.Event
	get(store.Key{Kind: v1alpha1.KindEvent, Name: "gh-1/0"}, &ev)
	ev.Subject = "compliance-framework/mock-docs#4 (edited)"
	must(s.Update(ctx, &ev))
	// A lease heartbeats; the budget counts.
	var ls scheduler.Lease
	get(store.Key{Kind: scheduler.KindLease, Name: "lse_1"}, &ls)
	ls.Status.LastHeartbeat, ls.Status.Usage.InputTokens = clk.Now(), 1000
	must(s.Update(ctx, &ls))
	var bc v1alpha1.BudgetCounter
	get(store.Key{Kind: v1alpha1.KindBudgetCounter, Name: "budget.global.all.2026-01-01"}, &bc)
	bc.Status.Runs++
	must(s.Update(ctx, &bc))
	// A handled delivery drops its body and headers.
	var d controllers.Delivery
	get(store.Key{Kind: controllers.KindDelivery, Name: "gh-1"}, &d)
	d.Spec.Body, d.Spec.Headers = nil, nil
	d.Status = controllers.DeliveryStatus{Phase: controllers.DeliveryNormalised, Events: []string{"gh-1/0"}, HandledAt: clk.Now()}
	must(s.Update(ctx, &d))

	// Transcripts, one with a name that isn't a file name.
	must(s.PutTranscript(ctx, "run_1", []byte("\x1f\x8bgzip-bytes")))
	clk.Advance(time.Second)
	must(s.PutTranscript(ctx, "run_gone", []byte("gone")))
	must(s.PutTranscript(ctx, "odd/name", []byte("odd")))

	// Deletes: a run with its transcript, a debounce record; a deleted name is reused.
	must(s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_gone"}))
	must(s.Delete(ctx, store.Key{Kind: controllers.KindDebounce, Name: "deb_1"}))
	must(s.Delete(ctx, store.Key{Kind: v1alpha1.KindTask, Name: "tsk_2"}))
	clk.Advance(time.Second)
	must(s.Create(ctx, task("tsk_2", "wi-1", v1alpha1.TaskPending)))

	// Locks are not journaled.
	u, err := s.Lock(ctx, "held-before", time.Hour)
	must(err)
	_ = u

	s.tmu.Lock()
	n := len(s.blobs)
	s.tmu.Unlock()
	if n != 2 {
		t.Fatalf("transcripts after the mix = %v, want 2 (run_1 and odd/name)", n)
	}
}
