package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// installPrivileges puts the helper (speaking this protocol) and the owner's sudoers rule in
// place.
func installPrivileges(t *testing.T, e *Env, r *fakeRunner) {
	t.Helper()
	writeMode(t, e, katahelper.HelperPath, "factory-agent", 0o755)
	writeMode(t, e, SudoersPath, SudoersRule("owner"), 0o440)
	r.helper = fmt.Sprint(katahelper.Protocol)
}

func TestKataPrivilegesCheck(t *testing.T) {
	runStepCases(t, []stepCase{
		{"privileges", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "lets owner run it with sudo -n"},
		{"privileges", "no helper", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, katahelper.HelperPath) }, StateMissing, "kata-helper"},
		{"privileges", "the helper is the owner's", func(_ *testing.T, e *Env, _ *fakeRunner) {
			e.Owner = func(fs.FileInfo) (int, int, bool) { return 1000, 1000, true }
		}, StateWrong, "kata-helper is owned by 1000:1000, want root:root"},
		{"privileges", "the helper setuid", func(t *testing.T, e *Env, _ *fakeRunner) {
			if err := os.Chmod(e.sys(katahelper.HelperPath), 0o755|fs.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}, StateWrong, "kata-helper has mode urwxr-xr-x, want -rwxr-xr-x"},
		{"privileges", "the helper group-writable", func(t *testing.T, e *Env, _ *fakeRunner) {
			if err := os.Chmod(e.sys(katahelper.HelperPath), 0o775); err != nil {
				t.Fatal(err)
			}
		}, StateWrong, "want -rwxr-xr-x"},
		{"privileges", "another protocol", func(_ *testing.T, _ *Env, r *fakeRunner) { r.helper = "0" }, StateOutdated,
			fmt.Sprintf(`kata-helper speaks protocol "0", this factory-agent %d: host init installs this one`, katahelper.Protocol)},
		{"privileges", "the helper doesn't run", func(_ *testing.T, _ *Env, r *fakeRunner) { r.helper = "" }, StateWrong, "kata-helper protocol: exec format error"},
		{"privileges", "no sudoers rule", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, SudoersPath) }, StateMissing, SudoersPath},
		{"privileges", "a drifted rule", func(t *testing.T, e *Env, _ *fakeRunner) {
			removeRoot(t, e, SudoersPath)
			writeMode(t, e, SudoersPath, SudoersRule("other"), 0o440)
		}, StateWrong, "factory-agent differs from the rendered rule for owner"},
		{"privileges", "a rule with mode 0644", func(t *testing.T, e *Env, _ *fakeRunner) {
			if err := os.Chmod(e.sys(SudoersPath), 0o644); err != nil {
				t.Fatal(err)
			}
		}, StateWrong, "has mode -rw-r--r--, want -r--r-----"},
		{"privileges", "sudo -n -l refused", func(_ *testing.T, _ *Env, r *fakeRunner) { r.sudoErr = errors.New("exit status 1") }, StateWrong,
			"the sudoers rule doesn't let you run the helper (sudo -n -l /usr/local/libexec/factory-agent/kata-helper: exit status 1: sudo: a password is required)"},
		{"privileges", "a bad owner name", func(_ *testing.T, e *Env, _ *fakeRunner) {
			e.Username = func() (string, error) { return "Owner ALL=(ALL) ALL", nil }
		}, StateWrong, `the user name "Owner ALL=(ALL) ALL" can't go in the sudoers rule`},
		{"privileges", "root", func(_ *testing.T, e *Env, _ *fakeRunner) { e.Username = func() (string, error) { return "root", nil } }, StateWrong,
			"host init runs as root: run it as the user who runs supervise"},
		{"privileges", "no user", func(_ *testing.T, e *Env, _ *fakeRunner) {
			e.Username = func() (string, error) { return "", errors.New("unknown userid 1000") }
		}, StateWrong, "the user running host init: unknown userid 1000"},
	})
}

// TestKataPrivilegesCheckRunsNothingAsRoot: the check runs the helper as the owner and only
// lists with sudo, ignoring a cached login.
func TestKataPrivilegesCheckRunsNothingAsRoot(t *testing.T) {
	e, r := kataEnv(t)
	installKata(t, e, r)
	wantState(t, check(t, privilegesStep{e}), StateOK, "")
	if r.ran("sudo") != 1 || r.ran("sudo -n -k -l "+katahelper.HelperPath) != 1 || r.ran(e.sys(katahelper.HelperPath)+" protocol") != 1 {
		t.Errorf("calls %v", r.calls)
	}
}

