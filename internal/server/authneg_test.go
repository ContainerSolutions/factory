package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
)

// reply is a raw response, its body read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

// replyOf is a generated client's response as a reply (its body is already read and closed).
func replyOf(h *http.Response) reply {
	if h == nil {
		return reply{}
	}
	return reply{StatusCode: h.StatusCode, Header: h.Header}
}

// wantProblemReply is wantProblem for a raw reply.
func wantProblemReply(t *testing.T, what string, r reply, body []byte, status int, code string) {
	t.Helper()
	checkProblem(t, what, r.StatusCode, r.Header, body, status, code)
}

// do sends a raw request to the public server.
func (e *env) do(method, path string, body []byte, edit ...func(*http.Request)) (reply, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, f := range edit {
		f(req)
	}
	res, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return reply{StatusCode: res.StatusCode, Header: res.Header}, b
}

func withBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withSession(v string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: v}) }
}

// concrete turns a spec path template into a request path.
func concrete(p string) string {
	r := strings.NewReplacer("{id}", "x", "{n}", "0", "{kind}", "TaskSpec", "{name}", "implement")
	return r.Replace(p)
}

// TestAuthRequired: every operation but the public ones refuses a request without
// credentials, and one with a wrong credential, with a 401 problem.
func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	for id, o := range spec(t) {
		acc, ok := operations[id]
		if !ok || acc == public {
			continue
		}
		path := concrete(o.Path)
		if id == "streamEvents" {
			path += "?topics=tasks"
		}
		var body []byte
		if o.Method != http.MethodGet && o.Method != http.MethodDelete {
			body = []byte("{}")
		}
		t.Run(id, func(t *testing.T) {
			res, b := e.do(o.Method, path, body)
			wantProblemReply(t, id+" without credentials", res, b, http.StatusUnauthorized, CodeUnauthenticated)

			var wrong []func(*http.Request)
			switch acc {
			case agentOnly:
				wrong = []func(*http.Request){withBearer("agt_nobody-x.wrong"), withBearer(cliToken), withSession("forged.cookie")}
			case agentOrRun:
				wrong = []func(*http.Request){withBearer("agt_nobody-x.wrong"), withBearer(fakeRunToken), withBearer(cliToken), withSession("forged.cookie")}
			case readerAgentOrRun:
				wrong = []func(*http.Request){withSession("forged.cookie"), withBearer("agt_nobody-x.wrong"), withBearer(fakeRunToken)}
			case reader, admin, sessionOnly, agentOrAdmin:
				wrong = []func(*http.Request){withSession("forged.cookie"), withBearer("not-the-cli-token"), withBearer("agt_nobody-x.wrong")}
			case readerOrAgent:
				wrong = []func(*http.Request){withSession("forged.cookie"), withBearer("agt_nobody-x.wrong")}
			case registration:
				wrong = []func(*http.Request){func(r *http.Request) { r.Header.Set(HeaderJoinToken, "join-secret") }}
			}
			for i, w := range wrong {
				res, b := e.do(o.Method, path, body, w)
				wantProblemReply(t, id+" with a wrong credential "+string(rune('a'+i)), res, b, http.StatusUnauthorized, "")
			}
		})
	}
	// The join and owner tokens are refused, whether or not they're right; an enrollment token
	// the dispatcher didn't mint is refused by the scheduler.
	body, _ := json.Marshal(apiclient.RegisterAgentRequest{Pool: "local", Host: "mac1", AgentVersion: "1",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}}})
	for name, edit := range map[string]func(*http.Request){
		"the join and owner tokens": func(r *http.Request) {
			r.Header.Set(HeaderJoinToken, "join-secret")
			r.Header.Set(HeaderOwnerToken, "tok-gus")
		},
		"the owner token alone":       func(r *http.Request) { r.Header.Set(HeaderOwnerToken, "tok-gus") },
		"an unknown enrollment token": func(r *http.Request) { r.Header.Set(HeaderEnrollmentToken, "enr_"+strings.Repeat("x", 43)) },
		"an unknown refresh token":    func(r *http.Request) { r.Header.Set(HeaderAgentRefreshToken, "arf_"+strings.Repeat("x", 43)) },
	} {
		res, b := e.do(http.MethodPost, BasePath+"/agents", body, edit)
		wantProblemReply(t, "register with "+name, res, b, http.StatusUnauthorized, CodeUnauthenticated)
	}
}

