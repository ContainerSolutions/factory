package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/harnesstest"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// vmAccount is the account a VM run's bundle names: identifiers, not credentials.
var vmAccount = runbundle.CodexAccount{AccountID: "acct_0123456789abcdef", UserID: "user-AbCdEf0123456789", PlanType: "pro"}

// vmWorkspace is workspace(t, writable) as a VM workspace's pipeline hands it over in gateway
// mode: no FACTORY_TOKEN, the gateway's proxy and CA, and GH_TOKEN the placeholder.
func vmWorkspace(t *testing.T, writable bool) harness.Workspace {
	t.Helper()
	ws := workspace(t, writable)
	ws.Env = []string{"FACTORY_API=http://dispatcher.factory.internal", "FACTORY_RESULT=" + ws.Result, "FACTORY_RUNTIME=workspace",
		"GH_TOKEN=" + harness.Placeholder, "HOME=" + ws.Home, "HTTPS_PROXY=http://192.168.64.1:3128", "NO_PROXY=localhost,127.0.0.1",
		"PATH=" + ws.Bin + ":" + os.Getenv("PATH"), "SSL_CERT_FILE=/var/lib/factory-agent/run/ca-bundle.pem", "TMPDIR=" + ws.Tmp}
	return ws
}

// vmAdapter is the adapter with vmAccount. An empty binary is one that doesn't exist
// (noCodex), never the codex on PATH.
func vmAdapter(t *testing.T, binary string) *Adapter {
	t.Helper()
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "no-codex")
	}
	acct := vmAccount
	return New(Options{
		Account: &acct, Binary: binary, Clock: clock.NewFake(t0), KillGrace: 200 * time.Millisecond,
		Config: config.Harness{Default: "gpt-5-codex"},
	})
}

func vmTask(model, effort, auth string) v1alpha1.Task {
	t := task(model, effort)
	t.Spec.Harness.Resolved.Auth = auth
	return t
}

// golden compares got, with the workspace root replaced by $ROOT, against testdata/vm/<name>
// (-update rewrites it).
func golden(t *testing.T, ws harness.Workspace, name string, got []byte) {
	t.Helper()
	got = bytes.ReplaceAll(got, []byte(ws.Root), []byte("$ROOT"))
	p := filepath.Join("testdata", "vm", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\n%s", p, got)
	}
}

