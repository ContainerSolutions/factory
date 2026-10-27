package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/effector"
	"github.com/ContainerSolutions/factory/internal/agent/gitcred"
	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
	"github.com/ContainerSolutions/factory/internal/agent/pipeline"
)

func TestFxGitCredential(t *testing.T) {
	s, err := gitcred.Listen(t.TempDir(), func() []gitcred.Entry {
		return []gitcred.Entry{{Protocol: "https", Host: "github.com", Repo: "acme/docs", Username: "x-access-token", Password: "tok"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	env := func(k string) string {
		if k == gitcred.EnvSocket {
			return s.Path()
		}
		return ""
	}
	var out, errOut bytes.Buffer
	code := runFx(context.Background(), []string{"git-credential", "get"}, strings.NewReader("protocol=https\nhost=github.com\npath=acme/docs.git\n\n"), &out, &errOut, env)
	if code != 0 || out.String() != "username=x-access-token\npassword=tok\n" {
		t.Errorf("get = %d %q %q", code, out.String(), errOut.String())
	}
	for _, args := range [][]string{nil, {"git-credential"}, {"pr", "open"}} {
		if code := runFx(context.Background(), args, strings.NewReader(""), &out, &errOut, env); code != 2 {
			t.Errorf("runFx %v = %d, want 2", args, code)
		}
	}
	gone := func(string) string { return filepath.Join(t.TempDir(), "nope") }
	if code := runFx(context.Background(), []string{"git-credential", "get"}, strings.NewReader("protocol=https\n"), &out, &errOut, gone); code != 5 {
		t.Errorf("a missing socket = %d, want 5", code)
	}
}

func TestFxClaudeHook(t *testing.T) {
	out := t.TempDir()
	call := func(input string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := runFx(context.Background(), []string{"claude-hook", "pre-tool-use", "--write", out}, strings.NewReader(input), &stdout, &stderr, func(string) string { return "" })
		return code, stderr.String()
	}
	if code, msg := call(`{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(out, "result.json") + `"}}`); code != 0 {
		t.Errorf("a write in out/ = %d %q", code, msg)
	}
	if code, msg := call(`{"tool_name":"Write","tool_input":{"file_path":"/etc/passwd"}}`); code != 2 || !strings.Contains(msg, "may write only under") {
		t.Errorf("a write outside = %d %q", code, msg)
	}
	if code, msg := call(`{"tool_name":"Bash","tool_input":{"command":"git push origin HEAD"}}`); code != 2 || !strings.Contains(msg, "factory push") {
		t.Errorf("git push = %d %q", code, msg)
	}
}

// TestFxEffectOp: `factory-agent fx <op>` is the Effector CLI, configured from the VM
// workspace run's environment: its requests carry no Authorization (the gateway adds it).
func TestFxEffectOp(t *testing.T) {
	var got map[string]any
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"effectId": "eff_1", "url": "https://github.com/acme/docs/issues/42#issuecomment-1"}`)
	}))
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, pipeline.EffectorFile)
	if err := os.WriteFile(cfg, []byte(`{"task": "tsk_1", "repo": "acme/docs", "allow": ["comment"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{pipeline.EnvAPI: ts.URL, pipeline.EnvTaskID: "tsk_1", pipeline.EnvRuntime: pipeline.RuntimeWorkspace,
		pipeline.EnvOut: dir, pipeline.EnvEffector: cfg}
	getenv := func(k string) string { return env[k] }
	var out, errOut bytes.Buffer
	if code := runFx(context.Background(), []string{"comment", "--target", "42", "--body", "hi"}, strings.NewReader(""), &out, &errOut, getenv); code != 0 {
		t.Fatalf("comment = %d %s", code, errOut.String())
	}
	if got["op"] != "comment" || got["target"] != "acme/docs#42" || auth != "" || !strings.Contains(out.String(), `"effectId":"eff_1"`) {
		t.Errorf("sent %v (%q), printed %s", got, auth, out.String())
	}
	if code := runFx(context.Background(), []string{"label", "add", "--name", "x"}, strings.NewReader(""), io.Discard, &errOut, getenv); code != 3 {
		t.Errorf("a refused op = %d", code)
	}
	env[pipeline.EnvEffector] = filepath.Join(dir, "missing.json")
	if code := runFx(context.Background(), []string{"comment", "--body", "hi"}, strings.NewReader(""), io.Discard, io.Discard, getenv); code != 2 {
		t.Errorf("a missing config = %d", code)
	}
	out.Reset()
	if code := runFx(context.Background(), []string{"help"}, nil, &out, io.Discard, getenv); code != 0 ||
		!strings.Contains(out.String(), "stack rebase") || !strings.Contains(out.String(), "git-credential") {
		t.Errorf("help = %d %s", code, out.String())
	}
}

// TestFxMCP: `fx mcp` and `factory-agent mcp` serve the Effector's tools on stdin/stdout,
// which is what both harness adapters' MCP configs run (bin/factory mcp).
func TestFxMCP(t *testing.T) {
	if got := append([]string{"fx"}, clirun.EffectorArgs()...); !slices.Equal(got, []string{"fx", "mcp"}) {
		t.Fatalf("the adapters run %q", got)
	}
	workspace := func(k string) string {
		if k == pipeline.EnvRuntime {
			return pipeline.RuntimeWorkspace
		}
		return ""
	}
	for _, viaFx := range []bool{true, false} {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan int, 1)
		go func() {
			if viaFx {
				done <- runFx(context.Background(), []string{"mcp"}, inR, outW, io.Discard, workspace)
			} else {
				done <- runMCP(context.Background(), nil, inR, outW, io.Discard, workspace)
			}
			_ = outW.Close()
		}()
		_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`+"\n"+
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
		dec := json.NewDecoder(outR)
		var names []string
		for len(names) == 0 {
			var m struct {
				ID     int `json:"id"`
				Result struct {
					Tools []struct{ Name string } `json:"tools"`
				} `json:"result"`
			}
			if err := dec.Decode(&m); err != nil {
				t.Fatal(err)
			}
			for _, tool := range m.Result.Tools {
				names = append(names, tool.Name)
			}
		}
		_ = inW.Close()
		if code := <-done; code != 0 {
			t.Errorf("mcp exit %d", code)
		}
		if len(names) != len(effector.Ops()) || !slices.Contains(names, "pr_open") || !slices.Contains(names, "stack_rebase") {
			t.Errorf("tools %q", names)
		}
	}
	if code := run(context.Background(), []string{"mcp", "extra"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("mcp with arguments = %d", code)
	}
}
