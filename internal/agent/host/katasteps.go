package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// The kata driver's install steps after the platform and the state dir
// (workspace-kata.md#steps-kata-driver): nerdctl-full, Kata and its config. Every file root
// installs from text is rendered by one function that both Check and Plan use, and goes into
// the sudo script (a quoted heredoc, or for the Kata config the sed that renders it); the
// archives come from the stage, and root checks the sha256 of its own copy before using it.

const (
	nerdctlName    = "nerdctl"
	kataName       = "kata"
	kataConfigName = "kataconfig"
)

const (
	// kataMarker holds the version of the Kata host init installed: an /opt/kata without it is
	// another install, which init never touches.
	kataMarker = katahelper.KataDir + "/.factory-agent"
	// kataShim is the runtime-rs shim containerd runs for io.containerd.kata.v2.
	kataShim = katahelper.KataDir + "/runtime-rs/bin/containerd-shim-kata-v2"
	// kataConfigSource is the release's runtime-rs Cloud Hypervisor config.
	kataConfigSource = katahelper.KataDir + "/share/defaults/kata-containers/runtime-rs/configuration-clh-runtime-rs.toml"
	// KataConfigPath is the Kata config: runtime-rs reads only its built-in paths, this the first.
	KataConfigPath = "/etc/kata-containers/runtime-rs/configuration.toml"
	// kataReconnectMS is the Kata agent's reconnect budget: phase 0's 8 GiB guest outlived the
	// default 3 s.
	kataReconnectMS = 60000
)

// rootTemp are the root lines that make the step's own root-owned temp dir $T (mktemp -d from
// template) and remove it if the script exits early; rootTempDone removes it at the step's
// end. umask 022 makes what root extracts and writes readable by the owner whatever the
// owner's umask (sudo keeps it). Each step makes its own, so --only works on any of them.
func rootTemp(template string) []string {
	return []string{
		"umask 022",
		"T=$(/usr/bin/mktemp -d " + quote(template) + ")",
		`trap '/usr/bin/rm -rf "$T"' EXIT`,
	}
}

const rootTempDone = `/usr/bin/rm -rf "$T"; trap - EXIT`

// rootTmp is the template of the temp dir of the steps that only write small files.
const rootTmp = "/tmp/factory-agent-root.XXXXXX"

// rootFail is a root line's failure: a message on stderr, then exit 1.
func rootFail(msg string) string {
	return "{ echo " + quote("factory-agent host init: "+msg) + " >&2; exit 1; }"
}

// rootSHA256 is the root line that checks the sha256 of root's copy of a staged file.
func rootSHA256(sum, file string) string {
	return "printf '%s  %s\\n' " + sum + " " + file + " | /usr/bin/sha256sum -c -"
}

// field is the i-th field of s's first line, "" when it has fewer.
func field(s string, i int) string {
	f := strings.Fields(firstLine(s))
	if i >= len(f) {
		return ""
	}
	return f[i]
}

