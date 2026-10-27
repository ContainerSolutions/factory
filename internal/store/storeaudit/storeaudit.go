// Package storeaudit keeps secrets out of the durable stores' bytes. It holds the audit of
// every stored kind (why its stored form carries no credential), the scan for token-shaped
// strings, and a real-shaped object of every kind, which the file store's and the object
// store's tests write and then scan: a kind that reaches the disk or the bucket without an
// audit line fails them, and so does a token marker anywhere in what they wrote.
//
// Design: design/workstreams/dispatcher-runtime/LLD/store.md §File store ("What never
// reaches the disk") and §Object store.
package storeaudit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/rules"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// InternalKinds are the internal kinds the dispatcher stores, with their Go types: the same
// table as serve's (cmd/factory internalKinds, which a test keeps equal to this one).
func InternalKinds() map[string]store.Object {
	return map[string]store.Object{
		auth.KindSession:               &auth.Session{},
		scheduler.KindLease:            &scheduler.Lease{},
		scheduler.KindAgentToken:       &scheduler.AgentToken{},
		scheduler.KindEnrollment:       &scheduler.Enrollment{},
		scheduler.KindAgentRefresh:     &scheduler.AgentRefresh{},
		controllers.KindDelivery:       &controllers.Delivery{},
		controllers.KindEventStatus:    &controllers.EventStatus{},
		controllers.KindDebounce:       &controllers.DebounceRecord{},
		controllers.KindTaskPrompt:     &controllers.TaskPrompt{},
		controllers.KindKillSwitch:     &controllers.KillSwitch{},
		objectstore.KindStoreLock:      &objectstore.StoreLock{},
		objectstore.KindTranscriptBlob: &objectstore.TranscriptBlob{},
	}
}

// KindAudit is every kind the stores hold, with why its stored form carries no secret. A kind
// that reaches the disk or a bucket without an entry here fails the tests: add one (after
// checking), or keep the secret out of the store.
var KindAudit = map[string]string{
	v1alpha1.KindEvent:             "normalised provider events: ids, refs, actors, facts and the untrusted raw payload; webhook payloads carry no credentials",
	v1alpha1.KindWorkItem:          "links, rule state, counters and decisions; no credentials",
	v1alpha1.KindTask:              "spec and status; spec.credentials describes the git token to mint (repo, permissions, TTL) and llm is always agent-local: the tokens themselves are minted per lease and never stored",
	v1alpha1.KindRun:               "attempt, agent, progress events and the Result; the agent reports no credentials",
	v1alpha1.KindAgent:             "pool, owner login, capabilities and lending; the agent's token is the AgentToken kind, hashed",
	v1alpha1.KindEffect:            "the provider write asked and its result (URL or error); the identity is a name, its credentials stay in the provider adapter",
	v1alpha1.KindBudgetCounter:     "run, token and USD counts",
	v1alpha1.KindApproval:          "the question, approvers group and decisions (logins)",
	auth.KindSession:               "named by the SHA-256 of the session id (never the cookie value), holds the GitHub login and no GitHub token",
	scheduler.KindLease:            "who holds which task until when and the usage deltas; git credentials are minted per lease and never stored, only their expiry",
	scheduler.KindAgentToken:       "the agent token as a sha256: hash of its secret part, never the token",
	scheduler.KindEnrollment:       "the pool, owner, host name and expiry an admin fixed, and the enrollment token as a sha256: hash, never the token",
	scheduler.KindAgentRefresh:     "the agent's refresh token as a sha256: hash, never the token, and the enrollment it came from",
	controllers.KindDelivery:       "the raw webhook as received, minus Authorization, Cookie and Proxy-Authorization (github ingress droppedHeaders); body and headers are dropped once handled. X-Hub-Signature-256 is an HMAC of the body, not the secret",
	controllers.KindEventStatus:    "an event id, a work item name and a time",
	controllers.KindDebounce:       "a rule's held actions (dispatch, set, notify...): catalog values and event facts, no credentials",
	controllers.KindTaskPrompt:     "the rendered prompt: catalog templates and untrusted provider text; no credential is templated into prompts",
	controllers.KindKillSwitch:     "a pause scope, target, actor and time",
	objectstore.KindStoreLock:      "a lock's name, the holding store's instance id, a random per-hold id and an expiry; no credential",
	objectstore.KindTranscriptBlob: "a transcript blob's run name, size and put time; the blob itself is agent output, kept beside the log",
}

