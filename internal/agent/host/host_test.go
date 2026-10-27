package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/katahelper"
	"github.com/compliance-framework/factory/internal/clock"
)

var update = flag.Bool("update", false, "rewrite the goldens under testdata/")

// fakeRunner answers the commands the steps run; sudo and any other command are recorded.
// It is also the Spawner, and fakes tart's VMs (vm_test.go).
type fakeRunner struct {
	mu       sync.Mutex
	calls    []string
	tart     fakeTart
	versions map[string]string // tart binary -> its --version
	team     string            // codesign -dv's TeamIdentifier
	verify   error             // codesign --verify's error (a bad signature)
	loaded   bool              // launchctl print succeeds
	launchd  string            // launchctl print's output
	macOS    string
	memory   string
	freeKB   string
	routes   string // ip -4 route show's output
	script   string // the sudo script's content when sudo ran
	// the kata steps': systemctl's unit states (is-enabled, is-active)
	units map[string][2]string
	// kata-helper protocol's output, and sudo -n -k -l's error (nil: it lists the helper)
	helper  string
	sudoErr error
	// kata answers sudo -n kata-helper's verbs (kataimage_test.go) once the helper is installed
	// (helper set, as installPrivileges or the sudo script that installs the sudoers rule does)
	kata fakeKata
}

// helperRefused is sudo -n's answer to a kata-helper call when the host has no helper and
// sudoers rule (helper unset), or the rule doesn't cover the call (sudoErr); nil when sudo runs
// the helper.
func (r *fakeRunner) helperRefused(out io.Writer) error {
	if r.helper != "" && r.sudoErr == nil {
		return nil
	}
	_, _ = io.WriteString(out, "sudo: a password is required\n")
	if r.sudoErr != nil {
		return r.sudoErr
	}
	return errors.New("exit status 1")
}

// isHelper reports whether a command is sudo -n kata-helper <verb>.
func isHelper(name string, args []string) bool {
	return name == "sudo" && len(args) > 2 && args[0] == "-n" && path.Base(args[1]) == "kata-helper"
}