// stage downloads url into a temp dir under the stage dir, checks its sha256, and moves it to
// the stage as name; the root lines take it from there.
func stage(ctx context.Context, e *Env, url, sum, name string) error {
	fmt.Fprintf(e.Out, "downloading %s\n", url)
	f, dir, err := fetchVerified(ctx, e.Fetcher, url, sum, e.StageDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return os.Rename(f, filepath.Join(e.StageDir, name))
}

// nerdctlStep installs the pinned nerdctl-full release (containerd, nerdctl, BuildKit, runc,
// the CNI plugins) at katahelper.NerdctlFullDir.
type nerdctlStep struct{ e *Env }

func (nerdctlStep) Name() string { return nerdctlName }

func (s nerdctlStep) bin(name string) string {
	return s.e.sys(katahelper.NerdctlFullDir + "/bin/" + name)
}

func (s nerdctlStep) staged() string { return filepath.Join(s.e.StageDir, "nerdctl-full.tar.gz") }

func (s nerdctlStep) Check(ctx context.Context) (Status, error) {
	e, n, c := s.e, s.bin("nerdctl"), s.bin("containerd")
	if _, err := os.Lstat(n); errors.Is(err, fs.ErrNotExist) {
		return Status{State: StateMissing, Detail: n}, nil
	} else if err != nil {
		return Status{}, err
	}
	nv, err := e.output(ctx, n, "--version")
	if err != nil {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s --version: %v", n, err)}, nil
	}
	cv, err := e.output(ctx, c, "--version")
	if err != nil {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s --version: %v", c, err)}, nil
	}
	// "nerdctl version 2.4.1"; "containerd github.com/containerd/containerd/v2 v2.4.1 <commit>"
	if got := field(nv, 2); got != e.Pins.NerdctlVersion {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s is nerdctl %q, want %s", n, got, e.Pins.NerdctlVersion)}, nil
	}
	if got := field(cv, 2); got != "v"+e.Pins.NerdctlContainerdVersion {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s is containerd %q, want v%s (nerdctl-full %s)", c, got, e.Pins.NerdctlContainerdVersion, e.Pins.NerdctlVersion)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("nerdctl-full %s, containerd %s (%s)", e.Pins.NerdctlVersion, e.Pins.NerdctlContainerdVersion, e.sys(katahelper.NerdctlFullDir))}, nil
}

func (s nerdctlStep) Plan(context.Context) (Plan, error) {
	e := s.e
	dir, parent := quote(e.sys(katahelper.NerdctlFullDir)), e.sys(katahelper.LibexecDir)
	a := `"$T/nerdctl-full.tar.gz"`
	root := []string{"/usr/bin/install -d -o root -g root -m 0755 " + quote(parent)}
	root = append(root, rootTemp(parent+"/.nerdctl-full.XXXXXX")...)
	root = append(root,
		"/usr/bin/install -o root -g root -m 0600 "+quote(s.staged())+" "+a,
		rootSHA256(e.Pins.NerdctlSHA256, a),
		// GNU tar refuses these too; the listing says so before anything is extracted.
		`if /usr/bin/tar -tzf `+a+` | /usr/bin/grep -qE '^/|(^|/)\.\.(/|$)'; then `+rootFail("nerdctl-full has an absolute or .. entry")+`; fi`,
		`/usr/bin/install -d -m 0755 "$T/x"`,
		`/usr/bin/tar -xzf `+a+` -C "$T/x" --no-same-owner --no-same-permissions`,
		`if [ -e `+dir+` ] || [ -L `+dir+` ]; then /usr/bin/mv `+dir+` "$T/old"; fi`,
		`/usr/bin/mv "$T/x" `+dir,
		rootTempDone,
	)
	return Plan{
		Do: []string{
			"download " + e.Pins.NerdctlURL + " into the stage and check its sha256",
			fmt.Sprintf("install nerdctl-full %s at %s (sudo: root checks its copy's sha256 again, then extracts it)", e.Pins.NerdctlVersion, e.sys(katahelper.NerdctlFullDir)),
		},
		Root: root,
	}, nil
}

func (s nerdctlStep) Apply(ctx context.Context) error {
	return stage(ctx, s.e, s.e.Pins.NerdctlURL, s.e.Pins.NerdctlSHA256, filepath.Base(s.staged()))
}

// kataStep installs the pinned Kata static release at /opt/kata, with the marker that says
// host init put it there. Another /opt/kata is never replaced.
type kataStep struct{ e *Env }

func (kataStep) Name() string { return kataName }

func (s kataStep) staged() string { return filepath.Join(s.e.StageDir, "kata-static.tar.zst") }

// foreign is the wrong status of an /opt/kata host init didn't make; nil when there is none,
// or it's host init's.
func (s kataStep) foreign() (*Status, error) {
	d := s.e.sys(katahelper.KataDir)
	fi, err := os.Lstat(d)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return &Status{State: StateWrong, Detail: d + " is not a directory: another Kata install? Move it away; host init never replaces it"}, nil
	}
	if _, err := os.Lstat(s.e.sys(kataMarker)); errors.Is(err, fs.ErrNotExist) {
		return &Status{State: StateWrong, Detail: d + " has no .factory-agent marker: it's another Kata install. Move it away; host init never replaces it"}, nil
	} else if err != nil {
		return nil, err
	}
	return nil, nil
}

func (s kataStep) Check(context.Context) (Status, error) {
	e, d := s.e, s.e.sys(katahelper.KataDir)
	if st, err := s.foreign(); st != nil || err != nil {
		return deref(st), err
	}
	b, err := os.ReadFile(e.sys(kataMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return Status{State: StateMissing, Detail: d}, nil
	} else if err != nil {
		return Status{}, err
	}
	if v := strings.TrimSpace(string(b)); v != e.Pins.KataVersion {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s is Kata %q, want %s", d, v, e.Pins.KataVersion)}, nil
	}
	shim := e.sys(kataShim)
	if fi, err := os.Stat(shim); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return Status{State: StateWrong, Detail: shim + " is not an executable file"}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("Kata %s (%s)", e.Pins.KataVersion, d)}, nil
}

func deref(st *Status) Status {
	if st == nil {
		return Status{}
	}
	return *st
}

// kataEntries are the archive entries the Kata release may have (tar -t's names): everything
// under opt/kata.
var kataEntries = []string{`^\.$`, `^\./$`, `^(\./)?opt/?$`, `^(\./)?opt/kata(/.*)?$`}

func (s kataStep) Plan(context.Context) (Plan, error) {
	e := s.e
	d := quote(e.sys(katahelper.KataDir))
	a := `"$T/kata-static.tar.zst"`
	var pats []string
	for _, p := range kataEntries {
		pats = append(pats, "-e '"+p+"'")
	}
	root := []string{
		// the check's guard again, now as root: an /opt/kata without the marker stays.
		`if [ -L ` + d + ` ] || { [ -e ` + d + ` ] && [ ! -f ` + quote(e.sys(kataMarker)) + ` ]; }; then ` +
			rootFail(e.sys(katahelper.KataDir)+" is another Kata install: move it away") + `; fi`,
	}
	root = append(root, rootTemp(e.sys("/opt")+"/.factory-agent-kata.XXXXXX")...)
	root = append(root,
		"/usr/bin/install -o root -g root -m 0600 "+quote(s.staged())+" "+a,
		rootSHA256(e.Pins.KataSHA256, a),
		`/usr/bin/tar --zstd -tf `+a+` > "$T/entries"`,
		`if /usr/bin/grep -qvE `+strings.Join(pats, " ")+` "$T/entries" || /usr/bin/grep -qE '(^|/)\.\.(/|$)' "$T/entries"; then `+
			rootFail("the Kata release has entries outside opt/kata")+`; fi`,
		`/usr/bin/install -d -m 0755 "$T/x"`,
		`/usr/bin/tar --zstd -xf `+a+` -C "$T/x" --no-same-owner --no-same-permissions`,
		`[ "$(cd "$T/x" && /usr/bin/find . -mindepth 1 -maxdepth 2 | /usr/bin/sort | /usr/bin/tr '\n' ' ')" = './opt ./opt/kata ' ] || `+
			rootFail("the Kata release extracted outside opt/kata"),
		`/usr/bin/rm -f "$T/x/opt/kata/.factory-agent"`,
		`printf '%s\n' `+quote(e.Pins.KataVersion)+` > "$T/x/opt/kata/.factory-agent"`,
		`if [ -e `+d+` ]; then /usr/bin/mv `+d+` "$T/old"; fi`,
		`/usr/bin/mv "$T/x/opt/kata" `+d,
		rootTempDone,
	)
	return Plan{
		Do: []string{
			"download " + e.Pins.KataURL + " (about 950 MiB) into the stage and check its sha256",
			fmt.Sprintf("install Kata %s at %s (sudo: root checks its copy's sha256 again, refuses any entry outside opt/kata, then extracts it)", e.Pins.KataVersion, e.sys(katahelper.KataDir)),
		},
		Root: root,
	}, nil
}

func (s kataStep) Apply(ctx context.Context) error {
	if st, err := s.foreign(); err != nil {
		return err
	} else if st != nil {
		return errors.New(st.Detail)
	}
	return stage(ctx, s.e, s.e.Pins.KataURL, s.e.Pins.KataSHA256, filepath.Base(s.staged()))
}

// kataConfigHeader starts the rendered Kata config; a config without it is another install's.
const kataConfigHeader = "# factory-agent host init: "

// kataReconnectRE is the line the rendering changes, as the root lines' sed -E matches it.
var kataReconnectRE = regexp.MustCompile(`^(reconnect_timeout_ms)[[:space:]]*=.*`)

// kataConfigFirstLine is the rendered config's first line, naming its source.
func kataConfigFirstLine(version string) string {
	return fmt.Sprintf("%sconfiguration-clh-runtime-rs.toml of the Kata %s static release, reconnect_timeout_ms = %d", kataConfigHeader, version, kataReconnectMS)
}

// RenderKataConfig is the Kata config: the release's runtime-rs Cloud Hypervisor config src
// with reconnect_timeout_ms raised to 60000, after a first comment line. It is what the root
// lines' sed renders, byte for byte; src without a reconnect_timeout_ms line is an error.
func RenderKataConfig(src []byte, version string) (string, error) {
	var b strings.Builder
	b.WriteString(kataConfigFirstLine(version) + "\n")
	n := 0
	for l := range strings.Lines(string(src)) {
		body, nl := strings.CutSuffix(l, "\n")
		if kataReconnectRE.MatchString(body) {
			body = kataReconnectRE.ReplaceAllString(body, "${1} = "+strconv.Itoa(kataReconnectMS))
			n++
		}
		b.WriteString(body)
		if nl {
			b.WriteString("\n")
		}
	}
	if n == 0 {
		return "", errors.New("it has no reconnect_timeout_ms line")
	}
	return b.String(), nil
}

// kataConfigStep renders the Kata config from the installed release (so it needs the kata
// step's files).
type kataConfigStep struct{ e *Env }

func (kataConfigStep) Name() string { return kataConfigName }

// foreign is the wrong status of a Kata config host init didn't write; nil when there is
// none, or it's host init's.
func (s kataConfigStep) foreign() (*Status, error) {
	p := s.e.sys(KataConfigPath)
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !strings.HasPrefix(string(b), kataConfigHeader):
		return &Status{State: StateWrong, Detail: p + " is another Kata install's config: move it away; host init never replaces it"}, nil
	}
	return nil, nil
}

