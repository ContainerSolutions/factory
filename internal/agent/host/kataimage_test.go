package host

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/host/provision"
)

// fakeKata answers sudo -n kata-helper's verbs. A call whose arguments contain fail fails,
// with the helper's one line.
type fakeKata struct {
	fail string
	// images are the images in the factory's containerd; inUse makes image-rm fail, as nerdctl
	// does while a workspace uses the image.
	images map[string]bool
	inUse  bool
	// built is the last image-build's context: each file's name and mode.
	built []string
	// warm is whether the warm disk exists; ws are the workspaces (created, running or
	// stopped); dockerDown is how many docker info execs fail before dockerd answers.
	warm       bool
	ws         map[string]string
	dockerDown int
}

func (k *fakeKata) run(call string, out io.Writer, in io.Reader, args []string) error {
	fail := func(msg string) error {
		_, _ = fmt.Fprintf(out, "kata-helper %s: %s\n", args[0], msg)
		return errors.New("exit status 1")
	}
	if k.fail != "" && strings.Contains(" "+strings.Join(args, " ")+" ", k.fail) {
		return fail("boom")
	}
	if k.images == nil {
		k.images = map[string]bool{}
	}
	if k.ws == nil {
		k.ws = map[string]string{}
	}
	switch args[0] {
	case "start", "stop", "exec", "warm-load", "warm-promote":
		if k.ws[args[1]] == "" {
			return fail("no workspace " + args[1])
		}
	}
	switch args[0] {
	case "image-build":
		k.built = nil
		tr := tar.NewReader(in)
		for {
			hd, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fail(err.Error())
			}
			k.built = append(k.built, fmt.Sprintf("%s %04o", hd.Name, hd.Mode))
		}
		k.images[args[1]] = true
	case "image-exists":
		if !k.images[args[1]] {
			return fail("nerdctl image: exit status 1: no such image: " + args[1])
		}
	case "image-rm":
		if k.inUse {
			return fail("nerdctl rmi: exit status 1: image is being used by stopped container ws-tsk_1")
		}
		delete(k.images, args[1])
	case "warm-exists":
		if !k.warm {
			return fail("no warm disk /var/lib/factory-agent/kata/warm-docker.img")
		}
	case "create":
		if k.ws[args[1]] != "" {
			return fail(args[1] + " already exists (slot 1)")
		}
		k.ws[args[1]] = "created"
	case "start":
		k.ws[args[1]] = "running"
	case "stop":
		k.ws[args[1]] = "stopped"
	case "rm":
		delete(k.ws, args[1])
	case "exec", "warm-load":
		if k.ws[args[1]] != "running" {
			return fail("nerdctl exec: " + args[1] + " isn't running")
		}
		if strings.HasSuffix(call, " docker info") && k.dockerDown > 0 {
			k.dockerDown--
			_, _ = io.WriteString(out, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n")
			return errors.New("exit status 1")
		}
	case "warm-promote":
		if k.ws[args[1]] != "stopped" {
			return fail(args[1] + " is running: stop it first")
		}
		k.warm = true
	default:
		return fail("unexpected " + call)
	}
	return nil
}

// installImage writes katabase.json for this binary's image, in the factory's containerd.
func installImage(t *testing.T, e *Env, r *fakeRunner) KataBaseRecord {
	t.Helper()
	sum, err := provision.KataContextSHA256()
	if err != nil {
		t.Fatal(err)
	}
	rec := KataBaseRecord{Image: e.KataImage(), ContextSHA256: sum, BuiltAt: "2026-10-20T08:00:00Z"}
	writeTestRecord(t, e.KataBaseRecordPath(), rec)
	if r.kata.images == nil {
		r.kata.images = map[string]bool{}
	}
	r.kata.images[rec.Image] = true
	return rec
}

func writeTestRecord(t *testing.T, p string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(p, v); err != nil {
		t.Fatal(err)
	}
}

func TestKataImage(t *testing.T) {
	e := &Env{AgentVersion: "v1.2.3+dirty"}
	if got := e.KataImage(); got != "factory-agent-base:1.2.3_dirty" {
		t.Errorf("KataImage %q", got)
	}
}