// fakeRunToken has the runToken scheme's format (`rtk_` + 43 base64url characters).
var fakeRunToken = "rtk_" + strings.Repeat("x", 43)

// runRoute is one of the routes that take a run token, for a task and its run.
type runRoute struct {
	id, method, path string
	body             []byte
	ctype            string
	ok               int
}

func runRoutes(t *testing.T, task, run string) []runRoute {
	effect := []byte(`{"op":"comment","idempotencyKey":"k-` + task + `","args":{"body":"hi"}}`)
	return []runRoute{
		{"getTaskPrompt", http.MethodGet, BasePath + "/tasks/" + task + "/prompt", nil, "", http.StatusOK},
		{"getTaskOutputSchema", http.MethodGet, BasePath + "/tasks/" + task + "/output-schema", nil, "", http.StatusOK},
		{"getTaskContext", http.MethodGet, BasePath + "/tasks/" + task + "/context/0", nil, "", http.StatusOK},
		{"createEffect", http.MethodPost, BasePath + "/tasks/" + task + "/effects", effect, "application/json", http.StatusOK},
		{"uploadTranscript", http.MethodPut, BasePath + "/runs/" + run + "/transcript", gzipped(t, "{}"), "application/gzip", http.StatusOK},
		{"uploadEnvironmentLog", http.MethodPut, BasePath + "/runs/" + run + "/environment-log", []byte("up\n"), "text/plain", http.StatusOK},
	}
}

func (r runRoute) do(e *env, tok string) (reply, []byte) {
	return e.do(r.method, r.path, r.body, withBearer(tok), func(req *http.Request) {
		if r.ctype != "" {
			req.Header.Set("Content-Type", r.ctype)
		}
	})
}

