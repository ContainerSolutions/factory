package host

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// testKataConfigSource stands in for the release's configuration-clh-runtime-rs.toml: the
// line the rendering changes, among lines it must leave alone.
const testKataConfigSource = `# Copyright (c) 2022 Intel Corporation
#
# SPDX-License-Identifier: Apache-2.0

[agent.kata]
# reconnect_timeout_ms = 1
  reconnect_timeout_ms = 2
dial_timeout_ms = 30
reconnect_timeout_ms = 3000
`

// writeMode writes a file under e's Root with mode.
func writeMode(t *testing.T, e *Env, p, body string, mode fs.FileMode) {
	t.Helper()
	writeRoot(t, e, p, body)
	if err := os.Chmod(filepath.Join(e.Root, p), mode); err != nil {
		t.Fatal(err)
	}
}

// installKata makes e's host look like a host init --driver kata that succeeded: every kata
// step's files in place and its commands answering.
func installKata(t *testing.T, e *Env, r *fakeRunner) {
	t.Helper()
	nerdctl, containerd := e.sys(katahelper.NerdctlFullDir+"/bin/nerdctl"), e.sys(katahelper.NerdctlFullDir+"/bin/containerd")
	writeMode(t, e, katahelper.NerdctlFullDir+"/bin/nerdctl", "nerdctl", 0o755)
	writeMode(t, e, katahelper.NerdctlFullDir+"/bin/containerd", "containerd", 0o755)
	r.versions[nerdctl] = "nerdctl version " + NerdctlVersion
	r.versions[containerd] = "containerd github.com/containerd/containerd/v2 v" + NerdctlContainerdVersion + " 0123456789abcdef"
	writeMode(t, e, kataMarker, KataVersion+"\n", 0o644)
	writeMode(t, e, kataShim, "shim", 0o755)
	writeMode(t, e, kataConfigSource, testKataConfigSource, 0o644)
	cfg, err := RenderKataConfig([]byte(testKataConfigSource), KataVersion)
	if err != nil {
		t.Fatal(err)
	}
	writeMode(t, e, KataConfigPath, cfg, 0o644)
	installServices(t, e, r)
	installPrivileges(t, e, r)
	installImage(t, e, r)
}

// stepCase is one state of a kata step, set up from a host where every step is ok.
type stepCase struct {
	step, name string
	set        func(*testing.T, *Env, *fakeRunner)
	state      State
	detail     string
}

func runStepCases(t *testing.T, tests []stepCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.step+"/"+tt.name, func(t *testing.T) {
			e, r := kataEnv(t)
			installKata(t, e, r)
			tt.set(t, e, r)
			steps, err := Steps(e)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range steps {
				if s.Name() == tt.step {
					wantState(t, check(t, s), tt.state, tt.detail)
					return
				}
			}
			t.Fatalf("no step %s", tt.step)
		})
	}
}

func TestKataInstallCheck(t *testing.T) {
	runStepCases(t, []stepCase{
		{"nerdctl", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "nerdctl-full 2.4.1, containerd 2.4.1"},
		{"nerdctl", "missing", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, katahelper.NerdctlFullDir) }, StateMissing, "nerdctl-full/bin/nerdctl"},
		{"nerdctl", "another nerdctl", func(_ *testing.T, e *Env, r *fakeRunner) {
			r.versions[e.sys(katahelper.NerdctlFullDir+"/bin/nerdctl")] = "nerdctl version 2.3.0"
		}, StateOutdated, `is nerdctl "2.3.0", want 2.4.1`},
		{"nerdctl", "another containerd", func(_ *testing.T, e *Env, r *fakeRunner) {
			r.versions[e.sys(katahelper.NerdctlFullDir+"/bin/containerd")] = "containerd github.com/containerd/containerd/v2 v2.2.0 abc"
		}, StateOutdated, `is containerd "v2.2.0", want v2.4.1 (nerdctl-full 2.4.1)`},
		{"nerdctl", "nerdctl doesn't run", func(_ *testing.T, e *Env, r *fakeRunner) {
			delete(r.versions, e.sys(katahelper.NerdctlFullDir+"/bin/nerdctl"))
		}, StateWrong, "nerdctl --version: exec format error"},

		{"kata", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "Kata 4.2.0"},
		{"kata", "missing", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, katahelper.KataDir) }, StateMissing, "/opt/kata"},
		{"kata", "another install", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, kataMarker) }, StateWrong,
			"has no .factory-agent marker: it's another Kata install. Move it away; host init never replaces it"},
		{"kata", "a symlink", func(t *testing.T, e *Env, _ *fakeRunner) {
			removeRoot(t, e, katahelper.KataDir)
			if err := os.Symlink(t.TempDir(), e.sys(katahelper.KataDir)); err != nil {
				t.Fatal(err)
			}
		}, StateWrong, "is not a directory: another Kata install?"},
		{"kata", "an older version", func(t *testing.T, e *Env, _ *fakeRunner) { writeRoot(t, e, kataMarker, "4.1.0\n") }, StateOutdated,
			`is Kata "4.1.0", want 4.2.0`},
		{"kata", "no shim", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, kataShim) }, StateWrong, "containerd-shim-kata-v2 is not an executable file"},
		{"kata", "the shim not executable", func(t *testing.T, e *Env, _ *fakeRunner) {
			if err := os.Chmod(e.sys(kataShim), 0o644); err != nil {
				t.Fatal(err)
			}
		}, StateWrong, "is not an executable file"},

		{"kataconfig", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "reconnect_timeout_ms = 60000"},
		{"kataconfig", "missing", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, KataConfigPath) }, StateMissing, KataConfigPath},
		{"kataconfig", "another install's", func(t *testing.T, e *Env, _ *fakeRunner) { writeRoot(t, e, KataConfigPath, "[hypervisor.clh]\n") }, StateWrong,
			"is another Kata install's config: move it away"},
		{"kataconfig", "drifted", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, KataConfigPath, kataConfigFirstLine(KataVersion)+"\nreconnect_timeout_ms = 3000\n")
		}, StateWrong, "differs from the rendering of"},
		{"kataconfig", "Kata upgraded", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, kataConfigSource, testKataConfigSource+"# a new setting\n")
		}, StateWrong, "differs from the rendering of"},
		{"kataconfig", "no source", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, kataConfigSource) }, StateWrong,
			"configuration-clh-runtime-rs.toml to render it from (the kata step installs it)"},
		{"kataconfig", "a source without the line", func(t *testing.T, e *Env, _ *fakeRunner) { writeRoot(t, e, kataConfigSource, "[agent.kata]\n") }, StateWrong,
			"has no reconnect_timeout_ms line"},
	})
}

