package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

const (
	platformName = "platform"
	stateDirName = "statedir"
	// minMacOS is the oldest macOS Softnet runs on.
	minMacOS = 26
	// minFreeGiB is the free disk the state dir's volume needs (the base and warm VMs).
	minFreeGiB = 80
	// HeadroomGiB is the physical memory left to the host on top of its workspaces: macOS's
	// on a Mac, and on Linux the share supervise's concurrency cap keeps back.
	HeadroomGiB = 4
)

// platformStep checks the Mac: Apple Silicon, macOS 26 or later, free disk and memory. It
// has nothing to apply: a failed check stops Init.
type platformStep struct{ e *Env }

func (platformStep) Name() string                       { return platformName }
func (platformStep) Plan(context.Context) (Plan, error) { return Plan{}, nil }
func (platformStep) Apply(context.Context) error {
	return errors.New("the platform can't be fixed by host init")
}

func (s platformStep) Check(ctx context.Context) (Status, error) {
	e := s.e
	if e.GOOS != "darwin" || e.GOARCH != "arm64" {
		return Status{State: StateWrong, Detail: fmt.Sprintf("the tart driver needs darwin/arm64, this is %s/%s", e.GOOS, e.GOARCH)}, nil
	}
	ver, err := e.output(ctx, "sw_vers", "-productVersion")
	if err != nil {
		return Status{}, err
	}
	mem, err := e.output(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return Status{}, err
	}
	memBytes, err := strconv.ParseUint(mem, 10, 64)
	if err != nil {
		return Status{}, fmt.Errorf("sysctl hw.memsize: %q is not a number", mem)
	}
	free, err := freeBytes(ctx, e, e.StateDir())
	if err != nil {
		return Status{}, err
	}
	var probs []string
	if major, _, _ := strings.Cut(ver, "."); atoi(major) < minMacOS {
		probs = append(probs, fmt.Sprintf("macOS %s: Softnet needs %d.0 or later", ver, minMacOS))
	}
	if want := uint64(e.Workspace.Size.MemoryGiB()+HeadroomGiB) << 30; memBytes < want {
		probs = append(probs, fmt.Sprintf("%d GiB memory: a %s workspace needs %d GiB", memBytes>>30, e.Workspace.Size.Memory, want>>30))
	}
	if free < minFreeGiB<<30 {
		probs = append(probs, fmt.Sprintf("%d GiB free on the state dir's volume, want %d GiB", free>>30, minFreeGiB))
	}
	if len(probs) > 0 {
		return Status{State: StateWrong, Detail: strings.Join(probs, "; ")}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("macOS %s, %d GiB memory, %d GiB free", ver, memBytes>>30, free>>30)}, nil
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// freeBytes is the space available on p's volume (its nearest existing parent), from df.
func freeBytes(ctx context.Context, e *Env, p string) (uint64, error) {
	for {
		if _, err := os.Stat(p); err == nil || filepath.Dir(p) == p {
			break
		}
		p = filepath.Dir(p)
	}
	out, err := e.output(ctx, "df", "-Pk", p)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(out, "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(lines) < 2 || len(f) < 4 {
		return 0, fmt.Errorf("df %s: unexpected output %q", p, out)
	}
	kb, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("df %s: available %q is not a number", p, f[3])
	}
	return kb << 10, nil
}

// stateDirStep makes the state dir: the owner's, mode 0700.
type stateDirStep struct{ e *Env }

func (stateDirStep) Name() string { return stateDirName }

func (s stateDirStep) Check(context.Context) (Status, error) {
	d := s.e.StateDir()
	fi, err := os.Lstat(d)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Status{State: StateMissing, Detail: d}, nil
	case err != nil:
		return Status{}, err
	case !fi.IsDir():
		return Status{State: StateWrong, Detail: d + " is not a directory"}, nil
	}
	if uid, _, ok := s.e.Owner(fi); ok && uid != s.e.UID() {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s is owned by uid %d, not you (%d)", d, uid, s.e.UID())}, nil
	}
	if fi.Mode().Perm() != 0o700 {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s has mode %04o, want 0700", d, fi.Mode().Perm())}, nil
	}
	return Status{State: StateOK, Detail: d}, nil
}