func (r *fakeRunner) Run(_ context.Context, out io.Writer, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	switch {
	case path.Base(name) == "tart" && len(args) > 0 && args[0] != "--version":
		return r.tart.run(call, out, nil, args)
	case name == "sw_vers":
		_, _ = io.WriteString(out, r.macOS+"\n")
	case name == "sysctl":
		_, _ = io.WriteString(out, r.memory+"\n")
	case name == "ip":
		_, _ = io.WriteString(out, r.routes)
	case name == "df":
		_, _ = fmt.Fprintf(out, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk3s5 482797652 1 %s 79%% /\n", r.freeKB)
	case len(args) == 1 && args[0] == "--version":
		v, ok := r.versions[name]
		if !ok {
			return errors.New("exec format error")
		}
		_, _ = io.WriteString(out, v+"\n")
	case name == "codesign" && args[0] == "-dv":
		_, _ = fmt.Fprintf(out, "Executable=%s\nTeamIdentifier=%s\n", args[1], r.team)
	case name == "codesign" && args[0] == "--verify":
		return r.verify
	case name == "launchctl" && !r.loaded:
		return errors.New("exit status 113")
	case name == "launchctl":
		_, _ = io.WriteString(out, r.launchd)
	case name == "systemctl" && len(args) == 2:
		st := r.units[args[1]]
		i := map[string]int{"is-enabled": 0, "is-active": 1}[args[0]]
		if st[i] == "" {
			st[i] = map[int]string{0: "Failed to get unit file state for " + args[1] + ": No such file or directory", 1: "inactive"}[i]
		}
		_, _ = io.WriteString(out, st[i]+"\n")
		if st[i] != "enabled" && st[i] != "active" {
			return errors.New("exit status 1")
		}
	case isHelper(name, args):
		if err := r.helperRefused(out); err != nil {
			return err
		}
		return r.kata.run(call, out, nil, args[2:])
	case path.Base(name) == "kata-helper" && len(args) == 1 && args[0] == "protocol":
		if r.helper == "" {
			return errors.New("exec format error")
		}
		_, _ = io.WriteString(out, r.helper+"\n")
	case name == "sudo" && len(args) > 0 && args[0] == "-n":
		if r.sudoErr != nil {
			_, _ = io.WriteString(out, "sudo: a password is required\n")
			return r.sudoErr
		}
		_, _ = io.WriteString(out, args[len(args)-1]+"\n")
	case name == "sudo":
		b, err := os.ReadFile(args[1])
		r.script = string(b)
		if err == nil && strings.Contains(r.script, SudoersPath) && r.helper == "" {
			r.helper = fmt.Sprint(katahelper.Protocol) // the script installed the helper
		}
		return err
	default:
		return fmt.Errorf("unexpected command %s", name)
	}
	return nil
}

func (r *fakeRunner) ran(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeReleases serves tarballs by base name over httptest; Fetch rewrites the pinned URLs to it.
type fakeReleases struct {
	srv     *httptest.Server
	fetched []string
}

func newReleases(t *testing.T, files map[string][]byte) *fakeReleases {
	f := &fakeReleases{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[path.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReleases) Fetch(ctx context.Context, url, dst string) error {
	f.fetched = append(f.fetched, url)
	return HTTPFetcher{Client: f.srv.Client()}.Fetch(ctx, f.srv.URL+"/"+path.Base(url), dst)
}

type entry struct {
	name, body, link string
	typ              byte
	size             int64 // header size when body is short (oversize tests)
	mode             int64 // header mode; 0 is 0o755
}

func tgz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body))}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if e.size > 0 {
			h.Size = e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Flush() // fails for an oversize entry: its header is enough
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// testEnv is a Mac that has nothing installed: system paths under a temp Root.
func testEnv(t *testing.T) (*Env, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, PFConfPath), []byte("anchor \"com.apple/*\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{versions: map[string]string{}, team: TartTeamID, macOS: "26.6.2", memory: "25769803776", freeKB: "98351052"}
	r.tart.init()
	home := t.TempDir()
	agent := filepath.Join(home, "bin", AgentLinuxName)
	if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("factory-agent linux/arm64"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Env{
		Workspace: config.Workspace{}.WithDefaults("darwin"),
		Runner:    r, Spawner: r, Root: root, Home: home, StageDir: t.TempDir(), GOOS: "darwin", GOARCH: "arm64",
		UID:          func() int { return 501 },
		Owner:        func(fs.FileInfo) (int, int, bool) { return 501, 20, true },
		LookPath:     func(string) (string, error) { return "", errors.New("not found") },
		AgentVersion: "v0.1.0", AgentLinux: agent,
		Clock: clock.NewFake(time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)),
		VM:    VMTimings{Boot: 5 * time.Second, Provision: 5 * time.Second, Shutdown: 5 * time.Second, Poll: time.Millisecond},
		Out:   io.Discard,
	}
	if err := e.Defaults(); err != nil {
		t.Fatal(err)
	}
	e.Fetcher = failFetcher{t}
	return e, r
}

type failFetcher struct{ t *testing.T }

func (f failFetcher) Fetch(context.Context, string, string) error {
	f.t.Error("unexpected download")
	return errors.New("no network in tests")
}

func check(t *testing.T, s Step) Status {
	t.Helper()
	st, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("%s.Check: %v", s.Name(), err)
	}
	return st
}

func wantState(t *testing.T, st Status, state State, detail string) {
	t.Helper()
	if st.State != state || !strings.Contains(st.Detail, detail) {
		t.Errorf("status %+v, want %s with %q", st, state, detail)
	}
}

func TestPlatform(t *testing.T) {
	tests := []struct {
		name   string
		set    func(*Env, *fakeRunner)
		state  State
		detail string
	}{
		{"ok", func(*Env, *fakeRunner) {}, StateOK, "macOS 26.6.2, 24 GiB memory, 93 GiB free"},
		{"linux", func(e *Env, _ *fakeRunner) { e.GOOS, e.GOARCH = "linux", "amd64" }, StateWrong, "needs darwin/arm64, this is linux/amd64"},
		{"intel", func(e *Env, _ *fakeRunner) { e.GOARCH = "amd64" }, StateWrong, "needs darwin/arm64"},
		{"old macOS", func(_ *Env, r *fakeRunner) { r.macOS = "15.4" }, StateWrong, "macOS 15.4: Softnet needs 26.0"},
		{"memory", func(e *Env, _ *fakeRunner) { e.Workspace.Size.Memory = "24Gi" }, StateWrong, "24 GiB memory: a 24Gi workspace needs 28 GiB"},
		{"disk", func(_ *Env, r *fakeRunner) { r.freeKB = "1048576" }, StateWrong, "1 GiB free on the state dir's volume, want 80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			tt.set(e, r)
			wantState(t, check(t, platformStep{e}), tt.state, tt.detail)
		})
	}
}

