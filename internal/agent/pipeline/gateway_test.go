package pipeline_test

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/agent/harness/fake"
	"github.com/compliance-framework/factory/internal/agent/internal/fakegateway"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
	"github.com/compliance-framework/factory/internal/agent/runbundle"
	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

func TestNewWorkspaceOptions(t *testing.T) {
	t.Parallel()
	ad := map[string]harness.Adapter{fake.Name: &fake.Adapter{}}
	for _, tt := range []struct {
		name string
		o    pipeline.Options
		want string // "" = accepted
	}{
		{"Root alone", pipeline.Options{Root: "/work", Adapters: ad}, ""},
		{"Root in gateway mode with its Env", pipeline.Options{Root: "/work", Adapters: ad, GitAuth: pipeline.GitAuthGateway, Env: map[string]string{
			"HTTPS_PROXY": "http://10.0.0.1:3128", "https_proxy": "http://10.0.0.1:3128", "NO_PROXY": "localhost", "GIT_SSL_CAINFO": "/s/ca.pem",
			"SSL_CERT_FILE": "/s/ca.pem", "GH_TOKEN": fakegateway.DefaultPlaceholder, "FACTORY_RUNTIME": "workspace", "DOCKER_CONFIG": "/s/docker"}}, ""},
		{"WorkRoot in gateway mode", pipeline.Options{WorkRoot: "/w", Adapters: ad, GitAuth: pipeline.GitAuthGateway}, ""},
		{"neither root", pipeline.Options{Adapters: ad}, "WorkRoot or Root is required"},
		{"both roots", pipeline.Options{WorkRoot: "/w", Root: "/work", Adapters: ad}, "exclusive"},
		{"a relative Root", pipeline.Options{Root: "work", Adapters: ad}, `Root "work" must be absolute`},
		{"an unknown GitAuth", pipeline.Options{Root: "/work", Adapters: ad, GitAuth: 7}, "unknown GitAuth GitAuth(7)"},
		{"Env sets HOME", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"HOME": "/root"}}, "HOME is set by the pipeline"},
		{"Env sets PATH", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"PATH": "/bin"}}, "PATH is set by the pipeline"},
		{"Env sets the socket", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{gitcred.EnvSocket: "/s"}}, gitcred.EnvSocket + " is set"},
		{"Env sets GIT_CONFIG_COUNT", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"GIT_CONFIG_COUNT": "1"}}, "GIT_CONFIG_COUNT is set"},
		{"Env sets a helper config key", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"GIT_CONFIG_KEY_0": "credential.helper"}}, "GIT_CONFIG_KEY_0 is set"},
		{"Env sets GIT_ALLOW_PROTOCOL", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"GIT_ALLOW_PROTOCOL": "file"}}, "GIT_ALLOW_PROTOCOL is set"},
		{"Env sets FACTORY_TOKEN", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{pipeline.EnvToken: "x"}}, "FACTORY_TOKEN is set"},
		{"Env sets FACTORY_API", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{pipeline.EnvAPI: "x"}}, "FACTORY_API is set"},
		{"Env sets FACTORY_EFFECTOR", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{pipeline.EnvEffector: "x"}}, "FACTORY_EFFECTOR is set"},
		{"Env sets a helper config value", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"GIT_CONFIG_VALUE_0": envSecret}}, "GIT_CONFIG_VALUE_0 is set"},
		{"an empty name", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"": "x"}}, `"" is not a variable name`},
		{"a name with =", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"A=B": "x"}}, `"A=B" is not a variable name`},
		{"a NUL in a value", pipeline.Options{Root: "/work", Adapters: ad, Env: map[string]string{"A": "x\x00y"}}, "A: the value holds a NUL byte"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pipeline.New(tt.o)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err %v, want %q", err, tt.want)
			case err != nil && (strings.Contains(err.Error(), "x\x00y") || strings.Contains(err.Error(), envSecret)):
				t.Errorf("the error holds the value: %v", err)
			}
		})
	}
}

// envSecret is a value Options.Env's refusals must never quote.
const envSecret = "env-value-never-quoted"

