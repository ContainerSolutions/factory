package provision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/compliance-framework/factory/internal/agent/internal/exectest"
)

// TestWriteKataContext writes the embedded context: exactly the five files, the scripts 0755,
// their embedded bytes, and the sha256 KataContextSHA256 gives, which sha256sum's manifest
// reproduces.
func TestWriteKataContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	sum, err := WriteKataContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"Dockerfile", "base.sh", "factory-docker-storage", "factory-docker-storage.service", "factory-init"}
	if !slices.Equal(names, want) {
		t.Fatalf("files %v, want %v", names, want)
	}
	modes := map[string]fs.FileMode{"Dockerfile": 0o644, "base.sh": 0o755, "factory-docker-storage": 0o755, "factory-docker-storage.service": 0o644, "factory-init": 0o755}
	var manifest strings.Builder
	for _, n := range want {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != modes[n] {
			t.Errorf("%s: mode %v, want %v", n, fi.Mode().Perm(), modes[n])
		}
		got, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		emb, err := fs.ReadFile(embedded, kataContext[n])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, emb) {
			t.Errorf("%s differs from the embedded file", n)
		}
		s := sha256.Sum256(got)
		fmt.Fprintf(&manifest, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	m := sha256.Sum256([]byte(manifest.String()))
	if hex.EncodeToString(m[:]) != sum {
		t.Errorf("sum %s, want sha256sum's manifest's %s", sum, hex.EncodeToString(m[:]))
	}
	again, err := KataContextSHA256()
	if err != nil || again != sum {
		t.Errorf("KataContextSHA256() = %s, %v; want %s", again, err, sum)
	}

	// a second write over the first (the files' modes changed meanwhile) restores them.
	if err := os.Chmod(filepath.Join(dir, "factory-init"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := WriteKataContext(dir); err != nil || again != sum {
		t.Fatalf("rewrite: %s, %v", again, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "factory-init")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("rewrite left factory-init at %v (%v)", fi.Mode().Perm(), err)
	}
}

// TestKataContextEmbedsEveryFile: every file under kata/ is in the context, and the
// Dockerfile uses every other file of the context by its name.
func TestKataContextEmbedsEveryFile(t *testing.T) {
	ents, err := fs.ReadDir(embedded, "kata")
	if err != nil {
		t.Fatal(err)
	}
	var inDir []string
	for _, e := range ents {
		inDir = append(inDir, "kata/"+e.Name())
	}
	var inCtx []string
	for _, src := range kataContext {
		if strings.HasPrefix(src, "kata/") {
			inCtx = append(inCtx, src)
		}
	}
	slices.Sort(inCtx)
	if !slices.Equal(inDir, inCtx) {
		t.Errorf("kata/ has %v, the context %v", inDir, inCtx)
	}
	df, err := fs.ReadFile(embedded, "kata/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for n := range kataContext {
		if n == "Dockerfile" {
			continue
		}
		if !regexp.MustCompile(`(?m)^(COPY|RUN)\b.*[ =]` + regexp.QuoteMeta(n) + `[ ,]`).Match(df) {
			t.Errorf("the Dockerfile doesn't use %s", n)
		}
	}
}

// TestContextSHA256 is stable and changes when any file's content or name changes.
func TestContextSHA256(t *testing.T) {
	fsys := fstest.MapFS{}
	files := map[string]string{}
	for n, src := range kataContext {
		fsys[src] = &fstest.MapFile{Data: []byte("content of " + n + "\n")}
		files[n] = src
	}
	base, err := contextSHA256(fsys, files)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := contextSHA256(fsys, files); again != base {
		t.Fatalf("unstable: %s then %s", base, again)
	}
	for n, src := range kataContext {
		t.Run(n, func(t *testing.T) {
			changed := maps.Clone(fsys)
			changed[src] = &fstest.MapFile{Data: []byte("content of " + n + "!\n")}
			if s, err := contextSHA256(changed, files); err != nil || s == base {
				t.Errorf("a change to %s's content: %s, %v", n, s, err)
			}
			renamed := maps.Clone(files)
			delete(renamed, n)
			renamed[n+".x"] = src
			if s, err := contextSHA256(fsys, renamed); err != nil || s == base {
				t.Errorf("a rename of %s: %s, %v", n, s, err)
			}
		})
	}
	if _, err := contextSHA256(fsys, map[string]string{"missing": "kata/missing"}); err == nil {
		t.Error("a missing file: no error")
	}
}

// TestKataScripts: the context's shell scripts parse (sh -n; bash -n for base.sh).
func TestKataScripts(t *testing.T) {
	for _, c := range []struct{ shell, src string }{
		{"sh", "kata/factory-init"},
		{"sh", "kata/factory-docker-storage"},
		{"bash", "base.sh"},
	} {
		sh, err := exec.LookPath(c.shell)
		if err != nil {
			t.Logf("no %s: skipping %s", c.shell, c.src)
			continue
		}
		b, err := fs.ReadFile(embedded, c.src)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), filepath.Base(c.src))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sh, "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", c.shell, c.src, err, out)
		}
	}
}