func TestStateDir(t *testing.T) {
	e, _ := testEnv(t)
	s := stateDirStep{e}
	wantState(t, check(t, s), StateMissing, e.StateDir())
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantState(t, check(t, s), StateOK, e.StateDir())
	if err := os.Chmod(e.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	wantState(t, check(t, s), StateWrong, "mode 0755, want 0700")
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantState(t, check(t, s), StateOK, "")
	e.Owner = func(fs.FileInfo) (int, int, bool) { return 0, 0, true }
	wantState(t, check(t, s), StateWrong, "owned by uid 0")
	if err := s.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "fix it by hand") {
		t.Fatalf("Apply on a dir owned by root: %v", err)
	}
}

func tartApp(team string) []entry {
	return []entry{{name: "tart.app/Contents/MacOS/tart", body: "tart binary"}, {name: "tart.app/Contents/Info.plist", body: team}}
}

func TestTartCheck(t *testing.T) {
	tests := []struct {
		name          string
		state, onPath string // versions; "" = not installed
		want          State
		detail        string
	}{
		{"missing", "", "", StateMissing, "no tart"},
		{"pinned in the state dir", TartVersion, "", StateOK, "tart " + TartVersion + " ("},
		{"old in the state dir", "2.30.0", "", StateOutdated, "is 2.30.0, want " + TartVersion},
		{"newer on PATH", "", "2.42.0", StateOK, "tart 2.42.0 on PATH (/opt/homebrew/bin/tart)"},
		{"old on PATH", "", "2.40.0", StateOutdated, `"2.40.0", want ` + TartVersion + " or newer"},
		{"broken on PATH", "", "broken", StateWrong, "/opt/homebrew/bin/tart: exec format error"},
		{"old in the state dir, newer on PATH", "2.30.0", "2.41.1", StateOK, "on PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			s := tartStep{e}
			if tt.state != "" {
				if err := os.MkdirAll(filepath.Dir(s.bin()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.bin(), nil, 0o755); err != nil {
					t.Fatal(err)
				}
				r.versions[s.bin()] = tt.state
			}
			if tt.onPath != "" {
				e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
				if tt.onPath != "broken" { // no version: the fake's --version fails
					r.versions["/opt/homebrew/bin/tart"] = tt.onPath
				}
			}
			wantState(t, check(t, s), tt.want, tt.detail)
		})
	}
}

func TestTartApply(t *testing.T) {
	good := tgz(t, tartApp("ok")...)
	tests := []struct {
		name    string
		archive []byte
		sha     string
		team    string
		verify  error
		wantErr string
	}{
		{"installs", good, sum(good), TartTeamID, nil, ""},
		{"sha256 mismatch", good, strings.Repeat("0", 64), TartTeamID, nil, "sha256 of tart.tar.gz is"},
		{"wrong team", good, sum(good), "ABCDE12345", nil, "isn't signed by team " + TartTeamID + ": TeamIdentifier=ABCDE12345"},
		{"bad signature", good, sum(good), TartTeamID, errors.New("exit status 3"), "tart.app's signature: exit status 3"},
		{"no tart.app", tgz(t, entry{name: "other", body: "x"}), "", TartTeamID, nil, "has no tart.app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			r.team, r.verify = tt.team, tt.verify
			if tt.sha == "" {
				tt.sha = sum(tt.archive)
			}
			e.Pins.TartSHA256 = tt.sha
			e.Fetcher = newReleases(t, map[string][]byte{"tart.tar.gz": tt.archive})
			if err := (stateDirStep{e}).Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			s := tartStep{e}
			err := s.Apply(context.Background())
			left, _ := os.ReadDir(e.StateDir())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if len(left) != 0 {
					t.Errorf("a failed Apply left %v in the state dir", left)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(s.bin()); err != nil || string(b) != "tart binary" {
				t.Fatalf("installed tart: %q %v", b, err)
			}
			// the signature must chain to Apple, with the team's leaf (not only codesign -dv's
			// TeamIdentifier, which a self-made certificate can carry).
			req := `codesign --verify --deep --strict -R =anchor apple generic and certificate leaf[subject.OU] = "` + TartTeamID + `" `
			if len(left) != 1 || r.ran(req) != 1 {
				t.Errorf("state dir %v, calls %v", left, r.calls)
			}
		})
	}
}

