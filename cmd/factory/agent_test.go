package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgent(t *testing.T) {
	env := map[string]string{envCLIToken: testCLIToken}
	const token = "enr_TestTokenTestTokenTestTokenTestTokenTest0"
	var method, path, query, body, auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, query, body, auth = r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Authorization")
		if r.Method != http.MethodDelete {
			w.Header().Set("Content-Type", "application/json")
		}
		switch {
		case strings.Contains(r.URL.Path, "enl_unknown"):
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"code":"not-found"}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"enrollment":{"id":"enl_01","pool":"local","owner":"gus","name":"mac","createdAt":"2026-10-27T09:00:00Z",` +
				`"expiresAt":"2026-11-03T09:00:00Z","createdBy":"factory:cli","usedAt":null,"agent":null},"token":"` + token + `"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Query().Get("cursor") == "":
			_, _ = w.Write([]byte(`{"items":[{"id":"enl_01","pool":"local","owner":"gus","name":"mac","createdAt":"2026-10-27T09:00:00Z","expiresAt":"2026-11-03T09:00:00Z","createdBy":"factory:cli","usedAt":null,"agent":null}],"nextCursor":"c2"}`))
		default:
			_, _ = w.Write([]byte(`{"items":[{"id":"enl_02","pool":"local","owner":"ana","name":"box","createdAt":"2026-10-27T09:00:00Z","expiresAt":"2026-11-03T09:00:00Z","createdBy":"factory:cli","usedAt":null,"agent":null}]}`))
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
		{name: "enroll", args: []string{"enroll", "--pool", "local", "--owner", "gus", "--name", "mac", "--ttl", "3d", "--server", ts.URL}, code: 0,
			method: http.MethodPost, path: "/api/v1/enrollments", in: `"ttl":"3d"`, stdout: token + "\n", stderr: "enrollment enl_01 for agt_gus-mac in pool local"},
		{name: "enroll needs the fields", args: []string{"enroll", "--pool", "local", "--server", ts.URL}, code: exitUsage, stderr: "--pool, --owner and --name are required"},
		{name: "enroll with a bad ttl", args: []string{"enroll", "--pool", "local", "--owner", "gus", "--name", "mac", "--ttl", "soon"}, code: exitUsage, stderr: "--ttl"},
		{name: "list, every page", args: []string{"enrollments", "--server", ts.URL}, code: 0,
			method: http.MethodGet, path: "/api/v1/enrollments", query: "cursor=c2", stdout: `"id": "enl_02"`},
		{name: "list by name", args: []string{"enrollments", "list", "--server", ts.URL}, code: 0, stdout: `"id": "enl_01"`},
		{name: "delete", args: []string{"enrollments", "delete", "enl_01", "--server", ts.URL}, code: 0,
			method: http.MethodDelete, path: "/api/v1/enrollments/enl_01", stderr: "revoked enl_01"},
		{name: "delete unknown", args: []string{"enrollments", "delete", "enl_unknown", "--server", ts.URL}, code: 1, stderr: "404"},
		{name: "delete needs an id", args: []string{"enrollments", "delete", "--server", ts.URL}, code: exitUsage, stderr: "the enrollment id is required"},
		{name: "unknown subcommand", args: []string{"register"}, code: exitUsage, stderr: "usage: factory agent enroll"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, path, query, body, auth = "", "", "", "", ""
			var out, errOut bytes.Buffer
			if code := runAgent(context.Background(), tt.args, getenvOf(env), &out, &errOut); code != tt.code {
				t.Fatalf("exit %d, want %d: %s", code, tt.code, errOut.String())
			}
			if tt.method != "" && (method != tt.method || auth != "Bearer "+testCLIToken) {
				t.Errorf("method %q auth %q, want %q with the CLI token", method, auth, tt.method)
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
			if !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
				t.Errorf("stdout %q (want %q), stderr %q (want %q)", out.String(), tt.stdout, errOut.String(), tt.stderr)
			}
			if strings.Contains(errOut.String(), token) {
				t.Errorf("the token reached stderr")
			}
		})
	}
}