// stubCommands are every command base.sh runs (sudo's arguments aside). The stub logs its name
// and arguments, and sha256sum -c's stdin (fetch's "<sha256>  <file>"), and answers what the
// script reads.
var stubCommands = []string{"cargo", "cat", "cloud-init", "cmp", "curl", "cut", "dpkg", "getent", "head", "id", "jq", "mkdir", "mktemp", "node", "npm", "od", "readlink", "rm", "sha256sum", "sudo", "tar", "tr"}

const stubScript = `#!/bin/sh
n=${0##*/}
printf '%s' "$n" >>"$STUB_LOG"
for a in "$@"; do printf ' %s' "$a" >>"$STUB_LOG"; done
echo >>"$STUB_LOG"
case $n in
dpkg) echo "$STUB_ARCH" ;;
mktemp) echo "$STUB_TMP" ;;
getent) echo "admin:x:1000:1000::$STUB_TMP/home:/bin/bash" ;;
id) echo admin ;;
npm) [ "$1" = prefix ] && echo /usr/local; [ "$1" = root ] && echo /usr/local/lib/node_modules ;;
node) echo /bin/sh ;;
sha256sum) [ "$1" = -c ] && /bin/cat >>"$STUB_LOG" ;;
esac
# a piped stdin is read, so the writer (echo ... | sudo tee) doesn't die of SIGPIPE.
if [ -p /dev/stdin ]; then /bin/cat >/dev/null; fi
exit 0
`

// runBase runs the embedded base.sh with every command it runs stubbed (PATH is the stubs'
// directory only, so nothing outside it can run) and returns the stubs' log, its stderr and
// its error. The run ends at the first check of a real file the stubs can't make (vhs and
// gifski in /usr/local/bin, or claude-code's native binary), after every download but codex's.
func runBase(t *testing.T, arch, mode string) (string, string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := os.Stat("/bin/cat"); err != nil {
		t.Skip("no /bin/cat for the stubs")
	}
	dir := t.TempDir()
	stubs, tmp := filepath.Join(dir, "stubs"), filepath.Join(dir, "tmp")
	for _, d := range []string{stubs, tmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range stubCommands {
		exectest.Write(t, filepath.Join(stubs, c), stubScript, 0o755)
	}
	// the Rust components' install.sh, which rust_component runs from $TMP.
	for _, c := range []string{"rustc", "cargo", "rust-std"} {
		for _, triple := range []string{"x86_64", "aarch64"} {
			d := filepath.Join(tmp, c+"-1.99.0-"+triple+"-unknown-linux-gnu")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			exectest.Write(t, filepath.Join(d, "install.sh"), stubScript, 0o755)
		}
	}
	script, err := fs.ReadFile(embedded, "base.sh")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "base.sh")
	if err := os.WriteFile(p, script, 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "log")
	cmd := exec.Command(bash, p)
	cmd.Env = []string{"PATH=" + stubs, "HOME=" + dir, "STUB_LOG=" + log, "STUB_ARCH=" + arch, "STUB_TMP=" + tmp}
	if mode != "" {
		cmd.Env = append(cmd.Env, "FACTORY_BASE_MODE="+mode)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b), stderr.String(), runErr
}