// TestVMPrepare pins the scratch CODEX_HOME: the placeholder auth.json is a
// 0600 file of its own (no link to a login), config.toml has no [sandbox_workspace_write]
// and keeps the MCP server, AGENTS.md is the preamble (plus, for a read-only checkout, that
// it may not be changed), and the skills are flattened as on the host. No token reaches a
// file: not the owner's login (in the adapter's Config.Home, which the VM never reads), nor
// a token in the run's environment (the "env tokens" case; the env's names are in
// config.toml, never its values).
func TestVMPrepare(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		writable  bool
		envTokens bool
	}{
		{"read-write", true, false},
		{"read-only", false, false},
		{"env tokens", true, true},
	} {
		name, writable := tt.name, tt.writable
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ws := vmWorkspace(t, writable)
			if tt.envTokens {
				// The host's tokens, as if the pipeline had handed them over: GH_TOKEN the
				// task's real token rather than the placeholder, and the agent token.
				i := slices.Index(ws.Env, "GH_TOKEN="+harness.Placeholder)
				ws.Env[i] = "GH_TOKEN=ghs_taskTokenNeverOnDisk0123456789"
				ws.Env = append(ws.Env, "FACTORY_TOKEN=agent-token-never-on-disk")
			}
			if err := os.MkdirAll(filepath.Join(ws.Repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := vmAdapter(t, "").Prepare(context.Background(), ws, vmTask("gpt-5-codex", "high", AuthSubscription)); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(ws.Home, ".codex")
			want := []string{
				"AGENTS.md", "auth.json", "config.toml", "skills/",
				"skills/ccf-review-review/", "skills/ccf-review-review/SKILL.md",
				"skills/docs-style/", "skills/docs-style/SKILL.md", "skills/docs-style/scripts/", "skills/docs-style/scripts/check.sh (x)",
				"skills/lisa-review/", "skills/lisa-review/SKILL.md",
			}
			if got := listTree(t, home); !slices.Equal(got, want) {
				t.Errorf("CODEX_HOME:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			fi, err := os.Lstat(filepath.Join(home, "auth.json"))
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
				t.Errorf("auth.json: %v %v", fi, err)
			}
			read := func(name string) []byte {
				b, err := os.ReadFile(filepath.Join(home, name))
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			cfg := read("config.toml")
			if strings.Contains(string(cfg), "sandbox_workspace_write") || strings.Contains(string(cfg), "writable_roots") {
				t.Errorf("the VM config.toml tunes a sandbox:\n%s", cfg)
			}
			if _, err := os.Stat(filepath.Join(ws.Home, ".cache")); err == nil {
				t.Error("the VM made the sandbox's writable cache dir")
			}
			// auth.json and config.toml don't depend on the checkout: one golden each, which
			// the read-write case writes on -update. The env tokens case's config.toml
			// names two more variables, and it writes no golden.
			switch {
			case tt.envTokens:
				if !*update {
					golden(t, ws, "auth.golden.json", read("auth.json"))
				}
				if !strings.Contains(string(cfg), `"FACTORY_TOKEN"`) {
					t.Errorf("config.toml doesn't pass FACTORY_TOKEN through by name:\n%s", cfg)
				}
			case writable || !*update:
				golden(t, ws, "agents-"+name+".golden.md", read("AGENTS.md"))
				golden(t, ws, "auth.golden.json", read("auth.json"))
				golden(t, ws, "config.golden.toml", cfg)
			default:
				golden(t, ws, "agents-"+name+".golden.md", read("AGENTS.md"))
			}
			checkPlaceholderAuth(t, read("auth.json"), vmAccount, t0)
			scanFiles(t, ws.Root)
		})
	}
}

// checkPlaceholderAuth decodes auth.json the way codex reads it and checks its meaning, so
// -update can't bless a regression in the golden: Phase 0's shape (P1-phase0-report.md, Q5)
// and nothing else at either level, OPENAI_API_KEY null, an unsigned id_token carrying
// Phase 0's email and exp and exactly the account's identifiers, the placeholder as both
// tokens, the account id, and last_refresh the injected now. The values are written out here
// rather than taken from flags.go, so a changed constant fails too.
func checkPlaceholderAuth(t *testing.T, b []byte, acct runbundle.CodexAccount, now time.Time) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(maps.Keys(top)), []string{"OPENAI_API_KEY", "last_refresh", "tokens"}; !slices.Equal(got, want) {
		t.Errorf("auth.json has the keys %q, want %q:\n%s", got, want, b)
	}
	if v := string(top["OPENAI_API_KEY"]); v != "null" {
		t.Errorf("auth.json: OPENAI_API_KEY = %s, want null", v)
	}
	var lastRefresh time.Time
	if err := json.Unmarshal(top["last_refresh"], &lastRefresh); err != nil || !lastRefresh.Equal(now) {
		t.Errorf("auth.json: last_refresh = %s (%v), want %s", top["last_refresh"], err, now.UTC().Format(time.RFC3339Nano))
	}
	var tokens map[string]string
	if err := json.Unmarshal(top["tokens"], &tokens); err != nil {
		t.Fatalf("auth.json: tokens: %v", err)
	}
	want := map[string]string{"access_token": harness.Placeholder, "refresh_token": harness.Placeholder, "account_id": acct.AccountID}
	for k, v := range want {
		if tokens[k] != v {
			t.Errorf("tokens.%s = %q, want %q", k, tokens[k], v)
		}
	}
	if got := slices.Sorted(maps.Keys(tokens)); !slices.Equal(got, []string{"access_token", "account_id", "id_token", "refresh_token"}) {
		t.Errorf("auth.json: tokens has the keys %q:\n%s", got, b)
	}
	parts := strings.Split(tokens["id_token"], ".")
	if len(parts) != 3 || parts[2] == "" {
		t.Fatalf("id_token %q isn't a three-segment JWT", tokens["id_token"])
	}
	decode := func(seg string, v any) {
		raw, err := base64.RawURLEncoding.DecodeString(seg)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	var header map[string]string
	decode(parts[0], &header)
	if header["alg"] != "none" || header["typ"] != "JWT" || len(header) != 2 {
		t.Errorf("id_token header %v", header)
	}
	var claims any
	decode(parts[1], &claims)
	wantClaims := map[string]any{
		"email": "placeholder@example.invalid",
		"exp":   4102444800,
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": acct.AccountID, "chatgpt_user_id": acct.UserID, "chatgpt_plan_type": acct.PlanType,
		},
	}
	if got := mustJSON(t, claims); !bytes.Equal(got, mustJSON(t, wantClaims)) {
		t.Errorf("id_token claims %s, want %s", got, mustJSON(t, wantClaims))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVMRun runs the fake codex: danger-full-access from the pipeline's
// WorkDir (never out/, whichever the checkout's mode), the placeholder login in CODEX_HOME,
// the pipeline's gateway environment unchanged, and a Repair that resumes with the Run's
// sandbox and directory.
func TestVMRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		writable bool
		// workDir moves the pipeline's WorkDir: "" keeps the checkout, "-" empties it.
		workDir string
		auth    string
	}{
		{"read-write", true, "", AuthSubscription},
		{"read-only, no resolved auth", false, "", ""},
		{"no work dir", true, "-", AuthSubscription},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := vmWorkspace(t, tt.writable)
			wantDir := ws.Repo
			if tt.workDir == "-" {
				ws.WorkDir, wantDir = "", ws.Root
			}
			fc := newFakeCodex(t, fakePlan{Runs: []fakeRun{
				{Stream: "codex-success.jsonl", Result: `{"status":"done"}`},
				{Stream: "codex-success.jsonl", LastMessage: okResult},
			}})
			a := vmAdapter(t, fc.Binary)
			if err := a.Prepare(context.Background(), ws, vmTask("", "high", tt.auth)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Run(context.Background(), ws, vmTask("gpt-5-codex", "high", tt.auth), "the prompt\n", &harnesstest.Recorder{}); err != nil {
				t.Fatal(err)
			}
			raw, err := a.Repair(context.Background(), ws, []string{"/outcome: missing"}, &harnesstest.Recorder{})
			if err != nil || string(raw.Result) != okResult {
				t.Fatalf("repair: %q, %v", raw.Result, err)
			}
			calls := fc.calls()
			if len(calls) != 2 {
				t.Fatalf("%d codex execs, want the run and the repair", len(calls))
			}
			home := filepath.Join(ws.Home, ".codex")
			for i, c := range calls {
				for _, w := range []string{"--sandbox", "danger-full-access", "--cd", wantDir, "--model", "gpt-5-codex", `approval_policy="never"`} {
					if !slices.Contains(c.Args, w) {
						t.Errorf("call %d: args %q lack %q", i, c.Args, w)
					}
				}
				if slices.Contains(c.Args, "workspace-write") || !sameDir(c.Dir, wantDir) {
					t.Errorf("call %d: args %q in %s", i, c.Args, c.Dir)
				}
				want := map[string]string{"CODEX_HOME": home, "GH_TOKEN": harness.Placeholder, "HTTPS_PROXY": "http://192.168.64.1:3128",
					"FACTORY_RUNTIME": "workspace"}
				for k, v := range want {
					if c.Env[k] != v {
						t.Errorf("call %d: env %s = %q, want %q", i, k, c.Env[k], v)
					}
				}
				for _, k := range []string{"FACTORY_TOKEN", "OPENAI_API_KEY", "FACTORY_GIT_CREDENTIAL_SOCKET"} {
					if v, ok := c.Env[k]; ok {
						t.Errorf("call %d: env has %s=%q", i, k, v)
					}
				}
				if !slices.Contains(c.Home, "auth.json") || c.Auth == "" {
					t.Errorf("call %d: CODEX_HOME %q, auth.json %q", i, c.Home, c.Auth)
				}
				checkPlaceholderAuth(t, []byte(c.Auth), vmAccount, t0)
				if strings.Contains(c.Config, "sandbox_workspace_write") || strings.Contains(c.Agents, "FACTORY_OUT") {
					t.Errorf("call %d: config.toml\n%s\nAGENTS.md\n%s", i, c.Config, c.Agents)
				}
			}
			if i := slices.Index(calls[1].Args, "resume"); i < 0 || calls[1].Args[i+1] != "0199f2a1-6b1e-7c33-9a51-2f0c4e8d1a01" {
				t.Errorf("repair args %q", calls[1].Args)
			}
			// The whole command lines, the run's and the repair's (one golden, from the
			// read-write case: the others differ only in --cd).
			if tt.name == "read-write" {
				golden(t, ws, "exec-args.golden", []byte(strings.Join(calls[0].Args, "\n")+"\n\n"+strings.Join(calls[1].Args, "\n")+"\n"))
			}
		})
	}
}