func TestKataImageCheck(t *testing.T) {
	runStepCases(t, []stepCase{
		{"kataimage", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "factory-agent-base:0.1.0 (built 2026-10-20T08:00:00Z)"},
		{"kataimage", "no record", func(t *testing.T, e *Env, _ *fakeRunner) {
			if err := os.Remove(e.KataBaseRecordPath()); err != nil {
				t.Fatal(err)
			}
		}, StateMissing, filepath.Join("state", "factory-agent", "katabase.json")},
		{"kataimage", "another version's", func(t *testing.T, e *Env, _ *fakeRunner) {
			e.AgentVersion = "v0.2.0"
		}, StateOutdated, "katabase.json: its image is factory-agent-base:0.1.0, want factory-agent-base:0.2.0: host init rebuilds it"},
		{"kataimage", "another context", func(t *testing.T, e *Env, r *fakeRunner) {
			rec := installImage(t, e, r)
			rec.ContextSHA256 = "0123"
			writeTestRecord(t, e.KataBaseRecordPath(), rec)
		}, StateOutdated, "its contextSHA256 is 0123, want "},
		{"kataimage", "not in containerd", func(_ *testing.T, e *Env, r *fakeRunner) { delete(r.kata.images, e.KataImage()) }, StateMissing,
			"factory-agent-base:0.1.0 isn't in the factory's containerd (kata-helper image-exists: nerdctl image: exit status 1: no such image"},
		{"kataimage", "the helper refused", func(_ *testing.T, _ *Env, r *fakeRunner) { r.kata.fail = "image-exists" }, StateMissing,
			"(kata-helper image-exists: boom)"},
	})
	e, r := kataEnv(t)
	installKata(t, e, r)
	if err := os.WriteFile(e.KataBaseRecordPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := Check(context.Background(), []Step{kataImageStep{e}}); st[0].State != StateWrong || !strings.Contains(st[0].Detail, "katabase.json: unexpected end of JSON input") {
		t.Errorf("an unreadable record: %+v", st[0])
	}
}

// buildImage applies the image step to a host where every other kata step is ok, the previous
// version's image (prev) recorded; it returns the output and the error.
func buildImage(t *testing.T, prev string, set func(*Env, *fakeRunner)) (*Env, *fakeRunner, string, error) {
	t.Helper()
	e, r := kataEnv(t)
	installKata(t, e, r)
	if prev != "" {
		writeTestRecord(t, e.KataBaseRecordPath(), KataBaseRecord{Image: prev, ContextSHA256: "old", BuiltAt: "2026-10-01T00:00:00Z"})
		r.kata.images[prev] = true
	}
	if set != nil {
		set(e, r)
	}
	var out bytes.Buffer
	e.Out = &out
	r.calls = nil
	err := kataImageStep{e}.Apply(context.Background())
	return e, r, out.String(), err
}

// helperCalls are the kata-helper calls the runner saw.
func (r *fakeRunner) helperCalls() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "sudo -n ") {
			out = append(out, c)
		}
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestKataImageBuild: the build gets this binary's context as a tar on stdin, the record is
// written, and the previous version's image removed (golden kataimage-build.golden).
func TestKataImageBuild(t *testing.T) {
	e, r, out, err := buildImage(t, "factory-agent-base:0.0.9", nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{"Dockerfile 0644", "base.sh 0755", "factory-docker-storage 0755", "factory-docker-storage.service 0644", "factory-init 0755"}
	if !slices.Equal(r.kata.built, want) {
		t.Errorf("the context %q, want %q", r.kata.built, want)
	}
	var rec KataBaseRecord
	if err := json.Unmarshal([]byte(readFile(t, e.KataBaseRecordPath())), &rec); err != nil {
		t.Fatal(err)
	}
	if sum, _ := provision.KataContextSHA256(); rec != (KataBaseRecord{Image: "factory-agent-base:0.1.0", ContextSHA256: sum, BuiltAt: "2026-10-22T09:30:00Z"}) {
		t.Errorf("record %+v", rec)
	}
	if r.kata.images["factory-agent-base:0.0.9"] || !r.kata.images["factory-agent-base:0.1.0"] {
		t.Errorf("images %v", r.kata.images)
	}
	golden(t, "kataimage-build.golden", e, strings.Join(r.helperCalls(), "\n")+"\n")
	wantState(t, check(t, kataImageStep{e}), StateOK, "factory-agent-base:0.1.0")
}

// TestKataImageBuildCases: an image in use stays, with a warning; a rebuild under the same tag
// or a foreign previous image removes nothing; a failed build writes no record.
func TestKataImageBuildCases(t *testing.T) {
	t.Run("previous in use", func(t *testing.T) {
		_, r, out, err := buildImage(t, "factory-agent-base:0.0.9", func(_ *Env, r *fakeRunner) { r.kata.inUse = true })
		if err != nil || !r.kata.images["factory-agent-base:0.0.9"] ||
			!strings.Contains(out, "warning: the previous image factory-agent-base:0.0.9 is left (a workspace may still use it: sudo -n ") {
			t.Errorf("err %v, images %v\n%s", err, r.kata.images, out)
		}
	})
	for _, prev := range []string{"factory-agent-base:0.1.0", "ubuntu:24.04", ""} {
		t.Run("previous "+prev, func(t *testing.T) {
			_, r, _, err := buildImage(t, prev, nil)
			if calls := r.helperCalls(); err != nil || len(calls) != 1 || !strings.Contains(calls[0], " image-build factory-agent-base:0.1.0 <stdin") {
				t.Errorf("err %v, calls %q", err, calls)
			}
		})
	}
	t.Run("the build fails", func(t *testing.T) {
		e, r, _, err := buildImage(t, "factory-agent-base:0.0.9", func(_ *Env, r *fakeRunner) { r.kata.fail = "image-build" })
		if err == nil || !strings.Contains(err.Error(), "kata-helper image-build: ") || len(r.helperCalls()) != 1 {
			t.Fatalf("err %v, calls %q", err, r.helperCalls())
		}
		if got := readFile(t, e.KataBaseRecordPath()); !strings.Contains(got, `"image": "factory-agent-base:0.0.9"`) {
			t.Errorf("the record changed: %s", got)
		}
	})
}

func TestKataImagePlan(t *testing.T) {
	e, r := kataEnv(t)
	installKata(t, e, r)
	writeTestRecord(t, e.KataBaseRecordPath(), KataBaseRecord{Image: "factory-agent-base:0.0.9"})
	p, err := kataImageStep{e}.Plan(context.Background())
	if err != nil || len(p.Root) > 0 || len(p.Do) != 4 ||
		p.Do[3] != "kata-helper image-rm factory-agent-base:0.0.9 (the previous image, named in katabase.json), unless a workspace uses it" {
		t.Errorf("plan %q, %v", p.Do, err)
	}
}
