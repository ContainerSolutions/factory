package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
)

// enroll mints an enrollment with the CLI token and returns it and its token.
func (e *env) enroll(owner, name string, ttl string) (apiclient.Enrollment, string) {
	e.t.Helper()
	req := apiclient.EnrollmentRequest{Pool: "local", Owner: owner, Name: name}
	if ttl != "" {
		d := v1alpha1.Duration(ttl)
		req.Ttl = &d
	}
	res, err := e.c.CreateEnrollmentWithResponse(e.ctx, req, bearer(cliToken))
	if err != nil || res.JSON201 == nil {
		e.t.Fatalf("enroll %s/%s: %v %s %s", owner, name, err, res.Status(), res.Body)
	}
	return res.JSON201.Enrollment, res.JSON201.Token
}

// registerWith registers host in the local pool with one registration header.
func (e *env) registerWith(host, headerName, tok string) (*apiclient.RegisterAgentResponse, error) {
	return e.c.RegisterAgentWithResponse(e.ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: host, AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}}}},
	}, header(headerName, tok))
}

// TestEnrollment: an enrollment token registers its agent once, as the enrollment's owner,
// and returns a refresh token; each refresh token registers once and is replaced by the next;
// an admin's deregistration revokes it.
func TestEnrollment(t *testing.T) {
	e := newEnv(t)
	en, tok := e.enroll("eve", "mac3", "")
	if en.CreatedBy != "factory:cli" || en.UsedAt != nil || en.Agent != nil || !en.ExpiresAt.Equal(t0.Add(7*24*time.Hour)) {
		t.Errorf("enrollment %+v", en)
	}
	// The record keeps only the hash.
	var stored scheduler.Enrollment
	if err := e.st.Get(e.ctx, store.Key{Kind: scheduler.KindEnrollment, Name: en.Id}, &stored); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(stored); strings.Contains(string(raw), tok[4:]) {
		t.Errorf("the stored enrollment holds the token: %s", raw)
	}

	// Another host or pool is a 400 and doesn't spend it.
	r, err := e.registerWith("mac4", HeaderEnrollmentToken, tok)
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "register another host", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeInvalid)

	r, err = e.registerWith("mac3", HeaderEnrollmentToken, tok)
	if err != nil || r.JSON201 == nil || r.JSON201.RefreshToken == nil {
		t.Fatalf("register with the enrollment: %v %s", err, r.Body)
	}
	// eve isn't in the pool's owners Group: the admin vouched for her.
	agent := r.JSON201.AgentId
	if agent != "agt_eve-mac3" {
		t.Errorf("agent %s", agent)
	}
	var a v1alpha1.Agent
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindAgent, Name: agent}, &a); err != nil || a.Spec.Owner != ghProvider+":eve" {
		t.Errorf("agent owner %q, %v", a.Spec.Owner, err)
	}
	if _, err := e.c.UpdateAgentWithResponse(e.ctx, agent, apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentReady)}, bearer(r.JSON201.AgentToken)); err != nil {
		t.Fatal(err)
	}
	ls, _ := e.c.ListEnrollmentsWithResponse(e.ctx, &apiclient.ListEnrollmentsParams{}, bearer(cliToken))
	if ls.JSON200 == nil || len(ls.JSON200.Items) != 1 || ls.JSON200.Items[0].Agent == nil || *ls.JSON200.Items[0].Agent != agent {
		t.Errorf("list after use: %s", ls.Body)
	}

	// Spent.
	r2, _ := e.registerWith("mac3", HeaderEnrollmentToken, tok)
	wantProblem(t, "the enrollment again", r2.HTTPResponse, r2.Body, http.StatusUnauthorized, CodeUnauthenticated)

	// The refresh token rotates.
	first := *r.JSON201.RefreshToken
	r3, err := e.registerWith("mac3", HeaderAgentRefreshToken, first)
	if err != nil || r3.JSON201 == nil || r3.JSON201.RefreshToken == nil || *r3.JSON201.RefreshToken == first || r3.JSON201.AgentId != agent {
		t.Fatalf("refresh: %v %s", err, r3.Body)
	}
	r4, _ := e.registerWith("mac3", HeaderAgentRefreshToken, first)
	wantProblem(t, "the spent refresh token", r4.HTTPResponse, r4.Body, http.StatusUnauthorized, CodeUnauthenticated)
	second := *r3.JSON201.RefreshToken
	r5, _ := e.registerWith("mac4", HeaderAgentRefreshToken, second)
	wantProblem(t, "the refresh token on another host", r5.HTTPResponse, r5.Body, http.StatusBadRequest, CodeInvalid)

	// The agent's own deregistration keeps the refresh token; an admin's revokes it.
	if d, err := e.c.DeregisterAgentWithResponse(e.ctx, agent, bearer(r3.JSON201.AgentToken)); err != nil || d.StatusCode() != http.StatusNoContent {
		t.Fatalf("own deregister: %v %s", err, d.Body)
	}
	r6, err := e.registerWith("mac3", HeaderAgentRefreshToken, second)
	if err != nil || r6.JSON201 == nil {
		t.Fatalf("refresh after its own deregistration: %v %s", err, r6.Body)
	}
	if d, err := e.c.DeregisterAgentWithResponse(e.ctx, agent, session(e.login("gus"))); err != nil || d.StatusCode() != http.StatusNoContent {
		t.Fatalf("admin deregister: %v %s", err, d.Body)
	}
	r7, _ := e.registerWith("mac3", HeaderAgentRefreshToken, *r6.JSON201.RefreshToken)
	wantProblem(t, "refresh after an admin's deregistration", r7.HTTPResponse, r7.Body, http.StatusUnauthorized, CodeUnauthenticated)
}

