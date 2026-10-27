package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// The kata driver's privileges step (workspace-kata.md#the-privileges-step): the root helper and
// the sudoers rule that lets the owner run it.

const privilegesName = "privileges"

// SudoersPath is the rule that lets the owner run the helper with sudo -n.
const SudoersPath = "/etc/sudoers.d/factory-agent"

// ownerRE is a user name the sudoers rule may name.
var ownerRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// SudoersRule lets owner run the helper as root without a password, with any arguments: the
// helper validates them.
func SudoersRule(owner string) string {
	return "# " + SudoersPath + " (factory-agent host init): the kata driver's root helper.\n" +
		owner + " ALL=(root) NOPASSWD: " + katahelper.HelperPath + "\n"
}

// privilegesNotice is what Init says before asking about the privileges step.
func privilegesNotice() string {
	return fmt.Sprintf(`The privileges step installs the kata helper: a root-owned copy of this factory-agent at
%s, and a sudoers rule (%s) that lets you run it as root without
a password. supervise --driver kata does every root action through it (creating, starting,
stopping and removing workspaces with the factory's nerdctl); its verbs take only workspace
names, sizes and slot numbers. Anything that runs as you can run it too.
`, katahelper.HelperPath, SudoersPath)
}

// helperStep is a step whose Apply runs the helper with sudo -n: without the privileges step,
// which installs the helper and its sudoers rule, it can't. Init skips it when the owner
// declines that step.
type helperStep interface {
	Step
	// throughHelper is what the step does through the helper, for Init's warning ("the image
	// is built").
	throughHelper() string
}

// usesHelper reports whether s runs the helper.
func usesHelper(s Step) bool {
	_, ok := s.(helperStep)
	return ok
}

// privilegesStep installs the helper, root-owned, and the sudoers rule for the owner.
type privilegesStep struct{ e *Env }

func (privilegesStep) Name() string { return privilegesName }

func (s privilegesStep) staged() string { return filepath.Join(s.e.StageDir, katahelper.Name) }

// owner is the user running host init, whom the sudoers rule names: never SUDO_USER, and
// never root (supervise runs as its owner).
func (s privilegesStep) owner() (string, error) {
	u, err := s.e.Username()
	if err != nil {
		return "", fmt.Errorf("the user running host init: %w", err)
	}
	if !ownerRE.MatchString(u) {
		return "", fmt.Errorf("the user name %q can't go in the sudoers rule (want %s)", u, ownerRE)
	}
	if u == "root" {
		return "", errors.New("host init runs as root: run it as the user who runs supervise (it asks for sudo itself)")
	}
	return u, nil
}

// rootFile is the status of a file root must own with exactly mode (missing or wrong); nil
// when it does.
func rootFile(e *Env, p string, mode fs.FileMode) (fi fs.FileInfo, st *Status, err error) {
	fi, err = os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &Status{State: StateMissing, Detail: p}, nil
	case err != nil:
		return nil, nil, err
	case !fi.Mode().IsRegular():
		return nil, &Status{State: StateWrong, Detail: p + " is not a regular file"}, nil
	}
	if uid, gid, ok := e.Owner(fi); !ok || uid != 0 || gid != 0 {
		return nil, &Status{State: StateWrong, Detail: fmt.Sprintf("%s is owned by %d:%d, want root:root", p, uid, gid)}, nil
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != mode {
		return nil, &Status{State: StateWrong, Detail: fmt.Sprintf("%s has mode %v, want %v", p, m, mode)}, nil
	}
	return fi, nil, nil
}