// TestRunToken: a lease's run token works on the run routes for its own task and run only,
// never on another task's or run's (403, even one its agent holds), never on the agent-only
// and reader routes (401), and not after its lease ends (401).
func TestRunToken(t *testing.T) {
	e := newEnv(t)
	e.cat.Pools["local"].Spec.Lease.MaxTasksPerAgent = 2
	a, aTok := e.register("gus", "mac1")
	if r, err := e.c.UpdateAgentWithResponse(e.ctx, a, apiclient.AgentUpdate{
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 2},
	}, bearer(aTok)); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("update lending: %v %s", err, r.Body)
	}
	e.task("tsk_a", nil)
	la := e.lease(a, aTok)
	e.task("tsk_b", nil)
	lb := e.lease(a, aTok) // the same agent's second lease
	if la.RunToken == nil || !regexp.MustCompile(`^rtk_[A-Za-z0-9_-]{43}$`).MatchString(*la.RunToken) ||
		lb.RunToken == nil || *lb.RunToken == *la.RunToken {
		t.Fatalf("lease run tokens %v, %v", la.RunToken, lb.RunToken)
	}
	tok := *la.RunToken
	// The lease record keeps only the token's hash.
	var stored scheduler.Lease
	if err := e.st.Get(e.ctx, store.Key{Kind: scheduler.KindLease, Name: la.Lease.Id}, &stored); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(stored); strings.Contains(string(raw), tok[len(scheduler.RunTokenPrefix):]) || !strings.HasPrefix(stored.Spec.RunTokenHash, "sha256:") {
		t.Errorf("the stored lease %s", raw)
	}

	own, other := runRoutes(t, "tsk_a", *la.Lease.Run), runRoutes(t, "tsk_b", *lb.Lease.Run)
	for i, rt := range own {
		t.Run(rt.id, func(t *testing.T) {
			if acc := operations[rt.id]; acc != agentOrRun && acc != readerAgentOrRun {
				t.Fatalf("%s's access is %d, not a run-token one", rt.id, acc)
			}
			if res, body := rt.do(e, tok); res.StatusCode != rt.ok {
				t.Errorf("own task: %d %s, want %d", res.StatusCode, body, rt.ok)
			}
			res, body := other[i].do(e, tok)
			wantProblemReply(t, rt.id+" on another task", res, body, http.StatusForbidden, CodeNotLeaseHolder)
			if res, body := other[i].do(e, aTok); res.StatusCode != other[i].ok {
				t.Errorf("the agent token on its other task: %d %s, want %d", res.StatusCode, body, other[i].ok)
			}
		})
	}

	// The routes that don't take it refuse it as a wrong credential.
	agentOnly := []struct{ name, method, path string }{
		{"heartbeatLease", http.MethodPost, BasePath + "/leases/" + la.Lease.Id + "/heartbeat"},
		{"completeLease", http.MethodPost, BasePath + "/leases/" + la.Lease.Id + "/complete"},
		{"abandonLease", http.MethodPost, BasePath + "/leases/" + la.Lease.Id + "/abandon"},
		{"leaseTask", http.MethodGet, BasePath + "/agents/" + a + "/lease?wait=0s"},
		{"updateAgent", http.MethodPatch, BasePath + "/agents/" + a},
		{"deregisterAgent", http.MethodDelete, BasePath + "/agents/" + a},
		{"getTask", http.MethodGet, BasePath + "/tasks/tsk_a"},
		{"getTranscript", http.MethodGet, BasePath + "/runs/" + *la.Lease.Run + "/transcript"},
		{"listTasks", http.MethodGet, BasePath + "/tasks"},
	}
	for _, r := range agentOnly {
		var body []byte
		if r.method == http.MethodPost || r.method == http.MethodPatch {
			body = []byte("{}")
		}
		res, b := e.do(r.method, r.path, body, withBearer(tok))
		wantProblemReply(t, r.name+" with a run token", res, b, http.StatusUnauthorized, CodeUnauthenticated)
	}
	if p := e.getTask("tsk_a").Status; p.Phase != v1alpha1.TaskLeased || *p.Agent != a {
		t.Errorf("tsk_a changed: %+v", p)
	}
}

// TestRunTokenLeaseEnd: a run token stops working once its lease ends, however it ends.
func TestRunTokenLeaseEnd(t *testing.T) {
	ends := []struct {
		name string
		end  func(e *env, l *apiclient.Lease, tok string)
	}{
		{"completed", func(e *env, l *apiclient.Lease, tok string) {
			r, err := e.c.CompleteLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.CompleteRequest{
				Result: v1alpha1.Result{Task: "tsk_a", Status: v1alpha1.ResultDone, Outcome: "pr-opened", Summary: "done"},
			}, bearer(tok))
			if err != nil || r.StatusCode() != http.StatusNoContent {
				e.t.Fatalf("complete: %v %s", err, r.Body)
			}
		}},
		{"abandoned", func(e *env, l *apiclient.Lease, tok string) {
			r, err := e.c.AbandonLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.AbandonRequest{Reason: apiclient.AbandonRequestReasonShutdown}, bearer(tok))
			if err != nil || r.StatusCode() != http.StatusNoContent {
				e.t.Fatalf("abandon: %v %s", err, r.Body)
			}
		}},
		{"expired", func(e *env, l *apiclient.Lease, _ string) {
			e.clk.Advance(l.Lease.ExpiresAt.Sub(e.clk.Now()) + time.Second) // no heartbeat; not yet reconciled
		}},
		{"revoked", func(e *env, _ *apiclient.Lease, _ string) {
			t := e.getTask("tsk_a")
			t.Status.Phase = v1alpha1.TaskFailed // finished by someone else
			if err := e.st.Update(e.ctx, t); err != nil {
				e.t.Fatal(err)
			}
			if _, err := e.sched.Reconcile(e.ctx, "tsk_a"); err != nil {
				e.t.Fatal(err)
			}
		}},
	}
	for _, c := range ends {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			a, aTok := e.register("gus", "mac1")
			e.task("tsk_a", nil)
			l := e.lease(a, aTok)
			rt := runRoutes(t, "tsk_a", *l.Lease.Run)[0]
			if res, b := rt.do(e, *l.RunToken); res.StatusCode != http.StatusOK {
				t.Fatalf("before the end: %d %s", res.StatusCode, b)
			}
			c.end(e, l, aTok)
			res, b := rt.do(e, *l.RunToken)
			wantProblemReply(t, "after the lease "+c.name, res, b, http.StatusUnauthorized, CodeUnauthenticated)
		})
	}
}