// TestEnrollmentRefused: expired and deleted enrollments, and two credentials at once, don't
// register; createEnrollment checks its fields; only admins mint, list and delete.
func TestEnrollmentRefused(t *testing.T) {
	e := newEnv(t)
	_, expiring := e.enroll("gus", "mac3", "1h")
	e.clk.Advance(time.Hour)
	r, _ := e.registerWith("mac3", HeaderEnrollmentToken, expiring)
	wantProblem(t, "an expired enrollment", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)

	del, deleted := e.enroll("gus", "mac4", "")
	if d, err := e.c.DeleteEnrollmentWithResponse(e.ctx, del.Id, bearer(cliToken)); err != nil || d.StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %v %s", err, d.Body)
	}
	r, _ = e.registerWith("mac4", HeaderEnrollmentToken, deleted)
	wantProblem(t, "a deleted enrollment", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)

	_, tok := e.enroll("gus", "mac5", "")
	body := []byte(`{"pool":"local","host":"mac5","agentVersion":"1","capabilities":{"harnesses":[{"name":"codex"}]}}`)
	res, b := e.do(http.MethodPost, BasePath+"/agents", body, func(r *http.Request) {
		r.Header.Set(HeaderEnrollmentToken, tok)
		r.Header.Set(HeaderJoinToken, joinSecret)
		r.Header.Set(HeaderOwnerToken, "tok-gus")
	})
	wantProblemReply(t, "two credentials", res, b, http.StatusUnauthorized, CodeUnauthenticated)
	for _, f := range []string{HeaderEnrollmentToken, HeaderAgentRefreshToken} {
		r, _ := e.registerWith("mac5", f, "malformed")
		wantProblem(t, "a malformed "+f, r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
	}

	gus, rita := e.login("gus"), e.login("rita")
	for _, c := range []struct {
		name string
		req  apiclient.EnrollmentRequest
	}{
		{"a ttl over 30d", apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: "x", Ttl: ptr(v1alpha1.Duration("31d"))}},
		{"a bad ttl", apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: "x", Ttl: ptr(v1alpha1.Duration("soon"))}},
		{"an unknown pool", apiclient.EnrollmentRequest{Pool: "nope", Owner: "gus", Name: "x"}},
		{"a bad owner", apiclient.EnrollmentRequest{Pool: "local", Owner: "gus carvalho", Name: "x"}},
		{"a bad name", apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: "Mac"}},
	} {
		res, err := e.c.CreateEnrollmentWithResponse(e.ctx, c.req, session(gus))
		if err != nil {
			t.Fatal(err)
		}
		wantProblem(t, "createEnrollment with "+c.name, res.HTTPResponse, res.Body, http.StatusBadRequest, CodeInvalid)
	}
	ce, _ := e.c.CreateEnrollmentWithResponse(e.ctx, apiclient.EnrollmentRequest{Pool: "local", Owner: "gus", Name: "x"}, session(rita))
	wantProblem(t, "a reader's createEnrollment", ce.HTTPResponse, ce.Body, http.StatusForbidden, CodeNotAdmin)
	le, _ := e.c.ListEnrollmentsWithResponse(e.ctx, &apiclient.ListEnrollmentsParams{}, session(rita))
	wantProblem(t, "a reader's listEnrollments", le.HTTPResponse, le.Body, http.StatusForbidden, CodeNotAdmin)
	de, _ := e.c.DeleteEnrollmentWithResponse(e.ctx, del.Id, session(rita))
	wantProblem(t, "a reader's deleteEnrollment", de.HTTPResponse, de.Body, http.StatusForbidden, CodeNotAdmin)
	de, _ = e.c.DeleteEnrollmentWithResponse(e.ctx, "enl_unknown", session(gus))
	wantProblem(t, "deleting an unknown enrollment", de.HTTPResponse, de.Body, http.StatusNotFound, CodeNotFound)
}