func TestSudoersMatch(t *testing.T) {
	rule := SudoersRule("owner")
	tests := []struct {
		name string
		b    string
		err  error
		size int64
		ok   bool
		fail bool
	}{
		{"readable, the rule", rule, nil, int64(len(rule)), true, false},
		{"readable, another rule", SudoersRule("ownr"), nil, int64(len(rule)) - 1, false, false},
		{"readable, the same size", SudoersRule("ownes"), nil, int64(len(rule)), false, false},
		{"unreadable, the rule's size", "", fs.ErrPermission, int64(len(rule)), true, false},
		{"unreadable, another size", "", fs.ErrPermission, int64(len(rule)) + 1, false, false},
		{"another error", "", errors.New("i/o error"), int64(len(rule)), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := sudoersMatch([]byte(tt.b), tt.err, tt.size, rule)
			if ok != tt.ok || (err != nil) != tt.fail {
				t.Errorf("sudoersMatch = %v, %v; want %v, error %v", ok, err, tt.ok, tt.fail)
			}
		})
	}
}

// TestPrivilegesApply: the running binary is staged 0700; no executable or a bad owner stops
// the plan and the apply.
func TestPrivilegesApply(t *testing.T) {
	e, _ := kataEnv(t)
	if err := (privilegesStep{e}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.StageDir, katahelper.Name)
	if b, err := os.ReadFile(p); err != nil || string(b) != "factory-agent linux/amd64" {
		t.Errorf("staged helper %q %v", b, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("staged helper %v %v", fi, err)
	}
	e.Executable = ""
	if err := (privilegesStep{e}).Apply(context.Background()); err == nil {
		t.Error("Apply without the executable succeeded")
	}
	if _, err := (privilegesStep{e}).Plan(context.Background()); err == nil {
		t.Error("Plan without the executable succeeded")
	}
	e, _ = kataEnv(t)
	e.Username = func() (string, error) { return "a\nb", nil }
	if _, err := (privilegesStep{e}).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "can't go in the sudoers rule") {
		t.Errorf("Plan: %v", err)
	}
	if err := (privilegesStep{e}).Apply(context.Background()); err == nil {
		t.Error("Apply with a bad owner succeeded")
	}
}