// Options.Env may not set a variable that adds a credential helper or changes what git, the
// loader or the shell runs (the gateway's "no helper at all", the host's "only the in-memory
// helper"); it may set every proxy and CA variable vmrun's system setup sets.
func TestNewEnvControlVariables(t *testing.T) {
	t.Parallel()
	ad := map[string]harness.Adapter{fake.Name: &fake.Adapter{}}
	for _, k := range []string{
		"GIT_CONFIG_PARAMETERS", "GIT_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "GIT_SSL_NO_VERIFY",
		"SSH_ASKPASS", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "BASH_ENV", "ENV",
	} {
		for _, mode := range []pipeline.GitAuth{pipeline.GitAuthHelper, pipeline.GitAuthGateway} {
			t.Run(k+"/"+mode.String(), func(t *testing.T) {
				_, err := pipeline.New(pipeline.Options{Root: "/work", Adapters: ad, GitAuth: mode, Env: map[string]string{k: envSecret}})
				if want := k + " controls git, the loader or the shell"; err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err %v, want %q", err, want)
				}
				if strings.Contains(err.Error(), envSecret) {
					t.Errorf("the error holds the value: %v", err)
				}
			})
		}
	}
	// vmrun's system setup (workspace-run.md §System setup), and names that only look close.
	accepted := map[string]string{
		"HTTPS_PROXY": "http://10.0.0.1:3128", "HTTP_PROXY": "http://10.0.0.1:3128", "https_proxy": "http://10.0.0.1:3128", "http_proxy": "http://10.0.0.1:3128",
		"NO_PROXY": "localhost,10.0.0.0/8", "no_proxy": "localhost,10.0.0.0/8",
		"SSL_CERT_FILE": "/s/ca-bundle.pem", "GIT_SSL_CAINFO": "/s/ca-bundle.pem", "GIT_SSL_CAPATH": "/s/certs", "CURL_CA_BUNDLE": "/s/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE": "/s/ca-bundle.pem", "NODE_EXTRA_CA_CERTS": "/b/gateway/ca.pem",
		pipeline.EnvGHToken: fakegateway.DefaultPlaceholder, "FACTORY_RUNTIME": "workspace", "DOCKER_CONFIG": "/s/docker", "XDG_DATA_HOME": "/home/admin/.local/share",
		"ENVIRONMENT": "ci", "GITHUB_ACTIONS": "true", "LDFLAGS": "-s",
	}
	for _, mode := range []pipeline.GitAuth{pipeline.GitAuthHelper, pipeline.GitAuthGateway} {
		if _, err := pipeline.New(pipeline.Options{Root: "/work", Adapters: ad, GitAuth: mode, Env: accepted}); err != nil {
			t.Errorf("%v: the system setup's Env refused: %v", mode, err)
		}
	}
}