func TestSoftnetCheck(t *testing.T) {
	bin := []byte("softnet binary")
	tests := []struct {
		name     string
		mode     fs.FileMode
		uid, gid int
		body     []byte
		want     State
		detail   string
	}{
		{"missing", 0, 0, 0, nil, StateMissing, SoftnetPath},
		{"ok", fs.ModeSetuid | 0o755, 0, 0, bin, StateOK, "softnet " + SoftnetVersion + ", SUID root"},
		{"not SUID", 0o755, 0, 0, bin, StateWrong, "want SUID 4755"},
		{"not root", fs.ModeSetuid | 0o755, 501, 20, bin, StateWrong, "owned by 501:20, want root:wheel"},
		{"other binary", fs.ModeSetuid | 0o755, 0, 0, []byte("0.23"), StateOutdated, "isn't the pinned " + SoftnetVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEnv(t)
			e.Pins.SoftnetBinarySHA256 = sum(bin)
			e.Owner = func(fs.FileInfo) (int, int, bool) { return tt.uid, tt.gid, true }
			if tt.body != nil {
				p := e.sys(SoftnetPath)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, tt.body, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			wantState(t, check(t, softnetStep{e}), tt.want, tt.detail)
		})
	}
}

func TestSoftnetApply(t *testing.T) {
	good := tgz(t, entry{name: "softnet", body: "softnet binary"})
	tests := []struct {
		name, tarSHA, binSHA, wantErr string
	}{
		{"stages the binary", sum(good), sum([]byte("softnet binary")), ""},
		{"tarball sha256 mismatch", strings.Repeat("1", 64), sum([]byte("softnet binary")), "sha256 of softnet.tar.gz is"},
		{"binary sha256 mismatch", sum(good), strings.Repeat("2", 64), "sha256 of softnet is"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEnv(t)
			e.Pins.SoftnetSHA256, e.Pins.SoftnetBinarySHA256 = tt.tarSHA, tt.binSHA
			e.Fetcher = newReleases(t, map[string][]byte{"softnet.tar.gz": good})
			s := softnetStep{e}
			err := s.Apply(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if _, err := os.Stat(s.staged()); err == nil {
					t.Error("a failed Apply staged the binary")
				}
				return
			}
			if b, err := os.ReadFile(s.staged()); err != nil || string(b) != "softnet binary" {
				t.Fatalf("staged %q %v", b, err)
			}
		})
	}
}