// TestKataInitPrivilegesPrompt: --yes skips the prompt; accepted, the helper's lines are in the
// sudo script and the image is built through it; declined, they're left out, the steps that run
// the helper are skipped with a warning (the final check reports them not ok), and the next init
// asks again; with no terminal, init stops before sudo.
func TestKataInitPrivilegesPrompt(t *testing.T) {
	nerdctlTar, kataTar := []byte("nerdctl-full tarball"), []byte("kata static tarball")
	tests := []struct {
		name       string
		o          InitOptions
		answer     bool
		promptErr  error
		wantHelper bool // the sudo script installs the helper, and the image build runs it
		wantOut    string
		err        string
	}{
		{"yes", InitOptions{Yes: true}, false, nil, true, "plan:", ""},
		{"prompt accepted", InitOptions{}, true, nil, true, "Install the kata helper and its sudoers rule? [y/N]", ""},
		{"prompt declined", InitOptions{}, false, nil, false, "warning: privileges skipped", ""},
		{"no terminal", InitOptions{}, false, errors.New("no terminal to ask"), false, "", "no terminal to ask"},
	}
	skips := []string{
		"warning: kataimage skipped: privileges were declined, and the image is built through the kata helper\n",
		"warning: katawarm skipped: privileges were declined, and the warm disk is built through the kata helper\n",
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := kataEnv(t)
			var out bytes.Buffer
			e.Out = &out
			e.Pins.NerdctlSHA256, e.Pins.KataSHA256 = sum(nerdctlTar), sum(kataTar)
			e.Fetcher = newReleases(t, map[string][]byte{filepath.Base(NerdctlURL): nerdctlTar, filepath.Base(KataURL): kataTar})
			prompts := 0
			e.Prompt = func(q string) (bool, error) { prompts++; fmt.Fprint(&out, q); return tt.answer, tt.promptErr }
			sts, err := Init(context.Background(), e, tt.o)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || r.ran("sudo") != 0 {
					t.Fatalf("err %v, sudo calls %d", err, r.ran("sudo"))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.ran("sudo /bin/sh "+filepath.Join(e.StageDir, "root.sh")) != 1 {
				t.Fatalf("calls %v, want one sudo", r.calls)
			}
			if got := strings.Contains(r.script, SudoersPath); got != tt.wantHelper {
				t.Errorf("the script has the helper %v, want %v", got, tt.wantHelper)
			}
			if got := r.ran("sudo -n "+e.sys(katahelper.HelperPath)+" image-build ") == 1; got != tt.wantHelper {
				t.Errorf("the image build ran the helper %v, want %v; calls %q", got, tt.wantHelper, r.helperCalls())
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output %q, want %q", out.String(), tt.wantOut)
			}
			if got := strings.Contains(out.String(), "a root-owned copy of this factory-agent"); got == tt.o.Yes {
				t.Errorf("the notice printed %v with --yes %v: output %q", got, tt.o.Yes, out.String())
			}
			for _, w := range skips {
				if got := strings.Contains(out.String(), w); got == tt.wantHelper {
					t.Errorf("output has %q: %v, want %v; output %q", w, got, !tt.wantHelper, out.String())
				}
			}
			if tt.wantHelper {
				return
			}
			// declined: no helper call at all, the final check says the helper's steps aren't ok
			// (so does host check), and nothing remembers the answer: the next init asks again.
			if calls := r.ran("sudo -n " + e.sys(katahelper.HelperPath)); calls != 0 {
				t.Errorf("%d helper calls, want none: %q", calls, r.helperCalls())
			}
			for _, st := range sts {
				switch st.Step {
				case privilegesName, kataImageName, KataWarmStep:
					if st.State == StateOK || st.State == StateSkipped {
						t.Errorf("final check %+v, want not ok", st)
					}
				}
			}
			if AllOK(sts) {
				t.Error("AllOK after a declined prompt")
			}
			if _, err := Init(context.Background(), e, tt.o); err != nil || prompts != 2 {
				t.Errorf("the second init: %v, %d prompts, want 2", err, prompts)
			}
		})
	}
}

// TestSudoersRuleVisudo: the rule passes visudo -cf where visudo exists, and fits a heredoc.
func TestSudoersRuleVisudo(t *testing.T) {
	if _, err := heredoc(`"$T/sudoers"`, SudoersRule("owner")); err != nil {
		t.Fatal(err)
	}
	bin, err := exec.LookPath("visudo")
	if err != nil {
		if bin, err = exec.LookPath("/usr/sbin/visudo"); err != nil {
			t.Skip("no visudo")
		}
	}
	p := filepath.Join(t.TempDir(), "sudoers")
	if err := os.WriteFile(p, []byte(SudoersRule("owner")), 0o440); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(bin, "-cf", p).CombinedOutput(); err != nil {
		t.Errorf("visudo -cf: %v\n%s", err, b)
	}
}