// Options.Root is the workspace itself: made when absent, taken when empty, refused when it
// holds anything or isn't a directory, and never removed. With FetchSkills it gets skills/.
func TestRunRootWorkspace(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string // "" = the run is done
	}{
		{"absent", func(*testing.T, string) {}, ""},
		{"empty", func(t *testing.T, root string) { mkdir(t, root) }, ""},
		{"not empty", func(t *testing.T, root string) {
			mkdir(t, root)
			writeFile(t, filepath.Join(root, "leftover"), "x")
		}, "is not empty"},
		{"a file", func(t *testing.T, root string) { writeFile(t, root, "x") }, "is not a directory"},
		{"its parent is missing", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Dir(root)); err != nil {
				t.Fatal(err)
			}
		}, "no such file or directory"},
		{"a symlink", func(t *testing.T, root string) {
			dir := filepath.Join(t.TempDir(), "elsewhere")
			mkdir(t, dir)
			if err := os.Symlink(dir, root); err != nil {
				t.Fatal(err)
			}
		}, "is not a directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			root := filepath.Join(t.TempDir(), "work")
			tt.setup(t, root)
			before := listTree(t, root)
			f.opts.WorkRoot, f.opts.Root = "", root
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult)}}
			out, err := f.pipeline().Run(context.Background(), f.job(f.task(), time.Now()), &sink{})
			if err != nil {
				t.Fatal(err)
			}
			if tt.want != "" {
				if out.Status != runner.StatusFailed || out.Reason != runner.ReasonHarnessError || out.Result == nil ||
					!strings.Contains(out.Result.Summary, "workspace: workspace root") || !strings.Contains(out.Result.Summary, tt.want) {
					t.Fatalf("outcome %+v %+v", out, out.Result)
				}
				if after := listTree(t, root); !slices.Equal(after, before) {
					t.Errorf("the refused root changed: %v, was %v", after, before)
				}
				return
			}
			if out.Status != runner.StatusDone {
				t.Fatalf("outcome %+v %+v", out, out.Result)
			}
			ws := f.adapter.Prepared()
			if len(ws) != 1 || ws[0].Root != root || ws[0].Repo != filepath.Join(root, "repo") || ws[0].Skills != filepath.Join(root, "skills") ||
				ws[0].Result != filepath.Join(root, "out", "result.json") || len(ws[0].SkillList) != 2 {
				t.Fatalf("workspaces %+v", ws)
			}
			if b, err := os.ReadFile(filepath.Join(root, "out", "result.json")); err != nil || len(b) == 0 {
				t.Errorf("out/result.json after the run: %q %v", b, err)
			}
			// A root the run made is 0700; one that existed keeps the mode its maker chose.
			wantMode := os.FileMode(0o700)
			if before != nil {
				wantMode = 0o755
			}
			if st, err := os.Lstat(root); err != nil {
				t.Errorf("the root after the run: %v", err)
			} else if st.Mode().Perm() != wantMode {
				t.Errorf("the root's mode %v, want %v", st.Mode().Perm(), wantMode)
			}
			if entries, err := os.ReadDir(f.workRoot); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the unused WorkRoot was made: %v %v", entries, err)
			}
		})
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listTree is every path under root (root itself included), or nil when it doesn't exist.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Options.Env on the host: it wins over the owner's variables, reaches the harness and the
// pipeline's git, and the job's real GH_TOKEN still replaces a placeholder.
func TestRunEnvOnTheHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.Env = map[string]string{"DOCKER_HOST": "unix:///run/env.sock", "FACTORY_RUNTIME": "host-test", pipeline.EnvGHToken: fakegateway.DefaultPlaceholder}
	f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult)}}
	out, err := f.pipeline().Run(context.Background(), f.job(f.task(), time.Now()), &sink{})
	if err != nil || out.Status != runner.StatusDone {
		t.Fatalf("outcome %+v %v", out, err)
	}
	env := envMap(f.adapter.Prepared()[0].Env)
	for k, want := range map[string]string{"DOCKER_HOST": "unix:///run/env.sock", "FACTORY_RUNTIME": "host-test", pipeline.EnvGHToken: gitSecret, "GIT_CONFIG_COUNT": "3"} {
		if env[k] != want {
			t.Errorf("%s=%q, want %q", k, env[k], want)
		}
	}
	if env[gitcred.EnvSocket] == "" {
		t.Error("the host run has no credential socket")
	}
	f.checkClean("")
}

// recordTranscripts is a TranscriptStore that keeps what it's given and returns no ref, as a
// workspace's file store does.
type recordTranscripts struct {
	mu  sync.Mutex
	got [][]byte
}

func (s *recordTranscripts) Store(_ context.Context, _ *runner.Job, gz []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, gz)
	return "", nil
}

// gatewayJob is the fixture's job as a VM workspace has it: the same task and credential
// metadata, with every token empty, no dispatcher client and no agent token.
func (f *fixture) gatewayJob() *runner.Job {
	job := f.job(f.task(), time.Now())
	cred, reads := job.Git.Get(), job.Git.Reads()
	cred.Token = ""
	for i := range reads {
		reads[i].Token = ""
	}
	job.Lease.GitCredential, job.Lease.ReadCredentials = cred, reads
	job.Git = runner.NewCredentials(cred, reads...)
	job.API, job.AgentToken, job.Dispatcher = nil, nil, runbundle.DefaultDispatcherURL
	return job
}

