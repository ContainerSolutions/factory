package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/server"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/filestore"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

const leaderLog = `"msg":"leader: this instance holds the leader lock and runs the leader's loops"`

// fakeGitHubOAuth is GitHub's OAuth web flow and GET /user: any code is exchanged for
// "tok-gus", which is gus.
func fakeGitHubOAuth(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/login/oauth/access_token" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-gus"})
		case r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer tok-gus":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "gus"})
		default:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// sharedServe runs a dispatcher on the file:// bucket dir with the shared fake GitHub. Its
// leader lock and its default resync interval come from leader and resync.
type sharedServe struct {
	t       *testing.T
	dir     string
	adapter *fake.Adapter
	github  *httptest.Server
}

func (sh sharedServe) start(name string, syncEvery, resync time.Duration) *served {
	sh.t.Helper()
	cfg := testConfig(sh.t, sh.github.URL)
	cfg.StateURL, cfg.StateMemory, cfg.StateSyncInterval = "file://"+sh.dir, false, syncEvery
	cfg.GitHubURL = sh.github.URL
	s := runServed(sh.t, cfg, serveDeps{
		Getenv: getenvOf(testEnv()), Fake: sh.adapter, ResyncInterval: resync,
		Leader: controllers.LeaderOptions{TTL: 3 * time.Second, Renew: 100 * time.Millisecond, Try: 300 * time.Millisecond},
	})
	s.gh = sh.adapter.Conn(ghProvider)
	sh.t.Logf("%s serves on %s", name, s.public)
	return s
}

