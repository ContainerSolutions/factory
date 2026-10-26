package katahelper

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const tag = "factory-agent-base:0.1.0"

// readContext reads the build context nerdctl build got: each file's mode and content.
func readContext(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("the build context: %v", err)
		return m
	}
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		m[e.Name()] = fmt.Sprintf("%v %s", fi.Mode(), b)
	}
	return m
}

// file is one entry of a context tar.
type file struct {
	name string
	mode int64
	body string
	typ  byte
}

func contextTar(t *testing.T, files ...file) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		typ := f.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hd := &tar.Header{Name: f.name, Mode: f.mode, Typeflag: typ}
		if typ == tar.TypeReg {
			hd.Size = int64(len(f.body))
		} else {
			hd.Linkname = f.body
		}
		if err := tw.WriteHeader(hd); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

var goodContext = []file{{name: "Dockerfile", mode: 0o600, body: "FROM x\n"}, {name: "base.sh", mode: 0o4775, body: "#!/bin/bash\n"}}

// contextDirRE is image-build's temp dir, normalised for the golden.
var contextDirRE = regexp.MustCompile(`context-[0-9]+`)

// leftContexts lists what image-build left under BuildDir.
func (f *fakeHost) leftContexts(t *testing.T) []string {
	t.Helper()
	left, err := filepath.Glob(f.root + BuildDir + "/*")
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// TestImageVerbs: image-build gets a root-only copy of the context with fixed modes and removes
// it after; image-exists and image-rm. The exact commands are the golden image.golden.
func TestImageVerbs(t *testing.T) {
	f := newHost(t)
	if code, out, errOut := runIn(t, f.opts(), contextTar(t, goodContext...), "image-build", tag); code != 0 || out != "" {
		t.Fatalf("image-build: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := map[string]string{"Dockerfile": "-rw-r--r-- FROM x\n", "base.sh": "-rwxr-xr-x #!/bin/bash\n"}
	if fmt.Sprint(f.built) != fmt.Sprint(want) {
		t.Errorf("the context built: %q, want %q", f.built, want)
	}
	if left := f.leftContexts(t); len(left) > 0 {
		t.Errorf("left %q", left)
	}
	if fi, err := os.Stat(f.root + BuildDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: %v %v", BuildDir, fi, err)
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"image-exists", tag}, 0},
		{[]string{"image-exists", "factory-agent-base:other"}, 1},
		{[]string{"image-rm", tag}, 0},
		{[]string{"image-exists", tag}, 1},
	} {
		if code, out, errOut := runIn(t, f.opts(), nil, c.args...); code != c.code || out != "" {
			t.Errorf("%v: code %d, stdout %q, stderr %q", c.args, code, out, errOut)
		}
	}
	calls := contextDirRE.ReplaceAllString(strings.ReplaceAll(strings.Join(f.calls, "\n"), f.root, ""), "context-N")
	golden(t, "image.golden", calls+"\n")
}

// TestImageBuildRefused: a context that isn't flat regular files with a Dockerfile, or exceeds
// the bounds, builds nothing and leaves nothing.
func TestImageBuildRefused(t *testing.T) {
	many := make([]file, maxContextFiles+1)
	for i := range many {
		many[i] = file{name: fmt.Sprintf("f%d", i)}
	}
	tests := []struct {
		name  string
		stdin *bytes.Buffer
		err   string
	}{
		{"a symlink", contextTar(t, append(goodContext, file{name: "x", typ: tar.TypeSymlink, body: "/etc/shadow"})...), `"x" isn't a regular file`},
		{"a hard link", contextTar(t, append(goodContext, file{name: "x", typ: tar.TypeLink, body: "Dockerfile"})...), `"x" isn't a regular file`},
		{"a directory", contextTar(t, append(goodContext, file{name: "d", typ: tar.TypeDir})...), `"d" isn't a regular file`},
		{"a path", contextTar(t, append(goodContext, file{name: "d/x"})...), `"d/x" isn't a regular file at the top level`},
		{"dot-dot", contextTar(t, append(goodContext, file{name: "../x"})...), `"../x" isn't`},
		{"a dotfile", contextTar(t, append(goodContext, file{name: ".x"})...), `".x" isn't`},
		{"twice", contextTar(t, append(goodContext, file{name: "base.sh"})...), "file exists"},
		{"no Dockerfile", contextTar(t, file{name: "base.sh"}), "no Dockerfile"},
		{"too many files", contextTar(t, many...), "more than 32 files"},
		{"too big", contextTar(t, file{name: "Dockerfile", body: strings.Repeat("x", maxContextBytes+1)}), "more than 16777216 bytes"},
		{"not a tar", bytes.NewBufferString(strings.Repeat("not a tar ", 100)), "the build context:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHost(t)
			code, _, errOut := runIn(t, f.opts(), tt.stdin, "image-build", tag)
			if code != 1 || !strings.HasPrefix(errOut, "kata-helper image-build: the build context: ") || !strings.Contains(errOut, tt.err) {
				t.Errorf("code %d, stderr %q, want %q", code, errOut, tt.err)
			}
			if len(f.calls) > 0 || len(f.leftContexts(t)) > 0 {
				t.Errorf("calls %q, left %q", f.calls, f.leftContexts(t))
			}
		})
	}
	t.Run("no stdin", func(t *testing.T) {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), nil, "image-build", tag); code != 1 || !strings.Contains(errOut, "no stdin") || len(f.calls) > 0 {
			t.Errorf("code %d, stderr %q", code, errOut)
		}
	})
}