// A run in gateway mode, from a bundle, as the VM workspace runs it: the pipeline's own clones
// and the harness's git reach the git server only through the fake gateway, which injects every
// credential; there is no credential socket, no helper config, no FACTORY_TOKEN and no real
// GH_TOKEN; the workspace is the exact Root, has no skills/ of its own, and is kept.
func TestRunGatewayMode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, b := f.prepare(f.job(f.task(), time.Now()), pipeline.BundleExtras{})
	before := len(f.disp.callList())

	gw := fakegateway.Start(t, fakegateway.Config{GitUpstream: f.git.ts.URL, Repos: map[string]fakegateway.Repo{
		"acme/docs": {Token: gitSecret, RW: true}, "acme/private-ro": {Token: readSecret}}})
	state := t.TempDir()
	caFile := filepath.Join(state, "ca.pem")
	writeFile(t, caFile, string(gw.CA().CertPEM()))
	genv := map[string]string{
		"HTTPS_PROXY": gw.URL(), "https_proxy": gw.URL(), "HTTP_PROXY": gw.URL(), "http_proxy": gw.URL(),
		"NO_PROXY": "", "no_proxy": "", "GIT_SSL_CAINFO": caFile, "SSL_CERT_FILE": caFile,
		pipeline.EnvGHToken: fakegateway.DefaultPlaceholder, "FACTORY_RUNTIME": "workspace",
	}
	root := filepath.Join(t.TempDir(), "work")
	sockets := t.TempDir()
	transcripts := &recordTranscripts{}
	f.opts.WorkRoot, f.opts.Root = "", root
	f.opts.GitAuth = pipeline.GitAuthGateway
	f.opts.Env = genv
	f.opts.GitBaseURL = "" // https://github.com, the credential's host, through the gateway
	f.opts.SocketDir = sockets
	f.opts.Prompts, f.opts.Schemas, f.opts.Skills = pipeline.BundlePrompts{B: b}, pipeline.BundleSchemas{B: b}, pipeline.BundleSkills(b)
	f.opts.Transcripts = transcripts
	var finished bool
	f.opts.Finished = func(runner.Outcome) {
		finished = true
		if _, err := os.Stat(filepath.Join(root, "out", "result.json")); err != nil {
			t.Errorf("out/result.json at Finished: %v", err)
		}
	}

	f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult), Do: func(ctx context.Context, ws harness.Workspace, _ string) error {
		if entries, err := os.ReadDir(sockets); err != nil || len(entries) != 0 {
			t.Errorf("the socket directory during the run: %v %v", entries, err)
		}
		// The harness's git fetches the private read repo with no credential of its own.
		cmd := exec.CommandContext(ctx, "git", "fetch", "--quiet", "origin")
		cmd.Dir, cmd.Env = filepath.Join(ws.Root, "ro", "private-ro"), ws.Env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("the harness's fetch through the gateway: %v\n%s", err, out)
		}
		return nil
	}}}
	out, err := f.pipeline().Run(context.Background(), f.gatewayJob(), &sink{})
	if err != nil || out.Status != runner.StatusDone || out.TranscriptRef != "" || !finished {
		t.Fatalf("outcome %+v %+v %v (finished %v)", out, out.Result, err, finished)
	}
	if calls := f.disp.callList()[before:]; len(calls) != 0 {
		t.Errorf("the gateway run called the dispatcher: %v", calls)
	}
	if len(transcripts.got) != 1 {
		t.Errorf("%d transcripts stored, want 1", len(transcripts.got))
	}

	ws := f.adapter.Prepared()
	if len(ws) != 1 {
		t.Fatalf("workspaces %+v", ws)
	}
	env := envMap(ws[0].Env)
	for k, want := range genv {
		if got, ok := env[k]; !ok || got != want {
			t.Errorf("the harness's %s=%q (set %v), want %q", k, got, ok, want)
		}
	}
	for k := range env {
		if k == gitcred.EnvSocket || k == pipeline.EnvToken || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || k == "GIT_CONFIG_COUNT" {
			t.Errorf("the harness's environment has %s", k)
		}
	}
	if env["GIT_CONFIG_NOSYSTEM"] != "1" || env["GIT_CONFIG_GLOBAL"] != filepath.Join(root, "home", ".gitconfig") || env[pipeline.EnvAPI] != runbundle.DefaultDispatcherURL {
		t.Errorf("the harness's environment: %v", env)
	}
	if ws[0].Root != root || ws[0].Repo != filepath.Join(root, "repo") || ws[0].Skills != b.Path(runbundle.SkillsDir) || len(ws[0].SkillList) != 2 {
		t.Errorf("workspace %+v", ws[0])
	}
	// The root is kept, without a skills/ of its own; the helper never ran.
	if _, err := os.Stat(filepath.Join(root, "out", "result.json")); err != nil {
		t.Errorf("out/ after the run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the root has a skills/ with bundle skills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.argvDir, "helper.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the credential helper ran: %v", err)
	}
	scanForSecrets(t, root)

	// Every git request went through the gateway, which injected the credentials.
	injected := map[string]bool{}
	for _, r := range gw.Records() {
		injected[r.Injected] = true
		if r.Host != fakegateway.DefaultGitHost {
			t.Errorf("a request to %s through the gateway", r.Host)
		}
	}
	if want := []string{"", "git:acme/docs", "git:acme/private-ro"}; !slices.Equal(slices.Sorted(maps.Keys(injected)), want) {
		t.Errorf("injected %v, want %v", slices.Sorted(maps.Keys(injected)), want)
	}
	gw.AssertNoClientCredential(t)
	for _, repo := range []string{"acme/docs", "acme/private-ro"} {
		if authed, denied, _ := f.git.counts(repo); authed == 0 || denied != 0 {
			t.Errorf("%s: %d authenticated and %d refused requests", repo, authed, denied)
		}
	}
	if _, _, sent := f.git.counts("acme/local-dev"); sent != 0 {
		t.Errorf("the public repo got %d requests with credentials", sent)
	}
}