// TestServeSharedState is two `serve` processes on one file:// bucket (P3 §Tests, serve): a
// webhook taken by one leads to a task leased and completed through the other; one leader at
// a time, and the other takes over when it stops; a UI login started on one finishes on the
// other, and its state can't be used twice; a /stream resumed on a lagging one with the
// other's Last-Event-ID resumes; and a change made on GitHub while the first led is reported
// by the second's first resync.
func TestServeSharedState(t *testing.T) {
	ctx := context.Background()
	gh := fakeGitHubOAuth(t)
	adapter := pocAdapter(t)
	adapter.Conn(ghProvider).SetOrgRole("compliance-framework", "admin", "gus")
	sh := sharedServe{t: t, dir: filepath.Join(t.TempDir(), "bucket"), adapter: adapter, github: gh}
	cli := bearer(testCLIToken)

	// A starts first and leads; it resyncs rarely, so a change on GitHub waits for B. B
	// doesn't lead while A does. C lags: it syncs only when it must.
	a := sh.start("A", 100*time.Millisecond, time.Hour)
	eventuallyT(t, "A to lead", func() bool { return strings.Contains(a.logs.String(), leaderLog) })
	b := sh.start("B", 100*time.Millisecond, 200*time.Millisecond)
	lagging := sh.start("C", time.Hour, time.Hour)
	time.Sleep(500 * time.Millisecond) // several of B's and C's tries
	for name, s := range map[string]*served{"B": b, "C": lagging} {
		if strings.Contains(s.logs.String(), leaderLog) {
			t.Fatalf("%s leads while A does", name)
		}
	}
	ca, cb := a.client(t), b.client(t)

	// A stream on A, to resume on C later.
	stream := openStream(t, a.public, "")

	// 1. The webhook reaches A; the agent works through B.
	body, err := os.ReadFile(filepath.Join("testdata", "serve", "issue-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(a.public+"/hooks/"+ghProvider, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook on A: %s", res.Status)
	}
	created := stream.next("task.updated")
	reg, err := cb.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: "shared", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
	}, enrolled(t, ctx, cb, "shared"))
	if err != nil || reg.JSON201 == nil {
		t.Fatalf("register on B: %v %s %s", err, reg.Status(), reg.Body)
	}
	agent := bearer(reg.JSON201.AgentToken)
	wait := "10s"
	var lease *apiclient.Lease
	eventuallyT(t, "a lease on B", func() bool {
		res, err := cb.LeaseTaskWithResponse(ctx, reg.JSON201.AgentId, &apiclient.LeaseTaskParams{Wait: &wait}, agent)
		if err != nil || (res.StatusCode() != http.StatusOK && res.StatusCode() != http.StatusNoContent) {
			t.Fatalf("lease on B: %v %s %s", err, res.Status(), res.Body)
		}
		lease = res.JSON200
		return lease != nil
	})
	task, wiID := lease.Task.Metadata.ID, lease.Task.Metadata.WorkItem
	if hb, err := cb.HeartbeatLeaseWithResponse(ctx, lease.Lease.Id, apiclient.HeartbeatRequest{}, agent); err != nil || hb.JSON200 == nil {
		t.Fatalf("heartbeat on B: %v", err)
	}
	done, err := cb.CompleteLeaseWithResponse(ctx, lease.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{
		Task: task, Status: v1alpha1.ResultDone, Outcome: "decomposed", Summary: "two sub-issues",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data: map[string]any{"plan": []any{
			map[string]any{"issue": "compliance-framework/docs#43", "title": "Translate the intro"},
			map[string]any{"issue": "compliance-framework/docs#44", "title": "Translate the guides"},
		}},
	}}, agent)
	if err != nil || done.StatusCode() != http.StatusNoContent {
		t.Fatalf("complete on B: %v %s %s", err, done.Status(), done.Body)
	}
	eventuallyT(t, "the task Succeeded, seen from A", func() bool {
		gt, err := ca.GetTaskWithResponse(ctx, task, cli)
		return err == nil && gt.JSON200 != nil && gt.JSON200.Status.Phase == v1alpha1.TaskSucceeded
	})
	// Only the leader (A) normalised the delivery and ran the rules.
	if strings.Contains(b.logs.String(), `"msg":"delivery normalised"`) || !strings.Contains(a.logs.String(), `"msg":"delivery normalised"`) {
		t.Errorf("the delivery wasn't normalised on the leader alone")
	}

	// 2. A /stream resumed on C, which is behind A, from A's Last-Event-ID.
	resumed := openStream(t, lagging.public, created.id)
	if f := resumed.next("task.updated"); rv(f.id) <= rv(created.id) {
		t.Fatalf("resumed on C at %s, after %s", f.id, created.id)
	}

	// 3. A login started on A finishes on B; its state can't be used twice.
	state, stateCookie := beginLogin(t, a.public)
	session := finishLogin(t, b.public, state, stateCookie, http.StatusFound)
	me, err := ca.GetMeWithResponse(ctx, func(_ context.Context, r *http.Request) error {
		r.AddCookie(session)
		return nil
	})
	if err != nil || me.JSON200 == nil || me.JSON200.Login != "gus" {
		t.Fatalf("the session made on B, on A: %v %s %s", err, me.Status(), me.Body)
	}
	finishLogin(t, a.public, state, stateCookie, http.StatusBadRequest)

	// 4. A change on GitHub while A leads (A resyncs hourly). A stops and B takes over; B's
	// first resync compares with the baselines A stored.
	eventuallyT(t, "A's resync baselines stored", func() bool {
		v, err := readBucket(ctx, sh.dir)
		return err == nil && v.Get(ctx, store.Key{Kind: controllers.KindLinkObservations, Name: wiID}, &controllers.LinkObservations{}) == nil
	})
	if err := adapter.Conn(ghProvider).Update(docsIssue, func(o *provider.Observation) {
		o.Labels = append(o.Labels, "help-wanted")
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.stop(); err != nil {
		t.Fatalf("A's shutdown: %v", err)
	}
	if !strings.Contains(a.logs.String(), `"msg":"leader: released the leader lock"`) {
		t.Errorf("A didn't release the leader lock:\n%s", a.logs)
	}
	eventuallyT(t, "B to take over", func() bool { return strings.Contains(b.logs.String(), leaderLog) })
	eventuallyT(t, "B's first resync to report the change", func() bool {
		return strings.Contains(b.logs.String(), `"msg":"work item resynced"`)
	})
	for _, s := range []*served{b, lagging} {
		if err := s.stop(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}
	if strings.Contains(lagging.logs.String(), leaderLog) {
		t.Error("C led while A or B did")
	}
}

// filestoreOpen opens a P2 state dir, as serve --state-dir does.
func filestoreOpen(dir string) (*filestore.Store, error) {
	return filestore.Open(dir, store.Options{Logger: slog.New(slog.DiscardHandler)}, fileStoreKinds()...)
}

// readBucket reads the state in a file:// bucket, read-only.
func readBucket(ctx context.Context, dir string) (*objectstore.View, error) {
	b, err := objectstore.NewFileBucket(dir)
	if err != nil {
		return nil, err
	}
	return objectstore.Read(ctx, b, store.Options{Logger: slog.New(slog.DiscardHandler)}, objectStoreKinds()...)
}

// sseStream is an open /stream on one dispatcher, as the CLI.
type sseStream struct {
	t      *testing.T
	frames chan sseEvent
}

type sseEvent struct{ event, id string }

func openStream(t *testing.T, base, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+server.BasePath+"/stream?topics=tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testCLIToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the cleanup: the stream outlives this function
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = res.Body.Close()
	})
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("stream (Last-Event-ID %q): %s %s", lastEventID, res.Status, b)
	}
	s := &sseStream{t: t, frames: make(chan sseEvent, 64)}
	go func() {
		defer close(s.frames)
		r := bufio.NewReader(res.Body)
		var f sseEvent
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				if f.event != "" {
					s.frames <- f
				}
				f = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			}
		}
	}()
	return s
}

