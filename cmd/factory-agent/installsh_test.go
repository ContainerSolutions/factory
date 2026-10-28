package main

// deploy/install.sh, the agent host installer each release carries (deploy/poc/README.md
// §Agent onboarding): it parses (bash -n, and shellcheck where installed), and its --dry-run
// --yes plan, in a temp HOME with a stub uname, names the platform's assets and driver and a
// config the agent's loader accepts. The script is never run for real: everything it must not
// run in a dry run (curl, install, mkdir, launchctl, systemctl, sudo, the agent) is a stub on
// PATH that records the call and fails, and no file may appear under HOME.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// dryRun runs the script with args in a fresh HOME, uname reporting sysName/machine and the
// forbidden commands stubbed. It returns the exit code, the output and HOME.
func dryRun(t *testing.T, sysName, machine string, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	stubs := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", `case "$1" in -s) echo "$STUB_SYSNAME" ;; -m) echo "$STUB_MACHINE" ;; -n) echo My-Host.local ;; *) exit 2 ;; esac`+"\n")
	for _, name := range forbidden {
		write(name, `echo "`+name+` $*" >> "$STUB_CALLS"; exit 99`+"\n")
	}
	cmd := exec.Command("bash", append([]string{installScript}, args...)...)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + stubs + ":/usr/bin:/bin",
		"STUB_SYSNAME=" + sysName,
		"STUB_MACHINE=" + machine,
		"STUB_CALLS=" + calls,
	}
	for k, v := range env {
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
		t.Errorf("the dry run ran %s", b)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the dry run wrote %s under HOME", e.Name())
	}
	return code, string(out), home
}

func TestInstallScriptDryRun(t *testing.T) {
	const token = "enroll-token-value-never-printed"
	base := map[string]string{
		"FACTORY_DISPATCHER":       "https://factory.example.dev",
		"FACTORY_ENROLLMENT_TOKEN": token,
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name, sysName, machine string
		env                    map[string]string
		args                   []string
		want, notWant          []string
		wantPool               string
	}{
		{
			name: "mac", sysName: "Darwin", machine: "arm64", env: with("FACTORY_VERSION", "v1.2.3"),
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
				"--name my-host",
			},
			notWant:  []string{"--driver kata", "systemctl"},
			wantPool: "local",
		},
		{
			name: "linux", sysName: "Linux", machine: "x86_64", env: with("FACTORY_POOL", "lab"),
			want: []string{
				"platform linux/amd64: the kata driver",
				"https://github.com/ContainerSolutions/factory/releases/latest/download/factory-agent-linux-amd64",
				"/releases/latest/download/SHA256SUMS",
				"sha256sum -c -",
				"host init --driver kata --yes",
				"systemctl --user enable --now factory-agent.service",
				".config/systemd/user/factory-agent.service",
				"--pool lab",
			},
			notWant:  []string{"factory-agent-linux-arm64", "--agent-linux", "launchctl"},
			wantPool: "lab",
		},
		{
			name: "linux without the service", sysName: "Linux", machine: "x86_64", env: with(),
			args:     []string{"--no-service"},
			want:     []string{"--driver kata", "--no-service: run supervise yourself"},
			notWant:  []string{"systemctl"},
			wantPool: "local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, home := dryRun(t, tt.sysName, tt.machine, tt.env, append([]string{"--dry-run", "--yes"}, tt.args...)...)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("the plan doesn't mention %q\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Errorf("the plan mentions %q\n%s", w, out)
				}
			}
			if strings.Contains(out, token) {
				t.Errorf("the plan prints the enrollment token")
			}

			// The config it would write loads with the agent's own loader.
			_, rest, ok := strings.Cut(out, "--- agent.yaml ---\n")
			yaml, _, ok2 := strings.Cut(rest, "--- end agent.yaml ---")
			if !ok || !ok2 {
				t.Fatalf("no agent.yaml in the plan\n%s", out)
			}
			cfg, err := config.Parse([]byte(yaml), config.Options{
				Home:     func() (string, error) { return home, nil },
				Hostname: func() (string, error) { return "other", nil },
			})
			if err != nil {
				t.Fatalf("the agent.yaml doesn't load: %v\n%s", err, yaml)
			}
			if cfg.Dispatcher != "https://factory.example.dev" || cfg.Pool != tt.wantPool || cfg.Host != "my-host" ||
				cfg.EnrollmentTokenFile != filepath.Join(home, ".config/factory-agent/enrollment-token") {
				t.Errorf("dispatcher %q, pool %q, host %q, enrollmentTokenFile %q", cfg.Dispatcher, cfg.Pool, cfg.Host, cfg.EnrollmentTokenFile)
			}
			// Its harnesses and lending are the example's (agentExample, deploykit_test.go).
			example, err := os.ReadFile(agentExample)
			if err != nil {
				t.Fatal(err)
			}
			want, err := config.Parse(example, exampleOptions())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Harnesses, want.Harnesses) || !reflect.DeepEqual(cfg.Lending, want.Lending) {
				t.Errorf("harnesses %+v, lending %+v; agent.yaml.example has %+v, %+v", cfg.Harnesses, cfg.Lending, want.Harnesses, want.Lending)
			}
		})
	}
}

func TestInstallScriptRefuses(t *testing.T) {
	tests := []struct {
		name, sysName, machine string
		env                    map[string]string
		want                   string
	}{
		{"an intel mac", "Darwin", "x86_64", map[string]string{"FACTORY_DISPATCHER": "https://f.example.dev", "FACTORY_ENROLLMENT_TOKEN": "t"}, "unsupported platform Darwin/amd64"},
		{"linux on arm", "Linux", "aarch64", map[string]string{"FACTORY_DISPATCHER": "https://f.example.dev", "FACTORY_ENROLLMENT_TOKEN": "t"}, "unsupported platform Linux/arm64"},
		{"--yes without the dispatcher", "Linux", "x86_64", map[string]string{"FACTORY_ENROLLMENT_TOKEN": "t"}, "--yes: set FACTORY_DISPATCHER"},
		{"--yes without the token", "Linux", "x86_64", map[string]string{"FACTORY_DISPATCHER": "https://f.example.dev"}, "--yes: set FACTORY_ENROLLMENT_TOKEN"},
		{"a bad version", "Linux", "x86_64", map[string]string{"FACTORY_VERSION": "main; rm -rf /"}, "is not a vX.Y.Z tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, _ := dryRun(t, tt.sysName, tt.machine, tt.env, "--dry-run", "--yes")
			if code == 0 || !strings.Contains(out, tt.want) {
				t.Errorf("exit %d, want non-zero and %q\n%s", code, tt.want, out)
			}
		})
	}
}