// TestImageFailures: nerdctl build's and rmi's failures (an image a workspace uses) carry
// nerdctl's last line, after build's output; the context is removed after a failed build.
func TestImageFailures(t *testing.T) {
	f := newHost(t)
	f.fail = "build"
	if code, _, errOut := runIn(t, f.opts(), contextTar(t, goodContext...), "image-build", tag); code != 1 || errOut != "a warning\nboom\nkata-helper image-build: nerdctl build: exit status 1: boom\n" {
		t.Errorf("build: code %d, stderr %q", code, errOut)
	}
	if left := f.leftContexts(t); len(left) > 0 {
		t.Errorf("left %q", left)
	}
	f.fail = "rmi"
	if code, _, errOut := runIn(t, f.opts(), nil, "image-rm", tag); code != 1 || errOut != "kata-helper image-rm: nerdctl rmi: exit status 1: boom\n" {
		t.Errorf("rmi: code %d, stderr %q", code, errOut)
	}
}

// TestImageArgs: the image verbs refuse anything but ImageRepo:<version>, before running
// anything.
func TestImageArgs(t *testing.T) {
	for _, args := range [][]string{
		{"image-build", "ubuntu:24.04"},
		{"image-build", "factory-agent-base:-x"},
		{"image-build", "factory-agent-base"},
		{"image-exists", "factory-agent-base:a b"},
		{"image-rm", "docker.io/library/factory-agent-base:dev"},
		{"image-rm", "factory-agent-base:dev@sha256:00"},
	} {
		f := newHost(t)
		code, _, errOut := runIn(t, f.opts(), contextTar(t, goodContext...), args...)
		if code != 2 || !strings.HasPrefix(errOut, "kata-helper "+args[0]+": ") || len(f.calls) > 0 || len(f.files()) > 0 {
			t.Errorf("%q: code %d, stderr %q, calls %v", args, code, errOut, f.calls)
		}
	}
	for _, args := range [][]string{{"image-build"}, {"image-rm", tag, tag}, {"image-exists"}} {
		f := newHost(t)
		if code, _, errOut := runIn(t, f.opts(), nil, args...); code != 2 || errOut != Usage+"\n" || len(f.calls) > 0 {
			t.Errorf("%q: code %d, stderr %q", args, code, errOut)
		}
	}
}