func (s kataConfigStep) Check(context.Context) (Status, error) {
	e, p, src := s.e, s.e.sys(KataConfigPath), s.e.sys(kataConfigSource)
	if st, err := s.foreign(); st != nil || err != nil {
		return deref(st), err
	}
	got, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{State: StateMissing, Detail: p}, nil
	} else if err != nil {
		return Status{}, err
	}
	b, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s: no %s to render it from (the kata step installs it)", p, src)}, nil
	} else if err != nil {
		return Status{}, err
	}
	want, err := RenderKataConfig(b, e.Pins.KataVersion)
	if err != nil {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s: %v", src, err)}, nil
	}
	if string(got) != want {
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s differs from the rendering of %s", p, src)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("%s: Kata %s's Cloud Hypervisor config, reconnect_timeout_ms = %d", p, e.Pins.KataVersion, kataReconnectMS)}, nil
}

func (s kataConfigStep) Plan(context.Context) (Plan, error) {
	e := s.e
	p, src := quote(e.sys(KataConfigPath)), quote(e.sys(kataConfigSource))
	c := `"$T/configuration.toml"`
	root := []string{
		`if [ -e ` + p + ` ] && ! /usr/bin/head -n 1 ` + p + ` | /usr/bin/grep -qF ` + quote(kataConfigHeader) + `; then ` +
			rootFail(e.sys(KataConfigPath)+" is another Kata install's config: move it away") + `; fi`,
		`[ -f ` + src + ` ] || ` + rootFail("no "+e.sys(kataConfigSource)+": the kata step installs it"),
	}
	root = append(root, rootTemp(rootTmp)...)
	root = append(root,
		`{ printf '%s\n' `+quote(kataConfigFirstLine(e.Pins.KataVersion))+`; /usr/bin/sed -E 's/^(reconnect_timeout_ms)[[:space:]]*=.*/\1 = `+strconv.Itoa(kataReconnectMS)+`/' `+src+`; } > `+c,
		`/usr/bin/grep -qx 'reconnect_timeout_ms = `+strconv.Itoa(kataReconnectMS)+`' `+c+` || `+rootFail(e.sys(kataConfigSource)+" has no reconnect_timeout_ms line"),
		"/usr/bin/install -d -o root -g root -m 0755 "+quote(e.sys("/etc/kata-containers"))+" "+quote(e.sys(filepath.Dir(KataConfigPath))),
		"/usr/bin/install -o root -g root -m 0644 "+c+" "+p,
		rootTempDone,
	)
	return Plan{
		Do:   []string{fmt.Sprintf("render %s from %s with reconnect_timeout_ms = %d (sudo)", e.sys(KataConfigPath), e.sys(kataConfigSource), kataReconnectMS)},
		Root: root,
	}, nil
}

func (s kataConfigStep) Apply(context.Context) error {
	if st, err := s.foreign(); err != nil {
		return err
	} else if st != nil {
		return errors.New(st.Detail)
	}
	return nil
}
