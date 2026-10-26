package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkitem(t *testing.T) {
	env := map[string]string{envCLIToken: testCLIToken}
	var method, path, query, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, query, body = r.Method, r.URL.Path, r.URL.RawQuery, string(b)
		switch {
		case strings.Contains(r.URL.Path, "wi_leased"):
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"title":"Conflict","status":409,"code":"task-leased","detail":"task tsk_1 is Running"}`))
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"WorkItem","metadata":{"name":"wi_1"},"spec":{},"status":{"closed":{"reason":"done","note":"fixed","at":"2026-10-16T11:05:00Z","by":"cli"}}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"workItem":"wi_1","deleted":{"WorkItem":1,"Task":2}}`))
		}
	}))
	defer ts.Close()

	tests := []struct {
		name                    string
		args                    []string
		code                    int
		method, path, query, in string
		stdout, stderr          string
	}{
		{name: "close", args: []string{"close", "wi_1", "--reason", "done", "--note", "fixed", "--server", ts.URL}, code: 0,
			method: http.MethodPost, path: "/api/v1/workitems/wi_1/close", in: `"reason":"done"`, stdout: `"note": "fixed"`},
		{name: "close needs a reason", args: []string{"close", "wi_1", "--server", ts.URL}, code: exitUsage, stderr: "--reason must be"},
		{name: "delete", args: []string{"delete", "wi_1", "--server", ts.URL}, code: 0,
			method: http.MethodDelete, path: "/api/v1/workitems/wi_1", stdout: `"Task": 2`},
		{name: "delete forced", args: []string{"delete", "--force", "wi_1", "--server", ts.URL}, code: 0,
			method: http.MethodDelete, query: "force=true"},
		{name: "a leased task", args: []string{"delete", "wi_leased", "--server", ts.URL}, code: 1, stderr: "409 Conflict: Conflict (task-leased)"},
		{name: "no id", args: []string{"delete", "--server", ts.URL}, code: exitUsage, stderr: "the work item id is required"},
		{name: "unknown subcommand", args: []string{"reopen", "wi_1"}, code: exitUsage, stderr: "usage: factory workitem close"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, path, query, body = "", "", "", ""
			var out, errOut bytes.Buffer
			if code := runWorkitem(context.Background(), tt.args, getenvOf(env), &out, &errOut); code != tt.code {
				t.Fatalf("exit %d, want %d: %s", code, tt.code, errOut.String())
			}
			if tt.method != "" && method != tt.method {
				t.Errorf("method %q, want %q", method, tt.method)
			}
			if tt.path != "" && path != tt.path {
				t.Errorf("path %q, want %q", path, tt.path)
			}
			if tt.query != "" && query != tt.query {
				t.Errorf("query %q, want %q", query, tt.query)
			}
			if !strings.Contains(body, tt.in) {
				t.Errorf("body %q, want %q in it", body, tt.in)
			}
			if tt.code == 0 && !json.Valid(out.Bytes()) {
				t.Errorf("stdout is not JSON: %q", out.String())
			}
			if !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
				t.Errorf("stdout %q (want %q), stderr %q (want %q)", out.String(), tt.stdout, errOut.String(), tt.stderr)
			}
		})
	}
}