// next returns the next frame of event.
func (s *sseStream) next(event string) sseEvent {
	s.t.Helper()
	deadline := time.After(serveWait)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				s.t.Fatal("the stream ended")
			}
			if f.event == event {
				return f
			}
		case <-deadline:
			s.t.Fatalf("timed out waiting for a %s frame", event)
		}
	}
}

func rv(id string) uint64 {
	var v uint64
	for _, c := range id {
		v = v*10 + uint64(c-'0')
	}
	return v
}

// noRedirects is a client that returns redirects instead of following them.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// beginLogin starts a UI login on base: the state GitHub would get, and its cookie.
func beginLogin(t *testing.T, base string) (string, *http.Cookie) {
	t.Helper()
	res, err := noRedirects.Get(base + server.BasePath + "/auth/login?returnTo=/board")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	loc, err := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("login: %s %v", res.Status, err)
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.StateCookie {
			return loc.Query().Get("state"), c
		}
	}
	t.Fatal("login set no state cookie")
	return "", nil
}

// finishLogin sends GitHub's callback to base and returns the session cookie it sets.
func finishLogin(t *testing.T, base, state string, stateCookie *http.Cookie, want int) *http.Cookie {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+server.BasePath+"/auth/callback?code=c1&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: stateCookie.Name, Value: stateCookie.Value})
	res, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != want {
		t.Fatalf("callback on %s: %s %s, want %d", base, res.Status, b, want)
	}
	if want == http.StatusBadRequest && !strings.Contains(string(b), server.CodeStateMismatch) {
		t.Fatalf("callback on %s: %s, want %s", base, b, server.CodeStateMismatch)
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	if want == http.StatusFound {
		t.Fatal("the callback set no session cookie")
	}
	return nil
}

// A file:// bucket and a P2 state dir never share a directory: serve refuses --state-url on a
// state dir and --state-dir on a bucket, and never prints the state URL.
func TestServeStateURLRefusals(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	stateDir := stateDirPath(t)
	fs, err := filestoreOpen(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "http://127.0.0.1:1")
	cfg.StateURL, cfg.StateMemory = "file://"+stateDir, false
	if err := runServe(ctx, cfg, serveDeps{Getenv: getenvOf(testEnv())}, log); err == nil ||
		!strings.Contains(err.Error(), "--state-url") || !strings.Contains(err.Error(), "P2 state dir") {
		t.Errorf("--state-url on a state dir: %v", err)
	}

	bucketDir := filepath.Join(t.TempDir(), "bucket")
	b, err := objectstore.NewFileBucket(bucketDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := objectstore.Open(ctx, b, store.Options{Logger: log}, objectStoreKinds()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, &v1alpha1.Agent{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent}, Metadata: v1alpha1.ObjectMeta{Name: "agt_1"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = testConfig(t, "http://127.0.0.1:1")
	cfg.StateDir, cfg.StateMemory = bucketDir, false
	if err := runServe(ctx, cfg, serveDeps{Getenv: getenvOf(testEnv())}, log); err == nil ||
		!strings.Contains(err.Error(), "--state-dir") || !strings.Contains(err.Error(), "file:// bucket of an object store") {
		t.Errorf("--state-dir on a bucket: %v", err)
	}

	// A URL that can't be opened is refused without printing it.
	cfg = testConfig(t, "http://127.0.0.1:1")
	cfg.StateURL, cfg.StateMemory = "gs://bucket/prefix?token=hunter2-secret", false
	if err := runServe(ctx, cfg, serveDeps{Getenv: getenvOf(testEnv())}, log); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("an unusable --state-url: %v", err)
	}
}
