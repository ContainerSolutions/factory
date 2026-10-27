package worker_test

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/enroll"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
	"github.com/ContainerSolutions/factory/internal/agent/worker"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// recorder is an HTTP client that records each exchange as "METHOD path status".
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) Do(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	status := "error"
	if err == nil {
		status = res.Status[:3]
	}
	r.mu.Lock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Path+" "+status)
	r.mu.Unlock()
	return res, err
}

func (r *recorder) Seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// index is the position of the first entry matching re at or after from, or -1.
func index(seen []string, from int, re string) int {
	rx := regexp.MustCompile(re)
	for i := from; i < len(seen); i++ {
		if rx.MatchString(seen[i]) {
			return i
		}
	}
	return -1
}

// buildBinary builds a main package of the module into the test's temp dir.
func buildBinary(t *testing.T, pkg string) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH to build " + pkg)
	}
	bin := filepath.Join(t.TempDir(), filepath.Base(pkg))
	if out, err := exec.Command(gobin, "build", "-o", bin, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// startFactoryMock builds cmd/factory-mock and runs it on an ephemeral port.
func startFactoryMock(t *testing.T) string {
	t.Helper()
	bin := buildBinary(t, "github.com/ContainerSolutions/factory/cmd/factory-mock")
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
	case <-time.After(30 * time.Second):
		t.Fatal("factory-mock didn't start")
		return ""
	}
}

// exampleRefreshToken is api/examples/registerAgent.json's refresh token.
const exampleRefreshToken = "arf_EXAMPLE-refresh-token-not-real-000000000000"

// TestAgainstFactoryMock is AGT-S1-T01's done criterion: against factory-mock (the contract's
// examples), the worker registers (an enrollment token from its file; the example's refresh
// token saved in the state dir), leases the canned task, heartbeats and completes it.
func TestAgainstFactoryMock(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs factory-mock")
	}
	base := startFactoryMock(t)
	tokFile := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(tokFile, []byte(enrollSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enr := enroll.Files{StateDir: t.TempDir(), TokenFile: tokFile}
	rec := &recorder{}
	logs := &logBuffer{}
	stub := &stubRunner{
		Outcome:  runner.Outcome{Result: &v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "triaged", Summary: "labelled area/docs"}},
		Progress: []v1alpha1.ProgressEvent{{At: time.Now().UTC(), Kind: "turn", Message: "triaging"}},
	}
	started := stub.Started()
	w, err := worker.New(worker.Options{
		Dispatcher: base, Pool: "maintainer-agents", Host: "mbp-01", AgentVersion: "test",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}}}},
		Lending:      v1alpha1.Lending{Unlimited: true},
		Enrollment:   enr,
		Runner:       stub,
		HTTPClient:   rec,
		Logger:       slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	var job *runner.Job
	select {
	case job = <-started:
	case err := <-done:
		t.Fatalf("Run returned before a lease: %v\n%s", err, logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("no lease\n%v\n%s", rec.Seen(), logs)
	}
	deadline := time.Now().Add(30 * time.Second)
	for index(rec.Seen(), 0, `^POST /api/v1/leases/\S+/complete 204$`) < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no complete\n%v\n%s", rec.Seen(), logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.Drain()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run didn't return after Drain")
	}

	seen := rec.Seen()
	i := 0
	for _, step := range []string{
		`^POST /api/v1/agents 201$`,
		`^GET /api/v1/agents/agt_gusfcarvalho-mbp-01/lease 200$`,
		`^POST /api/v1/leases/lse_01JAB3M2C4D5E6F7G8H9J0K1MN/heartbeat 200$`,
		`^POST /api/v1/leases/lse_01JAB3M2C4D5E6F7G8H9J0K1MN/complete 204$`,
	} {
		if i = index(seen, i, step); i < 0 {
			t.Fatalf("no %s in order in\n%s", step, strings.Join(seen, "\n"))
		}
	}
	if index(seen, 0, `^PATCH /api/v1/agents/agt_gusfcarvalho-mbp-01 200$`) < 0 || !strings.HasPrefix(seen[len(seen)-1], "DELETE /api/v1/agents/agt_gusfcarvalho-mbp-01 204") {
		t.Errorf("no drain PATCH or deregistration last:\n%s", strings.Join(seen, "\n"))
	}
	if job.TaskID() != "tsk_01JAB3K7Q2M8V9W4XYZ5N6P7RS" || job.Lease.Task.Spec.TaskSpec.Name != "triage" ||
		job.Git.Get().Token != "EXAMPLE-git-token-not-real" || job.Lease.Lease.HeartbeatEvery != "30s" {
		t.Errorf("job: task %s, lease %+v", job.TaskID(), job.Lease.Lease)
	}
	if tok, refresh, err := enr.Token(); err != nil || !refresh || tok != exampleRefreshToken {
		t.Errorf("the saved refresh token: %v %v", refresh, err)
	}
	for _, s := range []string{enrollSecret, exampleRefreshToken, "EXAMPLE-agent-token-not-real", "EXAMPLE-git-token-not-real"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a secret is in the logs: %q", s)
		}
	}
}
