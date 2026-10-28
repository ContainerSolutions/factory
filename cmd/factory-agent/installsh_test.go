package main

// deploy/install.sh, the agent host installer each release carries (deploy/poc/README.md
// §Agent onboarding): it parses; its defaults are agent.yaml.example's; and, in a temp HOME with
// a stub uname, its --quiet runs write a config the agent's loader accepts and refuse bad values
// before changing anything. It is never run for real: in dry runs and refusals every command
// that changes something is a stub that records the call and fails, and HOME stays empty; runs
// that write get a curl stub serving fake factory-agent binaries (which only record their
// arguments) from a local dir, and --no-service. Only --quiet runs: the default mode reads /dev/tty.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/config"
)

const installScript = "../../deploy/install.sh"

func TestInstallScriptParses(t *testing.T) {
	if out, err := exec.Command("bash", "-n", installScript).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Log("shellcheck isn't installed: skipped")
		return
	}
	if out, err := exec.Command("shellcheck", installScript).CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

// forbidden are the commands a dry run must not run; each gets a stub that logs and fails.
var forbidden = []string{"curl", "wget", "install", "mkdir", "launchctl", "systemctl", "loginctl", "sudo", "tart", "shasum", "sha256sum", "factory-agent"}

// forbiddenWriting are the commands a run that writes must still not run.
var forbiddenWriting = []string{"wget", "launchctl", "systemctl", "loginctl", "sudo", "tart", "factory-agent"}

// installRun is one install.sh run; writes lets it write into the temp HOME, which home (when
// set) prepares.
type installRun struct {
	sysName, machine string
	env              map[string]string
	args             []string
	writes           bool
	home             func(t *testing.T, home string)
}

// installResult is a run's exit code, output and HOME, and what the fake binaries were asked.
type installResult struct {
	code                int
	out, home, agentRan string
}

const testToken = "enroll-token-value-never-printed"

func (r installRun) do(t *testing.T) installResult {
	t.Helper()
	home := t.TempDir()
	if r.home != nil {
		r.home(t, home)
	}
	stubs, assets := t.TempDir(), t.TempDir()
	calls, ran := filepath.Join(t.TempDir(), "calls"), filepath.Join(t.TempDir(), "ran")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", `case "$1" in -s) echo "$STUB_SYSNAME" ;; -m) echo "$STUB_MACHINE" ;; -n) echo My-Host.local ;; *) exit 2 ;; esac`+"\n")
	fail := forbidden
	if r.writes {
		fail = forbiddenWriting
		var sums strings.Builder
		for _, name := range []string{"factory-agent-darwin-arm64", "factory-agent-linux-arm64", "SHA256SUMS"} {
			body := "#!/bin/sh\necho \"" + name + " $*\" >> \"$STUB_RAN\"\n"
			if name == "SHA256SUMS" {
				body = sums.String()
			}
			if err := os.WriteFile(filepath.Join(assets, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(body))
			sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
		}
		// curl serves the release's files from assets and refuses any other URL.
		write("curl", `out="" url=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; https://*) url="$1" ;; esac; shift; done
case "$url" in "$STUB_BASE"/*) cp "$STUB_ASSETS/${url##*/}" "$out" ;; *) echo "curl $url" >> "$STUB_CALLS"; exit 99 ;; esac
`)
	}
	for _, name := range fail {
		write(name, `echo "`+name+` $*" >> "$STUB_CALLS"; exit 99`+"\n")
	}
	cmd := exec.Command("bash", append([]string{installScript}, r.args...)...)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + stubs + ":/usr/bin:/bin",
		"STUB_SYSNAME=" + r.sysName,
		"STUB_MACHINE=" + r.machine,
		"STUB_CALLS=" + calls,
		"STUB_RAN=" + ran,
		"STUB_BASE=https://github.com/ContainerSolutions/factory/releases/latest/download",
		"STUB_ASSETS=" + assets,
	}
	for k, v := range r.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run install.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	if b, err := os.ReadFile(calls); err == nil {
		t.Errorf("install.sh ran %s", b)
	}
	if strings.Contains(string(out), testToken) {
		t.Errorf("install.sh printed the enrollment token\n%s", out)
	}
	if entries, _ := os.ReadDir(home); !r.writes && len(entries) > 0 {
		t.Errorf("install.sh wrote %v under HOME", entries)
	}
	agentRan, _ := os.ReadFile(ran)
	return installResult{code: code, out: string(out), home: home, agentRan: string(agentRan)}
}