func (s privilegesStep) Check(ctx context.Context) (Status, error) {
	e := s.e
	owner, err := s.owner()
	if err != nil {
		return Status{State: StateWrong, Detail: err.Error()}, nil
	}
	h := e.sys(katahelper.HelperPath)
	if _, st, err := rootFile(e, h, 0o755); st != nil || err != nil {
		return deref(st), err
	}
	want := strconv.Itoa(katahelper.Protocol)
	got, err := e.output(ctx, h, "protocol")
	if err != nil {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s protocol: %v", h, err)}, nil
	}
	if got != want {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s speaks protocol %q, this factory-agent %s: host init installs this one", h, got, want)}, nil
	}
	p, rule := e.sys(SudoersPath), SudoersRule(owner)
	fi, st, err := rootFile(e, p, 0o440)
	if st != nil || err != nil {
		return deref(st), err
	}
	b, rerr := os.ReadFile(p)
	if ok, err := sudoersMatch(b, rerr, fi.Size(), rule); err != nil {
		return Status{}, err
	} else if !ok {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s differs from the rendered rule for %s", p, owner)}, nil
	}
	// host check never runs anything as root: sudo -l only lists. With sudo's default
	// listpw=any (assumed here), a user with a NOPASSWD rule may list without a password; -k
	// ignores a cached sudo login (host init's own), so without the rule -n makes it fail.
	if _, err := e.output(ctx, "sudo", "-n", "-k", "-l", katahelper.HelperPath); err != nil {
		return Status{State: StateWrong, Detail: fmt.Sprintf("the sudoers rule doesn't let you run the helper (sudo -n -l %s: %v)", katahelper.HelperPath, err)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("%s (protocol %s), root:root 0755; %s lets %s run it with sudo -n", h, want, p, owner)}, nil
}

// sudoersMatch reports whether the sudoers file holds rule: its text b when it was readable
// (readErr nil); when the owner can't read it (root:root 0440, as installed), its size, and the
// check's sudo -n -l shows the rule works. Another read error is an error.
func sudoersMatch(b []byte, readErr error, size int64, rule string) (bool, error) {
	switch {
	case readErr == nil:
		return string(b) == rule, nil
	case errors.Is(readErr, fs.ErrPermission):
		return size == int64(len(rule)), nil
	}
	return false, readErr
}

func (s privilegesStep) Plan(context.Context) (Plan, error) {
	e := s.e
	if e.Executable == "" {
		return Plan{}, errors.New("can't find the running factory-agent to install as the helper")
	}
	sum, err := fileSHA256(e.Executable)
	if err != nil {
		return Plan{}, fmt.Errorf("the running factory-agent: %w", err)
	}
	owner, err := s.owner()
	if err != nil {
		return Plan{}, err
	}
	sudoers, err := heredoc(`"$T/sudoers"`, SudoersRule(owner))
	if err != nil {
		return Plan{}, err
	}
	lib, h := e.sys(katahelper.LibexecDir), `"$T/`+katahelper.Name+`"`
	root := []string{"/usr/bin/install -d -o root -g root -m 0755 " + quote(lib)}
	root = append(root, rootTemp(lib+"/.kata-helper.XXXXXX")...)
	root = append(root,
		"/usr/bin/install -o root -g root -m 0700 "+quote(s.staged())+" "+h,
		rootSHA256(sum, h),
		"/usr/bin/install -o root -g root -m 0755 "+h+" "+quote(e.sys(katahelper.HelperPath)),
		sudoers,
		`/usr/sbin/visudo -cf "$T/sudoers"`,
		`/usr/bin/install -o root -g root -m 0440 "$T/sudoers" `+quote(e.sys(SudoersPath)),
		rootTempDone,
	)
	return Plan{
		Do: []string{
			fmt.Sprintf("copy %s (sha256 %s) into the stage", e.Executable, sum),
			fmt.Sprintf("install it root:root 0755 at %s and the sudoers rule %s, which lets %s run it with sudo -n (sudo: root checks its copy's sha256 again, visudo -cf the rule)",
				e.sys(katahelper.HelperPath), e.sys(SudoersPath), owner),
		},
		Root: root,
	}, nil
}

// Apply copies the running factory-agent into the stage; the root lines check its sha256
// against the plan's.
func (s privilegesStep) Apply(context.Context) error {
	if _, err := s.owner(); err != nil {
		return err
	}
	if s.e.Executable == "" {
		return errors.New("can't find the running factory-agent to install as the helper")
	}
	src, err := os.Open(s.e.Executable)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := os.Remove(s.staged()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dst, err := os.OpenFile(s.staged(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}
