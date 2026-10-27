package effector_test

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/effector"
	"github.com/compliance-framework/factory/internal/agent/pipeline"
)

// startFactoryMock builds cmd/factory-mock and runs it on an ephemeral port.
func startFactoryMock(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH to build factory-mock")
	}
	bin := filepath.Join(t.TempDir(), "factory-mock")
	if out, err := exec.Command(gobin, "build", "-o", bin, "github.com/compliance-framework/factory/cmd/factory-mock").CombinedOutput(); err != nil {
		t.Fatalf("build factory-mock: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "--addr", "127.0.0.1:0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	addrs := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		re := regexp.MustCompile(`addr=(\S+)`)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case addrs <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case a := <-addrs:
		return "http://" + a
	case <-time.After(60 * time.Second):
		t.Fatal("factory-mock didn't start")
		return ""
	}
}

// TestAgainstFactoryMock is AGT-S1-T05's done criterion: every op against factory-mock (the
// contract's createEffect example): the API-side ops from the CLI, the git-side ops in a
// workspace with their records.
func TestAgainstFactoryMock(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs factory-mock")
	}
	base := startFactoryMock(t)
	r := newRun(t, nil, nil)
	r.env[pipeline.EnvAPI] = base
	for _, c := range apiCases(t) {
		code, stdout, stderr := r.cli(c.argv...)
		if code != 0 {
			t.Errorf("%s: exit %d %s", c.name, code, stderr)
			continue
		}
		out := decode(t, stdout)
		if out["effectId"] != "eff_01JAB3N4A5B6C7D8E9F0G1H2JK" || out["ref"] != "compliance-framework/docs#42" {
			t.Errorf("%s: %v", c.name, out)
		}
	}
	w := newWorkspace(t, nil)
	e := effector.New(effector.Options{API: client(t, base), TaskID: taskID, Config: &w.run.cfg, State: filepath.Join(t.TempDir(), "state"),
		Git: w.gitBin, Env: w.env})
	w.commit("stack/1", "one.md", "one\n")
	for _, op := range []string{"push", "stack_push", "stack_rebase"} {
		out, err := e.Do(context.Background(), op, nil)
		if err != nil || out["effectId"] != "eff_01JAB3N4A5B6C7D8E9F0G1H2JK" {
			t.Errorf("%s: %v %v", op, out, err)
		}
	}
}