// planned is the agent.yaml a run printed, parsed with the agent's loader.
func planned(t *testing.T, res installResult) *config.Config {
	t.Helper()
	_, rest, ok := strings.Cut(res.out, "--- agent.yaml ---\n")
	yaml, _, ok2 := strings.Cut(rest, "--- end agent.yaml ---")
	if !ok || !ok2 {
		t.Fatalf("no agent.yaml in the plan\n%s", res.out)
	}
	cfg, err := config.Parse([]byte(yaml), testOptions(res.home))
	if err != nil {
		t.Fatalf("the agent.yaml doesn't load: %v\n%s", err, yaml)
	}
	return cfg
}

// written is the agent.yaml a run wrote, loaded with the agent's loader.
func written(t *testing.T, home string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(home, ".config/factory-agent/agent.yaml"), testOptions(home))
	if err != nil {
		t.Fatalf("the written agent.yaml doesn't load: %v", err)
	}
	return cfg
}

func testOptions(home string) config.Options {
	return config.Options{
		Home:     func() (string, error) { return home, nil },
		Hostname: func() (string, error) { return "other", nil },
		GOOS:     "linux",
	}
}

// example is agent.yaml.example, loaded.
func example(t *testing.T) *config.Config {
	t.Helper()
	data, err := os.ReadFile(agentExample)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data, exampleOptions())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// checkDefaults fails unless cfg's harnesses, lending and workspace size are the example's.
func checkDefaults(t *testing.T, cfg *config.Config) {
	t.Helper()
	want := example(t)
	if !reflect.DeepEqual(cfg.Harnesses, want.Harnesses) {
		t.Errorf("harnesses %+v; agent.yaml.example has %+v", cfg.Harnesses, want.Harnesses)
	}
	if !cfg.Lending.Unlimited || cfg.MaxConcurrent() != want.MaxConcurrent() {
		t.Errorf("lending %+v; want unlimited and the example's maxConcurrent %d", cfg.Lending, want.MaxConcurrent())
	}
	if wantSize := (config.Workspace{}).WithDefaults("linux").Size; cfg.Workspace == nil || cfg.Workspace.Size != wantSize {
		t.Errorf("workspace %+v; want size %+v (the example's)", cfg.Workspace, wantSize)
	}
}