func (s stateDirStep) Plan(context.Context) (Plan, error) {
	return Plan{Do: []string{"create " + s.e.StateDir() + " with mode 0700"}}, nil
}

func (s stateDirStep) Apply(context.Context) error {
	d := s.e.StateDir()
	if fi, err := os.Lstat(d); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory: move it away", d)
		}
		if uid, _, ok := s.e.Owner(fi); ok && uid != s.e.UID() {
			return fmt.Errorf("%s is owned by uid %d: fix it by hand", d, uid)
		}
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return os.Chmod(d, 0o700)
}

// tartStep installs the pinned Tart into the state dir, unless a tart on PATH is at the pin
// or newer.
type tartStep struct{ e *Env }

func (tartStep) Name() string { return "tart" }

func (s tartStep) bin() string {
	return filepath.Join(s.e.StateDir(), "tart", "tart.app", "Contents", "MacOS", "tart")
}

func (s tartStep) version(ctx context.Context, bin string) (string, error) {
	v, err := s.e.output(ctx, bin, "--version")
	return firstLine(v), err
}

func (s tartStep) Check(ctx context.Context) (Status, error) {
	var found *Status
	if _, err := os.Stat(s.bin()); err == nil {
		v, err := s.version(ctx, s.bin())
		switch {
		case err != nil:
			found = &Status{State: StateWrong, Detail: fmt.Sprintf("%s: %v", s.bin(), err)}
		case v == s.e.Pins.TartVersion:
			return Status{State: StateOK, Detail: fmt.Sprintf("tart %s (%s)", v, s.bin())}, nil
		default:
			found = &Status{State: StateOutdated, Detail: fmt.Sprintf("%s is %s, want %s", s.bin(), v, s.e.Pins.TartVersion)}
		}
	}
	if p, err := s.e.LookPath("tart"); err == nil {
		v, err := s.version(ctx, p)
		if err == nil && semver.Compare("v"+v, "v"+s.e.Pins.TartVersion) >= 0 {
			return Status{State: StateOK, Detail: fmt.Sprintf("tart %s on PATH (%s)", v, p)}, nil
		}
		switch {
		case found != nil:
		case err != nil:
			found = &Status{State: StateWrong, Detail: fmt.Sprintf("%s: %v", p, err)}
		default:
			found = &Status{State: StateOutdated, Detail: fmt.Sprintf("%s is %q, want %s or newer", p, v, s.e.Pins.TartVersion)}
		}
	}
	if found != nil {
		return *found, nil
	}
	return Status{State: StateMissing, Detail: "no tart in the state dir or on PATH"}, nil
}

func (s tartStep) Plan(context.Context) (Plan, error) {
	return Plan{Do: []string{
		"download " + s.e.Pins.TartURL + " and check its sha256",
		"extract tart.app into " + filepath.Join(s.e.StateDir(), "tart"),
		"verify its signature with codesign: an Apple-issued certificate for team " + s.e.Pins.TartTeamID,
	}}, nil
}

func (s tartStep) Apply(ctx context.Context) error {
	tgz, dir, err := fetchVerified(ctx, s.e.Fetcher, s.e.Pins.TartURL, s.e.Pins.TartSHA256, s.e.StateDir())
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	x := filepath.Join(dir, "x")
	if err := os.Mkdir(x, 0o700); err != nil {
		return err
	}
	if err := Extract(tgz, x); err != nil {
		return err
	}
	app := filepath.Join(x, "tart.app")
	if fi, err := os.Lstat(app); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s has no tart.app", s.e.Pins.TartURL)
	}
	if _, err := s.e.output(ctx, "codesign", "--verify", "--deep", "--strict", "-R", tartRequirement(s.e.Pins.TartTeamID), app); err != nil {
		return fmt.Errorf("tart.app's signature: %w", err)
	}
	info, err := s.e.output(ctx, "codesign", "-dv", app)
	if err != nil {
		return err
	}
	if !hasLine(info, "TeamIdentifier="+s.e.Pins.TartTeamID) {
		return fmt.Errorf("tart.app isn't signed by team %s: %s", s.e.Pins.TartTeamID, teamLine(info))
	}
	dst := filepath.Join(s.e.StateDir(), "tart")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(dst, "tart.app")); err != nil {
		return err
	}
	return os.Rename(app, filepath.Join(dst, "tart.app"))
}