// installPF writes what the pf step's root lines would.
func installPF(t *testing.T, e *Env) {
	t.Helper()
	anchor, plist := pfStep{e}.files()
	for p, b := range map[string]string{PFAnchorPath: anchor, PFPlistPath: plist, PFConfPath: "anchor \"com.apple/*\"\n" + pfConfLine + "\n"} {
		if err := os.MkdirAll(filepath.Dir(e.sys(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.sys(p), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPFCheck(t *testing.T) {
	tests := []struct {
		name   string
		set    func(*testing.T, *Env, *fakeRunner)
		want   State
		detail string
	}{
		{"missing", func(*testing.T, *Env, *fakeRunner) {}, StateMissing, PFAnchorPath},
		{"ok", func(t *testing.T, e *Env, r *fakeRunner) { installPF(t, e); r.loaded = true }, StateOK, "only port 3128"},
		{"anchor drifted", func(t *testing.T, e *Env, r *fakeRunner) {
			installPF(t, e)
			r.loaded = true
			e.Workspace.GatewayPort = 8888
		}, StateWrong, "differs from the rendered rules (gateway port 8888)"},
		{"no pf.conf line", func(t *testing.T, e *Env, r *fakeRunner) {
			installPF(t, e)
			r.loaded = true
			_ = os.WriteFile(e.sys(PFConfPath), []byte("anchor \"com.apple/*\"\n"), 0o644)
		}, StateMissing, "has no " + pfConfLine},
		{"not loaded", func(t *testing.T, e *Env, _ *fakeRunner) { installPF(t, e) }, StateMissing, "isn't loaded"},
		{"loaded, not exited yet", func(t *testing.T, e *Env, r *fakeRunner) {
			installPF(t, e)
			r.loaded, r.launchd = true, "\tstate = running\n\tlast exit code = (never exited)\n"
		}, StateOK, "only port 3128"},
		{"loaded, exited 0", func(t *testing.T, e *Env, r *fakeRunner) {
			installPF(t, e)
			r.loaded, r.launchd = true, "\truns = 1\n\tlast exit code = 0\n"
		}, StateOK, "only port 3128"},
		{"pfctl failed", func(t *testing.T, e *Env, r *fakeRunner) {
			installPF(t, e)
			r.loaded, r.launchd = true, "\truns = 1\n\tlast exit code = 1\n"
		}, StateWrong, "last exited with 1: pfctl didn't load the anchor"},
		{"opted out", func(t *testing.T, e *Env, _ *fakeRunner) { optOut(t, e, "date=2026-10-01T08:00:00Z\nreason=--no-pf\n") },
			StateSkipped, "opted out on 2026-10-01T08:00:00Z (--no-pf, "},
		{"opted out, file without fields", func(t *testing.T, e *Env, _ *fakeRunner) { optOut(t, e, "") },
			StateSkipped, "opted out on an unknown date (no reason recorded, "},
		{"opted out, installed since", func(t *testing.T, e *Env, r *fakeRunner) {
			optOut(t, e, "reason=--no-pf\n")
			installPF(t, e)
			r.loaded = true
		}, StateOK, "bridge100 to bridge163 reach only port 3128"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			tt.set(t, e, r)
			wantState(t, check(t, pfStep{e}), tt.want, tt.detail)
		})
	}
}

// optOut writes the pf opt-out file.
func optOut(t *testing.T, e *Env, body string) {
	t.Helper()
	if err := os.MkdirAll(e.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pfOptOutPath(e), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// golden compares got, with the temp dirs replaced, to testdata/<name>.
func golden(t *testing.T, name string, e *Env, got string) {
	t.Helper()
	got = strings.NewReplacer(e.StageDir, "$STAGE", e.Root, "$ROOT", e.Home, "$HOME").Replace(got)
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -update to rewrite):\n%s", p, got)
	}
}

func TestInitDryRun(t *testing.T) {
	e, r := testEnv(t)
	var out bytes.Buffer
	e.Out = &out
	sts, err := Init(context.Background(), e, InitOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "init-dry-run.golden", e, out.String())
	// the script, heredocs and all, parses.
	_, script, _ := strings.Cut(out.String(), "sudo script (one sudo prompt):\n")
	if b, err := exec.Command("/bin/sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, b)
	}
	if r.ran("sudo") != 0 || len(sts) != 7 || AllOK(sts) {
		t.Errorf("sudo calls %d, statuses %+v", r.ran("sudo"), sts)
	}
	if _, err := os.Stat(e.StateDir()); err == nil {
		t.Error("--dry-run made the state dir")
	}
}

func TestInit(t *testing.T) {
	soft := tgz(t, entry{name: "softnet", body: "softnet binary"})
	tests := []struct {
		name    string
		o       InitOptions
		answer  bool
		wantPF  bool
		wantOut string
		noOut   string
	}{
		{"yes", InitOptions{Yes: true}, false, true, "plan:", "Install the pf anchor?"},
		{"prompt accepted", InitOptions{}, true, true, "Install the pf anchor? [y/N]", "warning:"},
		{"prompt declined", InitOptions{}, false, false, "warning: pf skipped", "--no-pf"},
		{"no pf", InitOptions{NoPF: true}, false, false, "warning: --no-pf", "Install the pf anchor?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			var out bytes.Buffer
			e.Out = &out
			e.Pins.SoftnetSHA256, e.Pins.SoftnetBinarySHA256 = sum(soft), sum([]byte("softnet binary"))
			e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
			r.versions["/opt/homebrew/bin/tart"] = TartVersion
			e.Fetcher = newReleases(t, map[string][]byte{"softnet.tar.gz": soft})
			e.Prompt = func(q string) (bool, error) { fmt.Fprint(&out, q); return tt.answer, nil }
			sts, err := Init(context.Background(), e, tt.o)
			if err != nil {
				t.Fatal(err)
			}
			if r.ran("sudo /bin/sh "+filepath.Join(e.StageDir, "root.sh")) != 1 {
				t.Fatalf("calls %v, want one sudo", r.calls)
			}
			if got := strings.Contains(r.script, PFAnchorPath); got != tt.wantPF || !strings.Contains(r.script, SoftnetPath) {
				t.Errorf("script has pf %v, want %v:\n%s", got, tt.wantPF, r.script)
			}
			if !strings.Contains(out.String(), tt.wantOut) || strings.Contains(out.String(), tt.noOut) {
				t.Errorf("output %q, want %q and not %q", out.String(), tt.wantOut, tt.noOut)
			}
			if _, err := os.Stat(filepath.Join(e.StageDir, "root.sh")); err == nil {
				t.Error("the sudo script was left behind")
			}
			// the fake sudo installs nothing: the final check still reports softnet.
			if sts[1].State != StateOK || sts[3].State != StateMissing {
				t.Errorf("final check %+v", sts)
			}
		})
	}
}

func TestInitStops(t *testing.T) {
	e, r := testEnv(t)
	r.macOS = "15.4"
	if _, err := Init(context.Background(), e, InitOptions{Yes: true}); err == nil || !strings.Contains(err.Error(), "platform: macOS 15.4") {
		t.Fatalf("err = %v", err)
	}
	// the kata driver on this Mac: its platform step stops init.
	e.Workspace.Driver = config.DriverKata
	if _, err := Init(context.Background(), e, InitOptions{}); err == nil || !strings.Contains(err.Error(), "platform: the kata driver needs linux/amd64, this is darwin/arm64") {
		t.Fatalf("kata: %v", err)
	}
	if _, err := os.Stat(e.StateDir()); err == nil || r.ran("sudo") != 0 {
		t.Error("a stopped init changed something")
	}
}

// TestInitApplyFails: a failed Apply stops init before sudo, and the statuses it returns are
// checked again, so they show what the earlier steps changed.
func TestInitApplyFails(t *testing.T) {
	e, r := testEnv(t)
	e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
	r.versions["/opt/homebrew/bin/tart"] = TartVersion
	e.Fetcher = newReleases(t, nil) // softnet.tar.gz is a 404
	sts, err := Init(context.Background(), e, InitOptions{NoPF: true})
	if err == nil || !strings.Contains(err.Error(), "softnet: GET") {
		t.Fatalf("err = %v", err)
	}
	if len(sts) != 7 || sts[1].State != StateOK || sts[3].State != StateMissing || r.ran("sudo") != 0 {
		t.Errorf("statuses %+v, sudo calls %d", sts, r.ran("sudo"))
	}
}

func TestRemovePF(t *testing.T) {
	e, r := testEnv(t)
	var out bytes.Buffer
	e.Out = &out
	if err := RemovePF(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	golden(t, "remove-pf.golden", e, out.String())
	if err := RemovePF(context.Background(), e, false); err != nil || r.script != RemovePFScript(e) {
		t.Fatalf("RemovePF: %v, script %q", err, r.script)
	}
}

func TestExtract(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
		wantErr string
	}{
		{"ok", []entry{{name: "a/", typ: tar.TypeDir}, {name: "a/b", body: "x"}, {name: "a/c", typ: tar.TypeSymlink, link: "b"}, {name: "d", typ: tar.TypeSymlink, link: "a/b"}}, ""},
		{"dot dot", []entry{{name: "../evil", body: "x"}}, "outside the archive"},
		{"nested dot dot", []entry{{name: "a/../../evil", body: "x"}}, "outside the archive"},
		{"absolute", []entry{{name: "/etc/evil", body: "x"}}, "outside the archive"},
		{"symlink escape", []entry{{name: "a/l", typ: tar.TypeSymlink, link: "../../etc"}}, "points outside"},
		{"absolute symlink", []entry{{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}}, "points outside"},
		{"write through a symlink", []entry{{name: "l", typ: tar.TypeSymlink, link: "."}, {name: "l/evil", body: "x"}}, `under the symlink "l"`},
		// a/l is the dir itself, so a/l/m -> .. would point outside it although a/.. is local.
		{"symlink chain", []entry{{name: "a/", typ: tar.TypeDir}, {name: "a/l", typ: tar.TypeSymlink, link: ".."}, {name: "a/l/m", typ: tar.TypeSymlink, link: ".."}, {name: "a/l/m/evil", body: "x"}}, `under the symlink "a/l"`},
		{"symlink target through a symlink", []entry{{name: "a/", typ: tar.TypeDir}, {name: "a/l", typ: tar.TypeSymlink, link: ".."}, {name: "x", typ: tar.TypeSymlink, link: "a/l/../.."}}, `symlink "x" points outside`},
		{"hard link", []entry{{name: "a", body: "x"}, {name: "h", typ: tar.TypeLink, link: "a"}}, "unsupported type"},
		{"oversize", []entry{{name: "big", body: "x", size: MaxArchive + 1}}, "larger than 536870912 bytes"},
		{"oversize in all", []entry{{name: "a", body: strings.Repeat("x", 1<<20)}, {name: "b", body: "x", size: MaxArchive - 1<<20 + 1}}, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			src := filepath.Join(tmp, "a.tar.gz")
			if err := os.WriteFile(src, tgz(t, tt.entries...), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(tmp, "x")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := Extract(src, dir)
			if tt.wantErr == "" {
				if b, rerr := os.ReadFile(filepath.Join(dir, "d")); err != nil || rerr != nil || string(b) != "x" {
					t.Fatalf("Extract: %v; d: %q %v", err, b, rerr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if _, err := os.Stat(filepath.Join(tmp, "evil")); err == nil {
				t.Error("an entry landed outside the dir")
			}
		})
	}
}

// TestExtractModes: modes keep only the 0755 bits, so an archive can't make a file setuid,
// setgid or group/other-writable; dirs get the owner's rwx too.
func TestExtractModes(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "a.tar.gz")
	archive := tgz(t,
		entry{name: "suid", body: "x", mode: 0o4755},
		entry{name: "sgid", body: "x", mode: 0o2777},
		entry{name: "open/", typ: tar.TypeDir, mode: 0o777},
		entry{name: "shut/", typ: tar.TypeDir, mode: 0o055},
	)
	if err := os.WriteFile(src, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Extract(src, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{"suid": 0o755, "sgid": 0o755, "open": fs.ModeDir | 0o755, "shut": fs.ModeDir | 0o755} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode() & (fs.ModeType | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | fs.ModePerm); got != want {
			t.Errorf("%s: mode %v, want %v", name, got, want)
		}
	}
}

// TestPFRules: the rules name each bridge (macOS pf has no interface groups) and parse with
// pfctl -n where it exists (macOS; no root needed).
func TestPFRules(t *testing.T) {
	rules := PFRules(3128)
	for _, w := range []string{" bridge100 ", " bridge163 }", "pass in quick on $bridges proto tcp from any to (self) port 3128\n"} {
		if !strings.Contains(rules, w) {
			t.Errorf("rules lack %q:\n%s", w, rules)
		}
	}
	if strings.Contains(rules, "bridge164") || strings.Contains(rules, "on bridge ") {
		t.Errorf("rules:\n%s", rules)
	}
	if _, err := os.Stat("/sbin/pfctl"); err != nil {
		t.Skip("no /sbin/pfctl")
	}
	p := filepath.Join(t.TempDir(), "anchor")
	if err := os.WriteFile(p, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/sbin/pfctl", "-n", "-v", "-a", PFAnchorName, "-f", p).CombinedOutput()
	if err != nil {
		t.Fatalf("pfctl -n: %v\n%s", err, out)
	}
	// 64 bridges, and 1 + 2 + 2 rules each once pfctl expands the port and proto lists.
	if n := strings.Count(string(out), "quick on bridge1"); n != 64*5 {
		t.Errorf("pfctl expanded %d rules, want %d:\n%s", n, 64*5, out)
	}
}

// TestPFConfRoundTrip runs pfConfAppend's and pfConfRestore's lines with sh on a scratch
// pf.conf: the restore gives back the original byte for byte, newline or not, and removes
// the backup; a later edit stays, and so does the backup.
func TestPFConfRoundTrip(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	tests := []struct {
		name, conf, edit string
		keepBackup       bool
	}{
		{"ends with a newline", "anchor \"com.apple/*\"\nload anchor \"com.apple\" from \"/etc/pf.anchors/com.apple\"\n", "", false},
		{"no final newline", "anchor \"com.apple/*\"\nload anchor \"com.apple\" from \"/etc/pf.anchors/com.apple\"", "", false},
		{"empty", "", "", false},
		{"edited after init", "anchor \"com.apple/*\"", "pass quick on lo0 all\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEnv(t)
			conf, backup := e.sys(PFConfPath), e.sys(PFConfBackup)
			if err := os.WriteFile(conf, []byte(tt.conf), 0o644); err != nil {
				t.Fatal(err)
			}
			run := func(lines []string) {
				t.Helper()
				script := Script(lines)
				if runtime.GOOS != "darwin" { // GNU sed: -i takes no separate suffix argument
					script = strings.ReplaceAll(script, "/usr/bin/sed -i ''", "/usr/bin/sed -i")
				}
				if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
					t.Fatalf("%v\n%s\n%s", err, out, script)
				}
			}
			run(pfConfAppend(e))
			run(pfConfAppend(e)) // a second init changes nothing
			nl := "\n"
			if tt.conf == "" || strings.HasSuffix(tt.conf, "\n") {
				nl = ""
			}
			if b, _ := os.ReadFile(conf); string(b) != tt.conf+nl+pfConfLine+"\n" {
				t.Fatalf("after init pf.conf is %q", b)
			}
			if b, err := os.ReadFile(backup); err != nil || string(b) != tt.conf {
				t.Fatalf("backup %q %v", b, err)
			}
			if tt.edit != "" {
				f, err := os.OpenFile(conf, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString(tt.edit)
				_ = f.Close()
			}
			run(pfConfRestore(e))
			want := tt.conf
			if tt.edit != "" {
				want += nl + tt.edit
			}
			if b, _ := os.ReadFile(conf); string(b) != want {
				t.Errorf("after --remove-pf pf.conf is %q, want %q", b, want)
			}
			if _, err := os.Stat(backup); (err == nil) != tt.keepBackup {
				t.Errorf("backup left: %v, want %v", err == nil, tt.keepBackup)
			}
		})
	}
}

// TestPFOptOut: --no-pf or a declined prompt records the opt-out, which check reports as
// skipped (AllOK counts it); a later init without --no-pf asks again and, once pf is
// installed, removes it.
func TestPFOptOut(t *testing.T) {
	tests := []struct {
		name   string
		o      InitOptions
		reason string
	}{
		{"no pf", InitOptions{NoPF: true}, "--no-pf"},
		{"prompt declined", InitOptions{}, "declined at host init's prompt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := testEnv(t)
			soft := tgz(t, entry{name: "softnet", body: "softnet binary"})
			e.Pins.SoftnetSHA256, e.Pins.SoftnetBinarySHA256 = sum(soft), sum([]byte("softnet binary"))
			e.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/tart", nil }
			r.versions["/opt/homebrew/bin/tart"] = TartVersion
			e.Fetcher = newReleases(t, map[string][]byte{"softnet.tar.gz": soft})
			asked := 0
			e.Prompt = func(string) (bool, error) { asked++; return false, nil }

			// a dry run records nothing.
			if _, err := Init(context.Background(), e, InitOptions{DryRun: true, NoPF: tt.o.NoPF}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(pfOptOutPath(e)); err == nil {
				t.Fatal("--dry-run recorded the opt-out")
			}

			sts, err := Init(context.Background(), e, tt.o)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(pfOptOutPath(e))
			if err != nil || !strings.Contains(string(b), "date=2026-10-22T09:30:00Z\nreason="+tt.reason+"\n") {
				t.Fatalf("opt-out %q %v", b, err)
			}
			wantState(t, sts[4], StateSkipped, "opted out on 2026-10-22T09:30:00Z ("+tt.reason+", ")
			// the fake sudo installed no softnet: only the pf step is ok-ish.
			if !AllOK([]Status{sts[4]}) || AllOK(sts) {
				t.Errorf("AllOK: %+v", sts)
			}

			// init without --no-pf asks again; accepted, pf is installed and the opt-out goes.
			asked = 0
			e.Prompt = func(string) (bool, error) { asked++; return true, nil }
			r.script = ""
			if _, err := Init(context.Background(), e, InitOptions{}); err != nil {
				t.Fatal(err)
			}
			if asked != 1 || !strings.Contains(r.script, PFAnchorPath) {
				t.Errorf("asked %d times; script:\n%s", asked, r.script)
			}
			if _, err := os.Stat(pfOptOutPath(e)); err == nil {
				t.Error("the opt-out is still there after pf was installed")
			}
		})
	}
}