// A gateway-mode run refuses, before any step, a job that carries a credential (the VM holds
// none: the gateway injects them), naming the field and never its value.
func TestRunGatewayModeRefusesJobCredentials(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		field string
		set   func(f *fixture, job *runner.Job)
	}{
		{"Lease.GitCredential.Token", func(_ *fixture, job *runner.Job) { job.Lease.GitCredential.Token = gitSecret }},
		{"Lease.ReadCredentials[0].Token", func(_ *fixture, job *runner.Job) { job.Lease.ReadCredentials[0].Token = readSecret }},
		{"Git.Token", func(_ *fixture, job *runner.Job) {
			cred := job.Git.Get()
			cred.Token = gitSecret
			job.Git = runner.NewCredentials(cred, job.Git.Reads()...)
		}},
		{"Git.Reads[0].Token", func(_ *fixture, job *runner.Job) {
			reads := job.Git.Reads()
			reads[0].Token = readSecret
			job.Git = runner.NewCredentials(job.Git.Get(), reads...)
		}},
		{"AgentToken", func(_ *fixture, job *runner.Job) { job.AgentToken = func() string { return agentSecret } }},
		{"API", func(f *fixture, job *runner.Job) { job.API = f.disp.client(f.t) }},
	} {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			root := filepath.Join(t.TempDir(), "work")
			f.opts.WorkRoot, f.opts.Root, f.opts.GitAuth = "", root, pipeline.GitAuthGateway
			f.opts.SocketDir = t.TempDir()
			var steps []string
			f.opts.OnStep = func(name string, _ time.Duration, _ error) { steps = append(steps, name) }
			f.adapter.Script = fake.Script{Turn: fake.Turn{Result: []byte(validResult)}}
			job := f.gatewayJob()
			tt.set(f, job)
			out, err := f.pipeline().Run(context.Background(), job, &sink{})
			if err != nil {
				t.Fatal(err)
			}
			want := "gateway mode: the job carries a credential in " + tt.field
			if out.Status != runner.StatusFailed || out.Reason != runner.ReasonHarnessError || out.Result == nil || !strings.Contains(out.Result.Summary, want) {
				t.Fatalf("outcome %+v %+v, want %q", out, out.Result, want)
			}
			for _, s := range []string{gitSecret, readSecret, agentSecret} {
				if strings.Contains(out.Result.Summary, s) || strings.Contains(f.logs.String(), s) {
					t.Errorf("the refusal quotes a credential: %q", out.Result.Summary)
				}
			}
			// Only finish, which every run ends with, reports.
			if !slices.Equal(steps, []string{"finish"}) || len(f.adapter.Prepared()) != 0 || listTree(t, root) != nil {
				t.Errorf("a step ran: steps %v, prepared %d, root %v", steps, len(f.adapter.Prepared()), listTree(t, root))
			}
			if calls := f.disp.callList(); len(calls) != 0 {
				t.Errorf("the refused run called the dispatcher: %v", calls)
			}
		})
	}
}

// PrepareBundle runs on the host: a workspace pipeline refuses it.
func TestPrepareBundleRefusesAWorkspacePipeline(t *testing.T) {
	t.Parallel()
	ad := map[string]harness.Adapter{fake.Name: &fake.Adapter{}}
	for name, o := range map[string]pipeline.Options{
		"Root":           {Root: filepath.Join(t.TempDir(), "work"), Adapters: ad, AgentBinary: "/bin/false"},
		"GitAuthGateway": {WorkRoot: t.TempDir(), GitAuth: pipeline.GitAuthGateway, Adapters: ad, AgentBinary: "/bin/false"},
	} {
		p, err := pipeline.New(o)
		if err != nil {
			t.Fatal(err)
		}
		job := &runner.Job{Lease: apiclient.Lease{}}
		if err := p.PrepareBundle(context.Background(), job, filepath.Join(t.TempDir(), "b"), pipeline.BundleExtras{}); err == nil ||
			!strings.Contains(err.Error(), "PrepareBundle runs on the host") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