// TokenMarkers are the prefixes of the credentials the dispatcher handles.
var TokenMarkers = []string{"ghs_", "gho_", "ghp_", "github_pat_", "sk-ant-", "-----BEGIN", "x-access-token:"}

// FindTokens returns the token markers in data: in the raw bytes, in every JSON string
// (unescaped) and in every JSON string that is base64 (the encoding of []byte fields such as
// a Delivery's body), whether data is one JSON document or one per line.
func FindTokens(data []byte) []string {
	var hits []string
	check := func(where string, b []byte) {
		for _, m := range TokenMarkers {
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

// Epoch is the fixtures' time.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Run returns a real-shaped Run.
func Run(name string) *v1alpha1.Run {
	return &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: map[string]string{scheduler.LabelTask: "tsk_1"}},
		Spec:     v1alpha1.RunSpec{Task: "tsk_1", Attempt: 1, Agent: "agt_1", Owner: "octocat"},
	}
}

// Task returns a real-shaped Task.
func Task(id, workItem string, phase v1alpha1.TaskPhase) *v1alpha1.Task {
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

// Lease returns a real-shaped Lease.
func Lease(name string) *scheduler.Lease {
	return &scheduler.Lease{
		TypeMeta: v1alpha1.TypeMeta{Kind: scheduler.KindLease},
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: map[string]string{scheduler.LabelTask: "tsk_1", scheduler.LabelAgent: "agt_1"}},
		Spec: scheduler.LeaseSpec{
			Task: "tsk_1", Agent: "agt_1", Pool: "default", Queue: "docs", Auth: "subscription",
			IssuedAt: Epoch, TTL: 10 * time.Minute, Heartbeat: 30 * time.Second,
		},
		Status: scheduler.LeaseStatus{ExpiresAt: Epoch.Add(10 * time.Minute), CredentialExpiresAt: Epoch.Add(time.Hour)},
	}
}

// Delivery returns a real-shaped Delivery, as the GitHub ingress stores it.
func Delivery(id string) *controllers.Delivery {
	h := http.Header{}
	h.Set("X-GitHub-Event", "issues")
	h.Set("X-GitHub-Delivery", strings.TrimPrefix(id, "gh-"))
	h.Set("X-Hub-Signature-256", "sha256=6c9a7d8f0e1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5")
	h.Set("User-Agent", "GitHub-Hookshot/abc123")
	return &controllers.Delivery{
		TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindDelivery},
		Metadata: v1alpha1.ObjectMeta{Name: id},
		Spec: provider.RawEvent{
			ID: id, Provider: "github-public", Type: "issues", Headers: h, ReceivedAt: Epoch,
			Body: []byte(`{"action":"opened","issue":{"number":4,"title":"Restart the stack","user":{"login":"octocat"}},"repository":{"full_name":"compliance-framework/mock-docs"}}`),
		},
		Status: controllers.DeliveryStatus{Phase: controllers.DeliveryReceived},
	}
}