func quietEnv(kv ...string) map[string]string {
	m := map[string]string{
		"FACTORY_DISPATCHER":       "https://factory.example.dev",
		"FACTORY_ENROLLMENT_TOKEN": testToken,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func skipWithoutShasum(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/shasum"); err != nil {
		t.Skip("no /usr/bin/shasum for the Mac path's checksums")
	}
}

func TestInstallScriptDryRun(t *testing.T) {
	tests := []struct {
		name, sysName, machine string
		env                    map[string]string
		args                   []string
		want, notWant          []string
		wantPool               string
	}{
		{
			name: "mac", sysName: "Darwin", machine: "arm64", env: quietEnv("FACTORY_VERSION", "v1.2.3"),
			want: []string{
				"platform darwin/arm64: the tart driver",
				"https://github.com/ContainerSolutions/factory/releases/download/v1.2.3/factory-agent-darwin-arm64",
				"https://github.com/ContainerSolutions/factory/releases/download/v1.2.3/factory-agent-linux-arm64",
				"/v1.2.3/SHA256SUMS",
				"shasum -a 256 -c -",
				"host init --driver tart --agent-linux ",
				"/.local/bin/factory-agent-linux-arm64 --yes",
				"launchctl bootstrap gui/",
				"Library/LaunchAgents/com.containersolutions.factory-agent.plist",
				"factory-agent login claude",
				"factory-agent login codex",
			},
			notWant:  []string{"--driver kata", "systemctl"},
			wantPool: "local",
		},
		{
			name: "linux", sysName: "Linux", machine: "x86_64", env: quietEnv("FACTORY_POOL", "lab"),
			want: []string{
				"platform linux/amd64: the kata driver",
				"https://github.com/ContainerSolutions/factory/releases/latest/download/factory-agent-linux-amd64",
				"/releases/latest/download/SHA256SUMS",
				"sha256sum -c -",
				"host init --driver kata --yes",
				"systemctl --user enable --now factory-agent.service",
				".config/systemd/user/factory-agent.service",
			},
			notWant:  []string{"factory-agent-linux-arm64", "--agent-linux", "launchctl"},
			wantPool: "lab",
		},
		{
			name: "linux without the service", sysName: "Linux", machine: "x86_64", env: quietEnv(),
			args:     []string{"--no-service"},
			want:     []string{"--driver kata", "--no-service: run supervise yourself"},
			notWant:  []string{"systemctl"},
			wantPool: "local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := installRun{sysName: tt.sysName, machine: tt.machine, env: tt.env, args: append([]string{"--dry-run", "--quiet"}, tt.args...)}.do(t)
			if res.code != 0 {
				t.Fatalf("exit %d\n%s", res.code, res.out)
			}
			for _, w := range tt.want {
				if !strings.Contains(res.out, w) {
					t.Errorf("the plan doesn't mention %q\n%s", w, res.out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(res.out, w) {
					t.Errorf("the plan mentions %q\n%s", w, res.out)
				}
			}
			cfg := planned(t, res)
			if cfg.Dispatcher != "https://factory.example.dev" || cfg.Pool != tt.wantPool || cfg.Host != "my-host" ||
				cfg.EnrollmentTokenFile != filepath.Join(res.home, ".config/factory-agent/enrollment-token") {
				t.Errorf("dispatcher %q, pool %q, host %q, enrollmentTokenFile %q", cfg.Dispatcher, cfg.Pool, cfg.Host, cfg.EnrollmentTokenFile)
			}
			checkDefaults(t, cfg)
		})
	}
}

// TestInstallScriptDefaults checks the script's defaults table against agent.yaml.example.
func TestInstallScriptDefaults(t *testing.T) {
	src, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^default_([a-z_]+)=(\S+)$`).FindAllStringSubmatch(string(src), -1) {
		defaults[m[1]] = m[2]
	}
	ex := example(t)
	size := (config.Workspace{}).WithDefaults("linux").Size
	join := func(l []string) string { return strings.Join(l, ",") }
	codex, claude := ex.Harnesses[config.HarnessCodex], ex.Harnesses[config.HarnessClaudeCode]
	want := map[string]string{
		"pool":           ex.Pool,
		"harnesses":      "codex,claude-code", // the example's two (checkDefaults compares the maps)
		"codex_models":   join(codex.Models),
		"codex_default":  codex.Default,
		"codex_efforts":  join(codex.Efforts),
		"claude_models":  join(claude.Models),
		"claude_default": claude.Default,
		"claude_efforts": join(claude.Efforts),
		"max_concurrent": strconv.Itoa(ex.MaxConcurrent()),
		"cpus":           strconv.Itoa(size.CPUs),
		"memory":         size.Memory,
		"disk":           size.Disk,
	}
	if !reflect.DeepEqual(defaults, want) {
		t.Errorf("install.sh's defaults %v\nagent.yaml.example's %v", defaults, want)
	}
}

func TestInstallScriptWrites(t *testing.T) {
	skipWithoutShasum(t)
	tests := []struct {
		name  string
		env   map[string]string
		args  []string
		check func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "the defaults", env: quietEnv(),
			check: func(t *testing.T, cfg *config.Config) {
				if cfg.Dispatcher != "https://factory.example.dev" || cfg.Pool != "local" || cfg.Host != "my-host" {
					t.Errorf("dispatcher %q, pool %q, host %q", cfg.Dispatcher, cfg.Pool, cfg.Host)
				}
				checkDefaults(t, cfg)
			},
		},
		{
			// The codex default isn't given and gpt-6.1-sol isn't a model: it is the first model.
			name: "overrides", env: quietEnv("FACTORY_MAX_CONCURRENT", "3", "FACTORY_CLAUDE_MODELS", "claude-haiku-5-5"),
			args: []string{
				"--harnesses", "claude-code,codex", "--claude-models", "claude-opus-5-5,claude-fable-5-1", "--claude-default=claude-fable-5-1",
				"--claude-efforts", "high,max", "--codex-models", "gpt-5.5,gpt-6.2", "--codex-efforts=medium", "--max-concurrent", "2",
				"--cpus", "4", "--memory", "6Gi", "--host", "lab-1", "--dispatcher", "http://127.0.0.1:8800",
			},
			check: func(t *testing.T, cfg *config.Config) {
				want := map[string]config.Harness{
					config.HarnessClaudeCode: {Models: []string{"claude-opus-5-5", "claude-fable-5-1"}, Efforts: []string{"high", "max"}, Default: "claude-fable-5-1"},
					config.HarnessCodex:      {Models: []string{"gpt-5.5", "gpt-6.2"}, Efforts: []string{"medium"}, Default: "gpt-5.5"},
				}
				if !reflect.DeepEqual(cfg.Harnesses, want) {
					t.Errorf("harnesses %+v, want %+v", cfg.Harnesses, want)
				}
				if cfg.Dispatcher != "http://127.0.0.1:8800" || cfg.Host != "lab-1" || cfg.MaxConcurrent() != 2 || !cfg.Lending.Unlimited {
					t.Errorf("dispatcher %q, host %q, lending %+v", cfg.Dispatcher, cfg.Host, cfg.Lending)
				}
				if cfg.Workspace == nil || cfg.Workspace.Size != (config.WorkspaceSize{CPUs: 4, Memory: "6Gi", Disk: "60Gi"}) {
					t.Errorf("workspace %+v", cfg.Workspace)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := installRun{sysName: "Darwin", machine: "arm64", env: tt.env, args: append([]string{"--quiet", "--no-service"}, tt.args...), writes: true}.do(t)
			if res.code != 0 {
				t.Fatalf("exit %d\n%s", res.code, res.out)
			}
			tt.check(t, written(t, res.home))
			checkSecret(t, filepath.Join(res.home, ".config/factory-agent/enrollment-token"), testToken)
			if !strings.Contains(res.agentRan, "factory-agent-darwin-arm64 host init --driver tart --agent-linux "+res.home+"/.local/bin/factory-agent-linux-arm64 --yes") {
				t.Errorf("the agent ran %q, want host init --yes", res.agentRan)
			}
			if strings.Contains(res.agentRan, "login") {
				t.Errorf("--quiet ran a login: %q", res.agentRan)
			}
		})
	}
}

// checkSecret fails unless path holds want on one line, mode 0600.
func checkSecret(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); info.Mode().Perm() != 0o600 || string(b) != want+"\n" {
		t.Errorf("%s has mode %v, want 0600 and the token", path, info.Mode().Perm())
	}
}

func TestInstallScriptEnrollmentTokenFile(t *testing.T) {
	skipWithoutShasum(t)
	src := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(src, []byte(testToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := installRun{
		sysName: "Darwin", machine: "arm64", writes: true,
		args: []string{"--quiet", "--no-service", "--dispatcher", "https://factory.example.dev", "--enrollment-token-file", src},
	}.do(t)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.out)
	}
	checkSecret(t, filepath.Join(res.home, ".config/factory-agent/enrollment-token"), testToken)
	written(t, res.home)
}

// TestInstallScriptEnrolled: a host whose state dir holds a refresh token needs no enrollment token.
func TestInstallScriptEnrolled(t *testing.T) {
	skipWithoutShasum(t)
	enrolled := func(t *testing.T, home string) {
		state := filepath.Join(home, "Library/Application Support/factory-agent")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "agent-refresh-token"), []byte("r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := installRun{sysName: "Darwin", machine: "arm64", writes: true, home: enrolled, env: map[string]string{"FACTORY_DISPATCHER": "https://f.example.dev"}, args: []string{"--quiet", "--no-service"}}.do(t)
	if _, err := os.Stat(filepath.Join(res.home, ".config/factory-agent/enrollment-token")); res.code != 0 || err == nil || !strings.Contains(res.out, "already enrolled") {
		t.Errorf("exit %d, the token file: %v\n%s", res.code, err, res.out)
	}
}

func TestInstallScriptExistingConfig(t *testing.T) {
	skipWithoutShasum(t)
	const mine = "# my own agent.yaml\n"
	dir := func(home string) string { return filepath.Join(home, ".config/factory-agent") }
	existing := func(t *testing.T, home string) {
		if err := os.MkdirAll(dir(home), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"agent.yaml": mine, "enrollment-token": "old-token\n"} {
			if err := os.WriteFile(filepath.Join(dir(home), name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("kept under --quiet", func(t *testing.T) {
		// Neither the dispatcher nor a token is needed.
		res := installRun{sysName: "Darwin", machine: "arm64", writes: true, home: existing, args: []string{"--quiet", "--no-service"}}.do(t)
		if res.code != 0 {
			t.Fatalf("exit %d\n%s", res.code, res.out)
		}
		if b, _ := os.ReadFile(filepath.Join(dir(res.home), "agent.yaml")); string(b) != mine {
			t.Errorf("agent.yaml is now %q", b)
		}
		if baks, _ := filepath.Glob(filepath.Join(dir(res.home), "agent.yaml.bak-*")); len(baks) != 0 {
			t.Errorf("backups %v", baks)
		}
		if !strings.Contains(res.out, "keeping it") || strings.Contains(res.out, "--- agent.yaml ---") {
			t.Errorf("the output\n%s", res.out)
		}
		if !strings.Contains(res.agentRan, "host init") {
			t.Errorf("the rest didn't run: %q", res.agentRan)
		}
	})
	t.Run("replaced under --quiet --force", func(t *testing.T) {
		res := installRun{sysName: "Darwin", machine: "arm64", writes: true, home: existing, env: quietEnv(), args: []string{"--quiet", "--force", "--no-service"}}.do(t)
		if res.code != 0 {
			t.Fatalf("exit %d\n%s", res.code, res.out)
		}
		checkDefaults(t, written(t, res.home))
		baks, _ := filepath.Glob(filepath.Join(dir(res.home), "agent.yaml.bak-*"))
		if len(baks) != 1 || !regexp.MustCompile(`agent\.yaml\.bak-\d{8}T\d{6}Z$`).MatchString(baks[0]) {
			t.Fatalf("backups %v", baks)
		}
		if b, _ := os.ReadFile(baks[0]); string(b) != mine {
			t.Errorf("the backup holds %q", b)
		}
		if info, err := os.Stat(baks[0]); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the backup: %v %v", info, err)
		}
		// The existing token file is used, not replaced.
		checkSecret(t, filepath.Join(dir(res.home), "enrollment-token"), "old-token")
	})
}

func TestInstallScriptRefuses(t *testing.T) {
	ok := quietEnv()
	two := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(two, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, sysName, machine string
		env                    map[string]string
		args                   []string
		want                   []string
	}{
		{"an intel mac", "Darwin", "x86_64", ok, nil, []string{"unsupported platform Darwin/amd64"}},
		{"linux on arm", "Linux", "aarch64", ok, nil, []string{"unsupported platform Linux/arm64"}},
		{"a bad version", "Linux", "x86_64", quietEnv("FACTORY_VERSION", "main; rm -rf /"), nil, []string{"is not a vX.Y.Z tag"}},
		{"--yes", "Linux", "x86_64", ok, []string{"--yes"}, []string{"--yes is replaced by --quiet"}},
		{"the token as a flag", "Linux", "x86_64", ok, []string{"--enrollment-token", "x"}, []string{"no flag takes the token itself"}},
		{"an unknown harness", "Linux", "x86_64", ok, []string{"--harnesses", "codex,gemini"}, []string{"--harnesses (FACTORY_HARNESSES): gemini is not one of codex claude-code"}},
		{"an unknown effort", "Linux", "x86_64", ok, []string{"--codex-efforts", "low,huge"}, []string{"--codex-efforts (FACTORY_CODEX_EFFORTS): huge is not one of"}},
		{"a default not in the models", "Linux", "x86_64", ok, []string{"--claude-default", "claude-haiku-5-5"}, []string{"--claude-default (FACTORY_CLAUDE_DEFAULT): claude-haiku-5-5 is not one of the models"}},
		{"too little memory", "Linux", "x86_64", ok, []string{"--memory", "1Gi"}, []string{"--memory (FACTORY_MEMORY): 1Gi: want 2Gi to 64Gi"}},
		{"too many cpus, from the environment", "Linux", "x86_64", quietEnv("FACTORY_CPUS", "17"), nil, []string{"--cpus (FACTORY_CPUS): 17"}},
		{"http off loopback", "Linux", "x86_64", ok, []string{"--dispatcher", "http://factory.example.dev"}, []string{"--dispatcher (FACTORY_DISPATCHER): http://factory.example.dev: want an https URL"}},
		{"--quiet without either", "Linux", "x86_64", nil, nil, []string{"--quiet: missing the dispatcher's URL (--dispatcher URL or FACTORY_DISPATCHER) and the enrollment token (--enrollment-token-file PATH or FACTORY_ENROLLMENT_TOKEN", "factory agent enroll --pool local --owner <your GitHub login> --name my-host", "nothing was changed"}},
		{"a token file of two lines", "Linux", "x86_64", nil, []string{"--dispatcher", "https://f.example.dev", "--enrollment-token-file", two}, []string{"--enrollment-token-file " + two + ": want the token on a single line"}},
		{"--quiet without the token", "Linux", "x86_64", map[string]string{"FACTORY_DISPATCHER": "https://f.example.dev"}, nil, []string{"--quiet: missing the enrollment token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not a dry run: every command that changes something is a failing stub, and HOME
			// must not change, so each refusal comes before any change.
			res := installRun{sysName: tt.sysName, machine: tt.machine, env: tt.env, args: append([]string{"--quiet"}, tt.args...)}.do(t)
			if res.code == 0 {
				t.Fatalf("exit 0, want a refusal\n%s", res.out)
			}
			for _, w := range tt.want {
				if !strings.Contains(res.out, w) {
					t.Errorf("the output doesn't say %q\n%s", w, res.out)
				}
			}
		})
	}
}