// tartRequirement is the code requirement tart.app must satisfy (codesign -R; a leading "="
// makes it requirement text, see csreq(1)): a certificate chain anchored at Apple, whose leaf
// is the team's. codesign -dv's TeamIdentifier alone comes from whatever certificate the
// bundle carries, so a self-made one with that OU would pass.
func tartRequirement(team string) string {
	return `=anchor apple generic and certificate leaf[subject.OU] = "` + team + `"`
}

func hasLine(s, line string) bool {
	for l := range strings.Lines(s) {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func teamLine(info string) string {
	for l := range strings.Lines(info) {
		if strings.HasPrefix(l, "TeamIdentifier=") {
			return strings.TrimSpace(l)
		}
	}
	return "no TeamIdentifier"
}

// SoftnetPath is where host init installs Softnet; the tart driver runs Tart with its
// directory on PATH.
const SoftnetPath = "/usr/local/libexec/factory-agent/softnet"

// softnetStep installs the pinned Softnet, root-owned and SUID (Tart runs it to set up the
// VMs' networking).
type softnetStep struct{ e *Env }

func (softnetStep) Name() string { return "softnet" }

func (s softnetStep) Check(context.Context) (Status, error) {
	p := s.e.sys(SoftnetPath)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Status{State: StateMissing, Detail: p}, nil
	case err != nil:
		return Status{}, err
	case !fi.Mode().IsRegular():
		return Status{State: StateWrong, Detail: p + " is not a regular file"}, nil
	}
	if uid, gid, ok := s.e.Owner(fi); !ok || uid != 0 || gid != 0 {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s is owned by %d:%d, want root:wheel", p, uid, gid)}, nil
	}
	if m := fi.Mode() & (fs.ModeSetuid | fs.ModeSetgid | fs.ModePerm); m != fs.ModeSetuid|0o755 {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s has mode %v, want SUID 4755", p, m)}, nil
	}
	sum, err := fileSHA256(p)
	if err != nil {
		return Status{}, err
	}
	if sum != s.e.Pins.SoftnetBinarySHA256 {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s isn't the pinned %s (sha256 %s)", p, SoftnetVersion, sum)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("softnet %s, SUID root (%s)", SoftnetVersion, p)}, nil
}

func (s softnetStep) staged() string { return filepath.Join(s.e.StageDir, "softnet") }

func (s softnetStep) Plan(context.Context) (Plan, error) {
	dst := s.e.sys(SoftnetPath)
	tmp := quote(dst + ".new")
	return Plan{
		Do: []string{"download " + s.e.Pins.SoftnetURL + ", check its sha256 and the binary's", "install it SUID root at " + dst + " (sudo)"},
		// root copies the binary, checks the copy's sha256 again, and only then sets SUID.
		Root: []string{
			"/usr/bin/install -d -o root -g wheel -m 0755 " + quote(filepath.Dir(dst)),
			"/usr/bin/install -o root -g wheel -m 0700 " + quote(s.staged()) + " " + tmp,
			"printf '%s  %s\\n' " + s.e.Pins.SoftnetBinarySHA256 + " " + tmp + " | /usr/bin/shasum -a 256 -c -",
			"/bin/chmod 4755 " + tmp,
			"/bin/mv -f " + tmp + " " + quote(dst),
		},
	}, nil
}

func (s softnetStep) Apply(ctx context.Context) error {
	tgz, dir, err := fetchVerified(ctx, s.e.Fetcher, s.e.Pins.SoftnetURL, s.e.Pins.SoftnetSHA256, s.e.StageDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	x := filepath.Join(dir, "x")
	if err := os.Mkdir(x, 0o700); err != nil {
		return err
	}
	if err := Extract(tgz, x); err != nil {
		return err
	}
	bin := filepath.Join(x, "softnet")
	if fi, err := os.Lstat(bin); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s has no softnet binary", s.e.Pins.SoftnetURL)
	}
	if err := checkSHA256(bin, s.e.Pins.SoftnetBinarySHA256); err != nil {
		return err
	}
	return os.Rename(bin, s.staged())
}