// TestKataStepsAllOK: a host where host init succeeded checks ok throughout.
func TestKataStepsAllOK(t *testing.T) {
	e, r := kataEnv(t)
	installKata(t, e, r)
	if err := os.MkdirAll(e.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	steps, err := Steps(e)
	if err != nil {
		t.Fatal(err)
	}
	if sts := Check(context.Background(), steps); !AllOK(sts) {
		t.Errorf("statuses %+v", sts)
	}
}

// TestKataApply: the downloads are sha256-checked and staged; a mismatch stages nothing; a
// foreign /opt/kata or Kata config stops the apply.
func TestKataApply(t *testing.T) {
	nerdctlTar, kataTar := []byte("nerdctl-full tarball"), []byte("kata static tarball")
	setup := func(t *testing.T) (*Env, *bytes.Buffer) {
		e, _ := kataEnv(t)
		var out bytes.Buffer
		e.Out = &out
		e.Pins.NerdctlSHA256, e.Pins.KataSHA256 = sum(nerdctlTar), sum(kataTar)
		e.Fetcher = newReleases(t, map[string][]byte{
			filepath.Base(NerdctlURL): nerdctlTar, filepath.Base(KataURL): kataTar,
		})
		return e, &out
	}
	t.Run("staged", func(t *testing.T) {
		e, out := setup(t)
		for _, s := range []Step{nerdctlStep{e}, kataStep{e}, kataConfigStep{e}} {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatalf("%s: %v", s.Name(), err)
			}
		}
		for name, want := range map[string]string{"nerdctl-full.tar.gz": string(nerdctlTar), "kata-static.tar.zst": string(kataTar)} {
			b, err := os.ReadFile(filepath.Join(e.StageDir, name))
			if err != nil || string(b) != want {
				t.Errorf("staged %s: %q %v", name, b, err)
			}
		}
		if !strings.Contains(out.String(), "downloading "+KataURL+"\n") {
			t.Errorf("output %q", out)
		}
		left, _ := filepath.Glob(filepath.Join(e.StageDir, ".download-*"))
		if len(left) > 0 {
			t.Errorf("download dirs left: %v", left)
		}
	})
	for _, step := range []string{"nerdctl", "kata"} {
		t.Run(step+" sha256 mismatch", func(t *testing.T) {
			e, _ := setup(t)
			e.Pins.NerdctlSHA256, e.Pins.KataSHA256 = sum([]byte("x")), sum([]byte("y"))
			s := map[string]Step{"nerdctl": nerdctlStep{e}, "kata": kataStep{e}}[step]
			if err := s.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "want the pinned") {
				t.Fatalf("Apply: %v", err)
			}
			if entries, _ := os.ReadDir(e.StageDir); len(entries) > 0 {
				t.Errorf("a mismatch left %v in the stage", entries)
			}
		})
	}
	t.Run("another /opt/kata", func(t *testing.T) {
		e, _ := setup(t)
		writeRoot(t, e, katahelper.KataDir+"/bin/kata-runtime", "x")
		if err := (kataStep{e}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "another Kata install") {
			t.Fatalf("Apply: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.StageDir, "kata-static.tar.zst")); err == nil {
			t.Error("downloaded Kata for a host with another install")
		}
	})
	t.Run("another Kata config", func(t *testing.T) {
		e, _ := setup(t)
		writeRoot(t, e, KataConfigPath, "[hypervisor.qemu]\n")
		if err := (kataConfigStep{e}).Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "another Kata install's config") {
			t.Fatalf("Apply: %v", err)
		}
	})
}