// Fixtures returns one real-shaped object of every kind the dispatcher stores, except the
// object store's own (StoreLock and TranscriptBlob), which it writes itself: a Lock and a
// PutTranscript make them.
func Fixtures() []store.Object {
	costUSD := 0.42
	return []store.Object{
		&v1alpha1.Event{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent}, ID: "gh-1/0", Source: "github-public",
			Type: v1alpha1.EventIssueOpened, Subject: "compliance-framework/mock-docs#4", Time: Epoch,
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
				Decisions: []v1alpha1.Decision{{At: Epoch, Event: "gh-1/0", RuleSet: "docs", Rule: "triage", Fired: true, Actions: []string{"dispatch"}}},
			},
		},
		Task("tsk_1", "wi-1", v1alpha1.TaskPending),
		Task("tsk_2", "wi-1", v1alpha1.TaskRunning),
		Run("run_1"),
		Run("run_gone"),
		&v1alpha1.Agent{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent},
			Metadata: v1alpha1.ObjectMeta{Name: "agt_1"},
			Spec: v1alpha1.AgentSpec{Pool: "default", Owner: "octocat", Capabilities: v1alpha1.AgentCapabilities{
				Harnesses: []v1alpha1.HarnessOffer{{Name: "codex", Version: "0.50.0", Auth: "subscription", Models: []string{"gpt-5"}}},
			}},
			Status: &v1alpha1.AgentStatus{Phase: "Ready", LastHeartbeat: Epoch},
		},
		&v1alpha1.Effect{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
			Metadata: v1alpha1.ObjectMeta{Name: "eff_1"},
			Spec: v1alpha1.EffectSpec{
				Task: "tsk_1", Op: v1alpha1.OpComment, IdempotencyKey: "tsk_1/comment/1", Identity: "author",
				Target: v1alpha1.ObjectRef{Provider: "github-public", Ref: "compliance-framework/mock-docs#4"}, Request: map[string]any{"body": "On it."},
			},
			Status: &v1alpha1.EffectStatus{Result: &v1alpha1.EffectResult{URL: "https://github.com/compliance-framework/mock-docs/issues/4#issuecomment-1"}, At: Epoch},
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
			ExpiresAt: Epoch.Add(12 * time.Hour),
		},
		Lease("lse_1"),
		&scheduler.AgentToken{
			TypeMeta:  v1alpha1.TypeMeta{Kind: scheduler.KindAgentToken},
			Metadata:  v1alpha1.ObjectMeta{Name: "agt_1"},
			Hash:      "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
			ExpiresAt: Epoch.Add(30 * 24 * time.Hour), RenewedAt: Epoch,
		},
		&scheduler.Enrollment{
			TypeMeta: v1alpha1.TypeMeta{Kind: scheduler.KindEnrollment},
			Metadata: v1alpha1.ObjectMeta{Name: "enl_1"},
			Spec: scheduler.EnrollmentSpec{
				Pool: "default", Owner: "octocat", OwnerProvider: "github-public", Name: "mac-mini",
				CreatedBy: "github-public:octocat", ExpiresAt: Epoch.Add(24 * time.Hour),
				TokenHash: "sha256:b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
			},
			Status: scheduler.EnrollmentStatus{UsedAt: Epoch.Add(time.Hour), Agent: "agt_octocat-mac-mini"},
		},
		&scheduler.AgentRefresh{
			TypeMeta:   v1alpha1.TypeMeta{Kind: scheduler.KindAgentRefresh},
			Metadata:   v1alpha1.ObjectMeta{Name: "agt_octocat-mac-mini"},
			Hash:       "sha256:7d865e959b2466918c9863afca942d0fb89d7c9ac0c99bafc3749504ded97730",
			Enrollment: "enl_1", Pool: "default", Owner: "octocat", OwnerProvider: "github-public", Name: "mac-mini",
			RotatedAt: Epoch.Add(time.Hour),
		},
		Delivery("gh-1"),
		&controllers.EventStatus{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindEventStatus},
			Metadata: v1alpha1.ObjectMeta{Name: "gh-1/0"}, WorkItem: "wi-1", ProcessedAt: Epoch,
		},
		&controllers.DebounceRecord{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindDebounce},
			Metadata: v1alpha1.ObjectMeta{Name: "deb_1"},
			Spec: controllers.DebounceSpec{
				Key: "triage/compliance-framework/mock-docs#4", WorkItem: "wi-1", Event: "gh-1/0", RuleSet: "docs", Rule: "triage",
				Until: Epoch.Add(time.Minute), Then: []rules.ActionIntent{{Set: &rules.Set{Key: "phase", Value: "triage"}}},
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
			Scope:    controllers.ScopeQueue, Target: "docs", By: "github-public:octocat", At: Epoch,
		},
	}
}

// ObjectStoreFixtures returns a real-shaped object of each of the object store's own kinds,
// for a store that doesn't make them itself (the file store holds them only as registered
// kinds).
func ObjectStoreFixtures() []store.Object {
	return []store.Object{
		&objectstore.StoreLock{
			TypeMeta: v1alpha1.TypeMeta{Kind: objectstore.KindStoreLock},
			Metadata: v1alpha1.ObjectMeta{Name: "wi/wi-1"},
			Holder:   "ins_0123456789abcdef", Token: "fedcba9876543210", ExpiresAt: Epoch.Add(time.Minute),
		},
		&objectstore.TranscriptBlob{
			TypeMeta: v1alpha1.TypeMeta{Kind: objectstore.KindTranscriptBlob},
			Metadata: v1alpha1.ObjectMeta{Name: "run_1"},
			Size:     2048, PutAt: Epoch,
		},
	}
}