// TestBaseScriptRuns runs base.sh against stubs: each architecture downloads its own half of
// every pin pair, the image mode never starts, stops or applies anything, and an unknown mode
// or architecture stops it before anything runs.
func TestBaseScriptRuns(t *testing.T) {
	script, err := fs.ReadFile(embedded, "base.sh")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]map[string]string{} // name -> arch -> sha256
	for _, m := range regexp.MustCompile(`(?m)^([A-Z0-9_]+_SHA256)_(amd64|arm64)=([0-9a-f]{64})$`).FindAllStringSubmatch(string(script), -1) {
		if pins[m[1]] == nil {
			pins[m[1]] = map[string]string{}
		}
		pins[m[1]][m[2]] = m[3]
	}
	if len(pins) == 0 {
		t.Fatal("no per-architecture pins")
	}
	vmOnly := []string{"sudo systemctl disable --now apt-daily.timer apt-daily-upgrade.timer", "sudo systemctl stop unattended-upgrades.service", "sudo cloud-init status --wait", "sudo systemctl enable --now docker", "sudo sysctl --quiet --system"}
	for _, c := range []struct {
		arch, mode, other string
		assets            []string
	}{
		{"amd64", "image", "arm64", []string{"/kind-linux-amd64", "/bin/linux/amd64/kubectl", "/v1.6.2/linux-amd64\n", "/v0.2.0/linux-amd64\n", "-linux-x64.tar.gz", "go1.26.9.linux-amd64.tar.gz", "_Linux_x86_64.tar.gz", "-x86_64-unknown-linux-gnu.tar.xz"}},
		{"amd64", "vm", "arm64", []string{"/kind-linux-amd64", "-linux-x64.tar.gz"}},
		{"arm64", "", "amd64", []string{"/kind-linux-arm64", "/bin/linux/arm64/kubectl", "/v1.6.2/linux-arm64\n", "/v0.2.0/linux-arm64\n", "-linux-arm64.tar.gz", "go1.26.9.linux-arm64.tar.gz", "_Linux_arm64.tar.gz", "-aarch64-unknown-linux-gnu.tar.xz"}},
	} {
		t.Run(c.arch+"-"+c.mode, func(t *testing.T) {
			log, stderr, _ := runBase(t, c.arch, c.mode)
			if !strings.Contains(log, "gifski-1.34.0.crate") {
				t.Fatalf("the run ended before gifski's download:\n%s\nstderr:\n%s", log, stderr)
			}
			for name, sums := range pins {
				if strings.HasPrefix(name, "CODEX_") {
					continue // after the claude-code check, which the stubs can't pass
				}
				if !strings.Contains(log, sums[c.arch]+"  ") {
					t.Errorf("%s: %s_%s isn't checked", c.arch, name, c.arch)
				}
				if strings.Contains(log, sums[c.other]) {
					t.Errorf("%s: %s_%s is used", c.arch, name, c.other)
				}
			}
			for _, a := range c.assets {
				if !strings.Contains(log, a) {
					t.Errorf("%s: no download of %q", c.arch, a)
				}
			}
			for _, l := range vmOnly {
				if got := strings.Contains(log, l+"\n") || strings.Contains(log, l+" "); got != (c.mode != "image") {
					t.Errorf("mode %q: %q run: %v", c.mode, l, got)
				}
			}
			if c.mode == "image" {
				for l := range strings.Lines(log) {
					if strings.Contains(l, "--now") || strings.Contains(l, "cloud-init") || strings.Contains(l, "sysctl --") {
						t.Errorf("the image mode runs %s", strings.TrimSpace(l))
					}
				}
				if !strings.Contains(log, "sudo systemctl enable docker\n") {
					t.Error("the image mode doesn't enable docker")
				}
			}
		})
	}
	for _, c := range []struct{ arch, mode, err, log string }{
		{"amd64", "bogus", "unknown FACTORY_BASE_MODE bogus", ""},
		{"riscv64", "image", "unsupported architecture riscv64", "dpkg --print-architecture\n"},
	} {
		log, stderr, err := runBase(t, c.arch, c.mode)
		if err == nil || !strings.Contains(stderr, c.err) {
			t.Errorf("%s %s: %v, stderr %q; want %q", c.arch, c.mode, err, stderr, c.err)
		}
		if log != c.log {
			t.Errorf("%s %s ran %q, want %q", c.arch, c.mode, log, c.log)
		}
	}
}