// TestKataInitHelperRequired: an --only that selects a helper step that isn't ok, but not a
// privileges step that isn't ok, fails before any prompt, plan or apply (--dry-run and --yes
// too), naming the steps and the privileges status; with the helper ok, or privileges selected,
// or no helper step selected, or no --only at all, init runs (privileges first).
func TestKataInitHelperRequired(t *testing.T) {
	var installed string // the agent version installKata built the image and the warm disk for
	noHelper := func(t *testing.T, e *Env, r *fakeRunner) { removeRoot(t, e, katahelper.HelperPath); r.helper = "" }
	outdated := func(_ *testing.T, _ *Env, r *fakeRunner) { r.helper = "0" }
	noRule := func(_ *testing.T, _ *Env, r *fakeRunner) { r.sudoErr = errors.New("exit status 1") }
	helperOK := func(*testing.T, *Env, *fakeRunner) {}
	// the image and the warm disk current (the other cases outdate them), the helper out of date
	current := func(_ *testing.T, e *Env, r *fakeRunner) { e.AgentVersion = installed; r.helper = "0" }
	tests := []struct {
		name    string
		set     func(*testing.T, *Env, *fakeRunner)
		o       InitOptions
		err     string
		steps   []string // the error's steps
		applied []string // the helper verbs init runs, in order (with the sudo script as "script")
	}{
		{"--only kataimage, no helper", noHelper, InitOptions{Only: []string{"kataimage"}},
			"kataimage needs the kata helper, which isn't installed (privileges missing: ", []string{"kataimage"}, nil},
		{"--only katawarm, no helper", noHelper, InitOptions{Only: []string{"katawarm"}},
			"katawarm needs the kata helper, which isn't installed", []string{"katawarm"}, nil},
		{"--only kataimage,katawarm, an out-of-date helper", outdated, InitOptions{Only: []string{"kataimage", "katawarm"}},
			"kataimage and katawarm need the kata helper, which is out of date (privileges outdated: ",
			[]string{"kataimage", "katawarm"}, nil},
		{"--only kataimage, no sudoers rule", noRule, InitOptions{Only: []string{"kataimage"}},
			"kataimage needs the kata helper, which isn't usable (privileges wrong: the sudoers rule doesn't let you run the helper", []string{"kataimage"}, nil},
		{"--dry-run", noHelper, InitOptions{DryRun: true, Only: []string{"kataimage"}}, "run `factory-agent host init`, or add privileges to --only", []string{"kataimage"}, nil},
		{"--yes", noHelper, InitOptions{Yes: true, Only: []string{"katawarm", "kataimage"}}, "kataimage and katawarm need the kata helper", []string{"kataimage", "katawarm"}, nil},
		{"--only kataimage, the helper ok", helperOK, InitOptions{Only: []string{"kataimage"}}, "", nil, []string{"image-build"}},
		{"--only kataimage, ok, an out-of-date helper", current, InitOptions{Only: []string{"kataimage"}}, "", nil, nil},
		{"--only statedir, no helper", noHelper, InitOptions{Only: []string{"statedir"}}, "", nil, nil},
		{"--only privileges,kataimage, no helper", noHelper, InitOptions{Only: []string{"privileges", "kataimage"}}, "", nil, []string{"script", "image-build"}},
		{"no --only, no helper", noHelper, InitOptions{}, "", nil, []string{"script", "image-build", "create"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := kataEnv(t)
			installKata(t, e, r)
			installed = e.AgentVersion
			e.AgentVersion = "v0.2.0" // kataimage and katawarm are outdated
			tt.set(t, e, r)
			var out bytes.Buffer
			e.Out = &out
			prompts := 0
			e.Prompt = func(string) (bool, error) { prompts++; return true, nil }
			r.calls = nil
			sts, err := Init(context.Background(), e, tt.o)
			var applied []string
			for _, c := range r.calls {
				switch {
				case strings.HasPrefix(c, "sudo /bin/sh "):
					applied = append(applied, "script")
				case strings.HasPrefix(c, "sudo -n "+e.sys(katahelper.HelperPath)+" "):
					if v := strings.Fields(c)[3]; v == "image-build" || v == "create" {
						applied = append(applied, v)
					}
				}
			}
			if tt.err == "" {
				if err != nil {
					t.Fatalf("%v\n%s", err, out.String())
				}
				if strings.Join(applied, " ") != strings.Join(tt.applied, " ") {
					t.Errorf("applied %q, want %q; calls %q", applied, tt.applied, r.calls)
				}
				return
			}
			if !strings.Contains(err.Error(), katahelper.HelperPath) {
				t.Errorf("err %v, want the privileges status's detail", err)
			}
			var he *HelperRequiredError
			if !errors.As(err, &he) || !strings.Contains(err.Error(), tt.err) || strings.Join(he.Steps, ",") != strings.Join(tt.steps, ",") {
				t.Fatalf("err %v, want a HelperRequiredError with %q for %v", err, tt.err, tt.steps)
			}
			// nothing asked, planned or changed: no prompt, no plan, no sudo, no helper call
			if prompts != 0 || out.Len() != 0 || r.ran("sudo /bin/sh") != 0 || r.ran("sudo -n "+e.sys(katahelper.HelperPath)) != 0 {
				t.Errorf("%d prompts, output %q, calls %q", prompts, out.String(), r.calls)
			}
			if steps, _ := Steps(e); len(sts) != len(steps) || AllOK(sts) {
				t.Errorf("statuses %+v, want the initial check", sts)
			}
			if _, err := os.Stat(filepath.Join(e.StateDir(), "host.lock")); err == nil {
				t.Error("init took the host lock")
			}
		})
	}
}