// TestAgentIsolation: agent A can't act on agent B, its lease, its task or its run.
func TestAgentIsolation(t *testing.T) {
	e := newEnv(t)
	_, aTok := e.register("gus", "mac1")
	b, bTok := e.register("ana", "mac2")
	e.task("tsk_b", nil)
	lb := e.lease(b, bTok)
	run := *lb.Lease.Run
	ctx := e.ctx

	type call struct {
		name string
		do   func() (reply, []byte, error)
		code string
	}
	wait := "0s"
	calls := []call{
		{"update B", func() (reply, []byte, error) {
			r, err := e.c.UpdateAgentWithResponse(ctx, b, apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentOffline)}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"deregister B", func() (reply, []byte, error) {
			r, err := e.c.DeregisterAgentWithResponse(ctx, b, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"lease for B", func() (reply, []byte, error) {
			r, err := e.c.LeaseTaskWithResponse(ctx, b, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"heartbeat B's lease", func() (reply, []byte, error) {
			r, err := e.c.HeartbeatLeaseWithResponse(ctx, lb.Lease.Id, apiclient.HeartbeatRequest{}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"complete B's lease", func() (reply, []byte, error) {
			r, err := e.c.CompleteLeaseWithResponse(ctx, lb.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{Task: "tsk_b", Status: v1alpha1.ResultDone}}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"abandon B's lease", func() (reply, []byte, error) {
			r, err := e.c.AbandonLeaseWithResponse(ctx, lb.Lease.Id, apiclient.AbandonRequest{Reason: apiclient.AbandonRequestReasonShutdown}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's task", func() (reply, []byte, error) {
			r, err := e.c.GetTaskWithResponse(ctx, "tsk_b", bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's prompt", func() (reply, []byte, error) {
			r, err := e.c.GetTaskPromptWithResponse(ctx, "tsk_b", bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's output schema", func() (reply, []byte, error) {
			r, err := e.c.GetTaskOutputSchemaWithResponse(ctx, "tsk_b", bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's context", func() (reply, []byte, error) {
			r, err := e.c.GetTaskContextWithResponse(ctx, "tsk_b", 0, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"effect on B's task", func() (reply, []byte, error) {
			r, err := e.c.CreateEffectWithResponse(ctx, "tsk_b", apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k"}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"upload B's transcript", func() (reply, []byte, error) {
			r, err := e.c.UploadTranscriptWithBodyWithResponse(ctx, run, "application/gzip", bytes.NewReader(gzipped(t, "{}")), bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's transcript", func() (reply, []byte, error) {
			r, err := e.c.GetTranscriptWithResponse(ctx, run, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
	}
	for _, c := range calls {
		res, body, err := c.do()
		if err != nil {
			t.Fatal(err)
		}
		wantProblemReply(t, c.name, res, body, http.StatusForbidden, c.code)
	}
	// B still holds its lease and its agent is untouched.
	if p := e.getTask("tsk_b").Status; p.Phase != v1alpha1.TaskLeased || *p.Agent != b {
		t.Errorf("B's task changed: %+v", p)
	}
}

// TestUIAccess: readers read, only maintainers (or the CLI) send commands, non-members get
// no session, sessions expire, and cookie-authenticated writes must be same-origin.
func TestUIAccess(t *testing.T) {
	e := newEnv(t)
	rita := e.login("rita") // a reader, not a maintainer
	gus := e.login("gus")
	ctx := e.ctx
	pause := apiclient.Command{Type: apiclient.CommandTypePause, Target: &apiclient.CommandTarget{Scope: ptr(apiclient.CommandTargetScopeGlobal)}}

	if r, err := e.c.ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, session(rita)); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("a reader lists tasks: %v %s", err, r.Status())
	}
	me, _ := e.c.GetMeWithResponse(ctx, session(rita))
	if me.JSON200 == nil || me.JSON200.IsAdmin {
		t.Errorf("rita's me: %s", me.Body)
	}
	pc, err := e.c.PostCommandWithResponse(ctx, pause, session(rita))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a reader's command", pc.HTTPResponse, pc.Body, http.StatusForbidden, CodeNotAdmin)
	rp, err := e.c.ReplayEventWithResponse(ctx, "github-public/gh-1", session(rita))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a reader's replay", rp.HTTPResponse, rp.Body, http.StatusForbidden, CodeNotAdmin)

	// A maintainer's command from another site is refused; from the same origin it's accepted.
	res, b := e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	wantProblemReply(t, "a cross-site command", res, b, http.StatusForbidden, CodeCrossSite)
	res, b = e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	wantProblemReply(t, "a cross-site command without Origin", res, b, http.StatusForbidden, CodeCrossSite)
	res, b = e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Origin", e.ts.URL) })
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("a same-origin command: %d %s", res.StatusCode, b)
	}

	// The CLI token is a full admin.
	if r, err := e.c.PostCommandWithResponse(ctx, pause, bearer(cliToken)); err != nil || r.StatusCode() != http.StatusAccepted {
		t.Fatalf("CLI command: %v %s", err, r.Status())
	}
	if r, err := e.c.ListWorkItemsWithResponse(ctx, &apiclient.ListWorkItemsParams{}, bearer(cliToken)); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("CLI read: %v %s", err, r.Status())
	}
	// An agent token is not a UI credential.
	_, agentTok := e.register("gus", "mac1")
	lw, _ := e.c.ListWorkItemsWithResponse(ctx, &apiclient.ListWorkItemsParams{}, bearer(agentTok))
	wantProblem(t, "an agent reading the UI routes", lw.HTTPResponse, lw.Body, http.StatusUnauthorized, CodeUnauthenticated)

	// eve is in no Group: no session.
	lg, _ := e.c.LoginWithResponse(ctx, &apiclient.LoginParams{})
	state := cookieOf(t, lg.HTTPResponse.Header, auth.StateCookie).Value
	cb, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-eve", State: state},
		func(_ context.Context, r *http.Request) error {
			r.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: state})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a non-member's callback", cb.HTTPResponse, cb.Body, http.StatusForbidden, CodeNotMember)
	if c := cookieOf(t, cb.HTTPResponse.Header, auth.SessionCookie); c != nil {
		t.Error("a non-member got a session cookie")
	}

	// A tampered cookie, then expiry after 12 h.
	tampered := gus[:len(gus)-2] + "xx"
	r, _ := e.c.GetMeWithResponse(ctx, session(tampered))
	wantProblem(t, "a tampered session", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
	e.clk.Advance(12*time.Hour + time.Second)
	r, _ = e.c.GetMeWithResponse(ctx, session(gus))
	wantProblem(t, "an expired session", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
}

// TestOAuthState: the callback's state must match the cookie of the browser that started the
// login, once, before it expires.
func TestOAuthState(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	begin := func() string {
		lg, err := e.c.LoginWithResponse(ctx, &apiclient.LoginParams{})
		if err != nil {
			t.Fatal(err)
		}
		return cookieOf(t, lg.HTTPResponse.Header, auth.StateCookie).Value
	}
	callback := func(state, cookie string) *apiclient.AuthCallbackResponse {
		r, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-gus", State: state},
			func(_ context.Context, r *http.Request) error {
				if cookie != "" {
					r.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: cookie})
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	s1, s2 := begin(), begin()
	for _, c := range []struct{ name, state, cookie string }{
		{"no cookie", s1, ""},
		{"another login's cookie", s1, s2},
		{"a forged state", "forged", "forged"},
	} {
		r := callback(c.state, c.cookie)
		wantProblem(t, c.name, r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	}
	if r := callback(s1, s1); r.StatusCode() != http.StatusFound {
		t.Fatalf("the matching state: %s %s", r.Status(), r.Body)
	}
	r := callback(s1, s1)
	wantProblem(t, "a state used twice", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	e.clk.Advance(11 * time.Minute)
	r = callback(s2, s2)
	wantProblem(t, "an expired state", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	// A code GitHub doesn't know.
	s3 := begin()
	r, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-nobody", State: s3},
		func(_ context.Context, req *http.Request) error {
			req.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: s3})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "an unknown code", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
}

// TestOpenRedirect: returnTo must be a same-origin path.
func TestOpenRedirect(t *testing.T) {
	e := newEnv(t)
	for _, rt := range []string{"//evil.example", "https://evil.example/", "/\\evil.example", "javascript:alert(1)", "evil", "/ok\r\nSet-Cookie: x=y", "/%0d%0a"} {
		lg, err := e.c.LoginWithResponse(e.ctx, &apiclient.LoginParams{ReturnTo: ptr(rt)})
		if err != nil {
			t.Fatal(err)
		}
		if rt == "/%0d%0a" {
			// Encoded, it's a plain path: allowed, and it stays a path.
			if lg.StatusCode() != http.StatusFound {
				t.Errorf("returnTo %q: %s", rt, lg.Status())
			}
			continue
		}
		wantProblem(t, "returnTo "+rt, lg.HTTPResponse, lg.Body, http.StatusBadRequest, CodeBadReturnTo)
	}
}

// TestWebhook: deliveries go to the provider's handler, which checks the signature before the
// sink stores them.
func TestWebhook(t *testing.T) {
	e := newEnv(t)
	body, err := json.Marshal(fake.Webhook{Delivery: "d-1", Type: "issue.opened", Repo: docsRepo, Sender: fake.Sender{Login: "octocat"}})
	if err != nil {
		t.Fatal(err)
	}
	res, b := e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256=00") })
	if res.StatusCode != http.StatusUnauthorized || e.sink.count() != 0 {
		t.Fatalf("a bad signature: %d %s, %d stored", res.StatusCode, b, e.sink.count())
	}
	res, b = e.do(http.MethodPost, "/hooks/"+ghProvider, body)
	if res.StatusCode != http.StatusUnauthorized || e.sink.count() != 0 {
		t.Fatalf("no signature: %d %s", res.StatusCode, b)
	}
	res, b = e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", sign(body)) })
	if res.StatusCode != http.StatusAccepted || e.sink.count() != 1 {
		t.Fatalf("a signed delivery: %d %s, %d stored", res.StatusCode, b, e.sink.count())
	}
	res, _ = e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", sign(body)) })
	if res.StatusCode != http.StatusOK || e.sink.count() != 1 {
		t.Fatalf("a duplicate delivery: %d, %d stored", res.StatusCode, e.sink.count())
	}
	res, b = e.do(http.MethodPost, "/hooks/nope", body)
	wantProblemReply(t, "an unknown provider", res, b, http.StatusNotFound, CodeNotFound)
}
