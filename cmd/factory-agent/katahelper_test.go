package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/katahelper"
)

// TestKataHelperArgv0: factory-agent run as kata-helper is the helper, whatever its other
// arguments; under its own name it isn't.
func TestKataHelperArgv0(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no kata helper on windows")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH to build factory-agent")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "factory-agent")
	if out, err := exec.Command(gobin, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	helper := filepath.Join(dir, "kata-helper")
	if err := os.Link(bin, helper); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(helper, "protocol").Output(); err != nil || string(out) != strconv.Itoa(katahelper.Protocol)+"\n" {
		t.Errorf("kata-helper protocol: %q %v", out, err)
	}
	var ee *exec.ExitError
	for _, args := range [][]string{nil, {"version"}, {"host", "check"}} {
		out, err := exec.Command(helper, args...).CombinedOutput()
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || string(out) != katahelper.Usage+"\n" {
			t.Errorf("kata-helper %v: %q %v, want the helper's usage, exit 2", args, out, err)
		}
	}
	if out, err := exec.Command(bin, "protocol").CombinedOutput(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Errorf("factory-agent protocol: %q %v, want an unknown command", out, err)
	}
}