// TestVMRefusals: a VM run without a work dir, without the account, on an API key or an
// unknown auth, or over an existing auth.json is refused.
func TestVMRefusals(t *testing.T) {
	t.Parallel()
	noDir := vmWorkspace(t, true)
	noDir.WorkDir, noDir.Root = "", ""
	if _, err := vmAdapter(t, "").Run(context.Background(), noDir, vmTask("", "", ""), "p", &harnesstest.Recorder{}); err == nil || !strings.Contains(err.Error(), "neither a work dir nor a root") {
		t.Errorf("Run without a work dir = %v", err)
	}

	for _, tt := range []struct {
		name    string
		account *runbundle.CodexAccount
		auth    string
		home    bool // a workspace HOME
		want    string
	}{
		{"api key", &vmAccount, AuthAPIKey, true, "API-key run isn't supported in a VM workspace"},
		{"unknown auth", &vmAccount, "sso", true, `unknown auth "sso"`},
		{"no account", nil, AuthSubscription, true, "needs the ChatGPT account's identifiers"},
		{"no plan", &runbundle.CodexAccount{AccountID: "acct_1", UserID: "user_1"}, "", true, "needs the ChatGPT account's identifiers"},
		{"no HOME", &vmAccount, AuthSubscription, false, "no scratch HOME"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := vmAdapter(t, "")
			a.o.Account = tt.account
			ws := vmWorkspace(t, true)
			if !tt.home {
				ws.Home = ""
			}
			if err := a.Prepare(context.Background(), ws, vmTask("", "", tt.auth)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Prepare = %v, want %q", err, tt.want)
			}
		})
	}

	// An auth.json already there (a file, or a link to a real login) is never written over.
	for name, plant := range map[string]func(t *testing.T, p string) error{
		"file":    func(_ *testing.T, p string) error { return os.WriteFile(p, []byte(ownerLogin), 0o600) },
		"symlink": func(t *testing.T, p string) error { return os.Symlink(filepath.Join(ownerHome(t), "auth.json"), p) },
	} {
		t.Run("existing "+name, func(t *testing.T) {
			ws := vmWorkspace(t, true)
			home := filepath.Join(ws.Home, ".codex")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := plant(t, filepath.Join(home, "auth.json")); err != nil {
				t.Fatal(err)
			}
			if err := vmAdapter(t, "").Prepare(context.Background(), ws, vmTask("", "", "")); err == nil || !strings.Contains(err.Error(), "the placeholder login") {
				t.Errorf("Prepare = %v", err)
			}
		})
	}
}
