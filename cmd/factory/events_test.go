package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apiclient"
)

func TestEventsReplay(t *testing.T) {
	env := map[string]string{envCLIToken: testCLIToken}
	var gotPath, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "gh-nope") {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"code":"not-found","detail":"no event \"github-public/gh-nope\""}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"event":"github-public/gh-1","workItem":"wi_1","decisions":[{"rule":"plan-maintainer-epics","fired":true,"replay":true}]}`))
	}))
	defer ts.Close()

	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		code     int
		path     string
		stdout   string
		stderrIn string
	}{
		{name: "prints the result", args: []string{"replay", "github-public/gh-1", "--server", ts.URL}, env: env, code: 0,
			path: "/api/v1/events/github-public%2Fgh-1/replay", stdout: `"rule": "plan-maintainer-epics"`},
		{name: "flags first", args: []string{"replay", "--server", ts.URL, "github-public/gh-1"}, env: env, code: 0,
			path: "/api/v1/events/github-public%2Fgh-1/replay", stdout: `"workItem": "wi_1"`},
		{name: "a problem", args: []string{"replay", "github-public/gh-nope", "--server", ts.URL}, env: env, code: 1,
			stderrIn: "404 Not Found: Not Found (not-found): no event"},
		{name: "no id", args: []string{"replay", "--server", ts.URL}, env: env, code: exitUsage, stderrIn: "the event id is required"},
		{name: "no subcommand", args: nil, env: env, code: exitUsage, stderrIn: "usage: factory events replay"},
		{name: "no token", args: []string{"replay", "x", "--server", ts.URL}, env: map[string]string{}, code: exitUsage, stderrIn: envCLIToken + " is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotAuth = "", ""
			var out, errOut bytes.Buffer
			if code := runEvents(context.Background(), tt.args, getenvOf(tt.env), &out, &errOut); code != tt.code {
				t.Fatalf("exit %d, want %d: %s", code, tt.code, errOut.String())
			}
			if tt.path != "" && gotPath != tt.path {
				t.Errorf("path %q, want %q", gotPath, tt.path)
			}
			if tt.code == 0 {
				if gotAuth != "Bearer "+testCLIToken {
					t.Errorf("authorization %q", gotAuth)
				}
				var res apiclient.ReplayResult
				if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Decisions) != 1 || !res.Decisions[0].Replay {
					t.Errorf("stdout %q: %v", out.String(), err)
				}
			}
			if !strings.Contains(out.String(), tt.stdout) {
				t.Errorf("stdout %q, want %q in it", out.String(), tt.stdout)
			}
			if !strings.Contains(errOut.String(), tt.stderrIn) {
				t.Errorf("stderr %q, want %q in it", errOut.String(), tt.stderrIn)
			}
		})
	}
}
