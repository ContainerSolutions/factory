package policy

import (
	"net/http"
	"slices"
	"testing"
)

// TestRefusals has a row per code of gateway.md's "Refusals and errors" table: its status, its
// header and its body, written out here rather than derived from the package's table.
func TestRefusals(t *testing.T) {
	tests := []struct {
		code   Code
		status int    // 0: no answer, the connection is closed
		body   string // after "factory gateway: ", without the newline
	}{
		{CodeNotProxy, 400, "not a proxy request"},
		{CodeBadPath, 400, "refused: the path is not canonical"},
		{CodeHostMismatch, 400, "refused: the Host header does not match the CONNECT"},
		{CodeBadTarget, 400, "refused: the CONNECT target is not host:port"},
		{CodePort, 403, "refused: this host is served on port 443 only"},
		{CodePlainHTTP, 403, "refused: plain HTTP to an intercepted host"},
		{CodePlainOnly, 403, "refused: the dispatcher is served over plain HTTP through the proxy"},
		{CodeAuthHost, 403, "refused: auth.openai.com is not reachable from workspaces (the gateway refreshes the login)"},
		{CodePushRefused, 403, "push refused: the repository is not writable for this task"},
		{CodeWebWrite, 403, "refused: github.com takes only GET and HEAD outside git"},
		{CodeAPIWrite, 403, "refused: api.github.com is read-only for workspaces"},
		{CodeDispatcherRoute, 403, "refused: the dispatcher route is not open to workspaces"},
		{CodeModelPath, 403, "refused: this path on a model host is not open to workspaces"},
		{CodeEgressNotAllowed, 403, "refused: the task's egress policy does not allow this host"},
		{CodeEgressDenied, 403, "refused: the destination is a denied address (private)"},
		{CodeResolve, 502, "the destination's name does not resolve"},
		{CodeUpstream, 502, "the upstream is unreachable"},
		{CodeTimeout, 504, "the upstream timed out"},
		{CodeClosing, 503, "the gateway is closing"},
		{CodeNoLogin, 503, "no model login for this host on this agent host"},
		{CodeSNIMismatch, 0, ""},
		{CodeNotTLS, 0, ""},
		{CodePeer, 0, ""},
	}
	var seen []Code
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if got := tt.code.Status(); got != tt.status {
				t.Errorf("Status() = %d, want %d", got, tt.status)
			}
			r, ok := tt.code.Response(DenyPrivate)
			if ok != (tt.status != 0) {
				t.Fatalf("Response ok = %v, want %v", ok, tt.status != 0)
			}
			if !ok {
				return
			}
			want := "factory gateway: " + tt.body + "\n"
			if r.Status != tt.status || r.Body != want {
				t.Errorf("Response = %d %q, want %d %q", r.Status, r.Body, tt.status, want)
			}
			if got := r.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := r.Header.Values("X-Factory-Gateway-Refusal"); !slices.Equal(got, []string{string(tt.code)}) {
				t.Errorf("X-Factory-Gateway-Refusal = %q, want %q", got, tt.code)
			}
			if len(r.Header) != 2 {
				t.Errorf("Header = %v, want only Content-Type and the refusal", r.Header)
			}
		})
		seen = append(seen, tt.code)
	}
	if got := Codes(); !slices.Equal(got, seen) {
		t.Errorf("Codes() = %v, want the table's %v", got, seen)
	}
}

func TestEgressDeniedBodies(t *testing.T) {
	for _, c := range DenyClasses() {
		r, ok := CodeEgressDenied.Response(c)
		want := "factory gateway: refused: the destination is a denied address (" + string(c) + ")\n"
		if !ok || r.Status != http.StatusForbidden || r.Body != want {
			t.Errorf("Response(%s) = %v %d %q, want %q", c, ok, r.Status, r.Body, want)
		}
	}
	for _, c := range []DenyClass{"", "10.0.0.1", "Private"} {
		if _, ok := CodeEgressDenied.Response(c); ok {
			t.Errorf("Response(%q) built a body for a class outside the set", c)
		}
	}
	if r, ok := CodePort.Response("10.0.0.1"); !ok || r.Body != "factory gateway: refused: this host is served on port 443 only\n" {
		t.Errorf("another code's body used the class: %q", r.Body)
	}
	if _, ok := Code("nope").Response(""); ok || Code("nope").Status() != 0 {
		t.Error("an unknown code got an answer")
	}
	r1, _ := CodePort.Response("")
	r1.Header.Set(RefusalHeader, "changed")
	if r2, _ := CodePort.Response(""); r2.Header.Get(RefusalHeader) != string(CodePort) {
		t.Error("Response shares its Header between calls")
	}
}