// TestKataInitDryRun: the plan and the sudo script of a host with nothing installed (golden);
// the script parses; nothing is staged or made, and the stage is under the state dir.
func TestKataInitDryRun(t *testing.T) {
	e, r := kataEnv(t)
	var out bytes.Buffer
	e.Out, e.StageDir = &out, ""
	sts, err := Init(context.Background(), e, InitOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(e.StateDir(), "stage")
	e.StageDir = stage
	golden(t, "kata-init-dry-run.golden", e, out.String())
	_, script, _ := strings.Cut(out.String(), "sudo script (one sudo prompt):\n")
	if b, err := exec.Command("/bin/sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, b)
	}
	steps, _ := Steps(e)
	if r.ran("sudo") != 0 || len(sts) != len(steps) || AllOK(sts[2:]) || !strings.Contains(script, stage+"/nerdctl-full.tar.gz") {
		t.Errorf("sudo calls %d, statuses %+v\n%s", r.ran("sudo"), sts, script)
	}
	if _, err := os.Stat(e.StateDir()); err == nil {
		t.Error("--dry-run made the state dir")
	}
}

// TestKataInit: one sudo, after every step is staged in <statedir>/stage, which a killed
// init's leftovers don't stop and which is gone afterwards.
func TestKataInit(t *testing.T) {
	nerdctlTar, kataTar := []byte("nerdctl-full tarball"), []byte("kata static tarball")
	e, r := kataEnv(t)
	e.StageDir = ""
	e.Pins.NerdctlSHA256, e.Pins.KataSHA256 = sum(nerdctlTar), sum(kataTar)
	e.Fetcher = newReleases(t, map[string][]byte{filepath.Base(NerdctlURL): nerdctlTar, filepath.Base(KataURL): kataTar})
	stage := filepath.Join(e.StateDir(), "stage")
	if err := os.MkdirAll(filepath.Join(stage, "left"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(context.Background(), e, InitOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if r.ran("sudo /bin/sh "+filepath.Join(stage, "root.sh")) != 1 {
		t.Fatalf("calls %v, want one sudo of the script in %s", r.calls, stage)
	}
	for _, want := range []string{stage + "/nerdctl-full.tar.gz", stage + "/kata-static.tar.zst", katahelper.NerdctlFullDir, KataConfigPath} {
		if !strings.Contains(r.script, want) {
			t.Errorf("the script has no %s", want)
		}
	}
	if _, err := os.Stat(stage); err == nil || e.StageDir != "" {
		t.Errorf("the stage %s is left (StageDir %q)", stage, e.StageDir)
	}
}

// TestRenderKataConfig: the Go rendering is the root lines' sed's, byte for byte, where sed
// exists; a source without the line is an error.
func TestRenderKataConfig(t *testing.T) {
	if _, err := RenderKataConfig([]byte("[agent.kata]\n"), KataVersion); err == nil {
		t.Error("a source without reconnect_timeout_ms rendered")
	}
	for _, src := range []string{
		testKataConfigSource,
		strings.TrimSuffix(testKataConfigSource, "\n"),         // no final newline
		"reconnect_timeout_ms\t=  3000 # a comment\r\nx = 1\n", // tabs, a comment, CRLF
	} {
		got, err := RenderKataConfig([]byte(src), KataVersion)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := strings.CutPrefix(got, kataConfigFirstLine(KataVersion)+"\n")
		if !ok || strings.Count(body, "reconnect_timeout_ms = 60000") != 1 {
			t.Errorf("rendering of %q:\n%s", src, got)
		}
		sed, err := exec.LookPath("sed")
		if err != nil {
			continue
		}
		p := filepath.Join(t.TempDir(), "src.toml")
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := exec.Command(sed, "-E", `s/^(reconnect_timeout_ms)[[:space:]]*=.*/\1 = 60000/`, p).Output()
		if err != nil {
			t.Skipf("sed -E: %v", err)
		}
		if want := kataConfigFirstLine(KataVersion) + "\n" + string(b); got != want {
			t.Errorf("Go and sed differ for %q:\n%q\n%q", src, got, want)
		}
	}
}
