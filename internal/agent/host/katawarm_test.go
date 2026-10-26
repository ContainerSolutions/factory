package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// installWarm writes katawarm.json for the installed image and workspace.warm, and makes the
// warm disk.
func installWarm(t *testing.T, e *Env, r *fakeRunner) {
	t.Helper()
	var img KataBaseRecord
	if _, err := readRecord(e.KataBaseRecordPath(), &img); err != nil {
		t.Fatal(err)
	}
	writeTestRecord(t, e.KataWarmRecordPath(), KataWarmRecord{Image: img.Image, ImageBuiltAt: img.BuiltAt, ImagesSHA256: warmBaseStep{e}.imagesSHA256(), DiskGiB: e.Workspace.Size.DiskGiB(), BuiltAt: "2026-10-20T09:00:00Z"})
	r.kata.warm = true
}

// testWarm are warm images of each kind: a plain one, a digest-pinned kind image (tagged
// before the save) and a tagged one.
var testWarm = config.WarmImages{Images: []string{"postgres:17.5"}, KindImages: []string{"ghcr.io/x/agent:0.7.1@sha256:ab", "kindest/node:v1.34.0"}}

func TestKataWarmBuildName(t *testing.T) {
	if err := workspace.ValidName(KataWarmBuild); err != nil {
		t.Error(err)
	}
}

func TestKataWarmCheck(t *testing.T) {
	runStepCases(t, []stepCase{
		{"katawarm", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "the warm disk from factory-agent-base:0.1.0, 0 images, 0 kind images (built 2026-10-20T09:00:00Z)"},
		{"katawarm", "no record", func(t *testing.T, e *Env, _ *fakeRunner) { removeRecord(t, e.KataWarmRecordPath()) }, StateMissing, "katawarm.json"},
		{"katawarm", "the image outdated", func(_ *testing.T, e *Env, _ *fakeRunner) { e.AgentVersion = "v0.2.0" }, StateOutdated,
			"the image is outdated: the warm disk follows it"},
		{"katawarm", "the image rebuilt", func(t *testing.T, e *Env, r *fakeRunner) {
			rec := installImage(t, e, r)
			rec.BuiltAt = "2026-10-21T00:00:00Z"
			writeTestRecord(t, e.KataBaseRecordPath(), rec)
		}, StateOutdated, "katawarm.json: its imageBuiltAt is 2026-10-20T08:00:00Z, want 2026-10-21T00:00:00Z: host init rebuilds it"},
		{"katawarm", "other warm images", func(_ *testing.T, e *Env, _ *fakeRunner) { e.Workspace.Warm = testWarm }, StateOutdated, "its imagesSHA256 is "},
		{"katawarm", "a kind image moved", func(t *testing.T, e *Env, r *fakeRunner) {
			e.Workspace.Warm = testWarm
			installWarm(t, e, r)
			e.Workspace.Warm = config.WarmImages{Images: append([]string{"kindest/node:v1.34.0"}, testWarm.Images...), KindImages: testWarm.KindImages[:1]}
		}, StateOutdated, "its imagesSHA256 is "},
		{"katawarm", "another disk size", func(_ *testing.T, e *Env, _ *fakeRunner) { e.Workspace.Size.Disk = "120Gi" }, StateOutdated,
			"katawarm.json: its diskGiB is 60, want 120: host init rebuilds it"},
		{"katawarm", "a record without the disk size", func(t *testing.T, e *Env, r *fakeRunner) {
			installWarm(t, e, r)
			var rec KataWarmRecord
			if _, err := readRecord(e.KataWarmRecordPath(), &rec); err != nil {
				t.Fatal(err)
			}
			rec.DiskGiB = 0
			writeTestRecord(t, e.KataWarmRecordPath(), rec)
		}, StateOutdated, "its diskGiB is 0, want 60"},
		{"katawarm", "no warm disk", func(_ *testing.T, _ *Env, r *fakeRunner) { r.kata.warm = false }, StateMissing,
			"no warm disk (kata-helper warm-exists: no warm disk /var/lib/factory-agent/kata/warm-docker.img)"},
	})
}

func removeRecord(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// buildWarm applies the warm step to a host whose other kata steps are ok; it returns the
// output and the error.
func buildWarm(t *testing.T, set func(*Env, *fakeRunner)) (*Env, *fakeRunner, string, error) {
	t.Helper()
	e, r := kataEnv(t)
	installKata(t, e, r)
	r.kata.warm = false
	removeRecord(t, e.KataWarmRecordPath())
	e.Workspace.Warm = testWarm
	if set != nil {
		set(e, r)
	}
	var out bytes.Buffer
	e.Out = &out
	r.calls = nil
	err := kataWarmStep{e}.Apply(context.Background())
	return e, r, out.String(), err
}

// TestKataWarmBuild: the build workspace from the image, an empty disk, each warm image loaded
// and each kind image saved, its disk promoted, the workspace removed and the record written
// (golden katawarm-build.golden).
func TestKataWarmBuild(t *testing.T) {
	e, r, out, err := buildWarm(t, func(_ *Env, r *fakeRunner) { r.kata.dockerDown = 2 })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !r.kata.warm || len(r.kata.ws) > 0 {
		t.Errorf("warm %v, workspaces %v", r.kata.warm, r.kata.ws)
	}
	var rec KataWarmRecord
	if err := json.Unmarshal([]byte(readFile(t, e.KataWarmRecordPath())), &rec); err != nil {
		t.Fatal(err)
	}
	if rec != (KataWarmRecord{Image: "factory-agent-base:0.1.0", ImageBuiltAt: "2026-10-20T08:00:00Z", ImagesSHA256: warmBaseStep{e}.imagesSHA256(), DiskGiB: 60, BuiltAt: "2026-10-22T09:30:00Z"}) {
		t.Errorf("record %+v", rec)
	}
	golden(t, "katawarm-build.golden", e, strings.Join(r.helperCalls(), "\n")+"\n")
	wantState(t, check(t, kataWarmStep{e}), StateOK, "1 images, 2 kind images")
}

// TestKataWarmNoImages: with no warm images the build workspace's empty disk, formatted at
// its boot, is promoted.
func TestKataWarmNoImages(t *testing.T) {
	_, r, _, err := buildWarm(t, func(e *Env, _ *fakeRunner) { e.Workspace.Warm = config.WarmImages{} })
	var verbs []string
	for _, c := range r.helperCalls() {
		verbs = append(verbs, strings.Fields(c)[3])
	}
	if err != nil || !r.kata.warm || strings.Join(verbs, " ") != "image-exists rm create start exec stop warm-promote rm" {
		t.Errorf("err %v, warm %v, verbs %v", err, r.kata.warm, verbs)
	}
}

// TestKataWarmBuildFails: a failure at each stage removes the build workspace and writes no
// record; --keep-failed keeps it; a stale image is refused before anything runs.
func TestKataWarmBuildFails(t *testing.T) {
	for _, stage := range []string{"create", "start", "docker info", "warm-load", "docker tag", "docker save", " stop ", "warm-promote"} {
		for _, keep := range []bool{false, true} {
			t.Run(stage+map[bool]string{true: " --keep-failed"}[keep], func(t *testing.T) {
				e, r, out, err := buildWarm(t, func(e *Env, r *fakeRunner) {
					e.KeepFailed = keep
					if stage == "docker info" {
						r.kata.dockerDown, e.VM.Boot = 1<<30, 20*time.Millisecond
					} else {
						r.kata.fail = stage
					}
				})
				if err == nil {
					t.Fatal("no error")
				}
				if _, ok := r.kata.ws[KataWarmBuild]; ok != (keep && stage != "create") {
					t.Errorf("the build workspace kept %v, want %v; calls %q", ok, keep, r.helperCalls())
				}
				if keep && !strings.Contains(out, "keeping the build workspace "+KataWarmBuild+" (--keep-failed)") {
					t.Errorf("output %q", out)
				}
				if found, _ := readRecord(e.KataWarmRecordPath(), &KataWarmRecord{}); found {
					t.Error("a record was written")
				}
			})
		}
	}
	t.Run("dockerd never answers", func(t *testing.T) {
		_, _, _, err := buildWarm(t, func(e *Env, r *fakeRunner) { r.kata.dockerDown, e.VM.Boot = 1<<30, 20*time.Millisecond })
		if err == nil || !strings.Contains(err.Error(), KataWarmBuild+"'s dockerd didn't answer in 20ms: kata-helper exec: exit status 1: Cannot connect to the Docker daemon") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a leftover", func(t *testing.T) {
		_, r, _, err := buildWarm(t, func(_ *Env, r *fakeRunner) { r.kata.ws = map[string]string{KataWarmBuild: "stopped"} })
		if err != nil || !r.kata.warm {
			t.Errorf("err %v, calls %q", err, r.helperCalls())
		}
	})
	t.Run("a stale image", func(t *testing.T) {
		_, r, _, err := buildWarm(t, func(e *Env, _ *fakeRunner) { e.AgentVersion = "v0.2.0" })
		if err == nil || !strings.Contains(err.Error(), "the image is outdated") || !strings.Contains(err.Error(), "build it first (--only kataimage,katawarm)") ||
			len(r.helperCalls()) > 0 {
			t.Errorf("err %v, calls %q", err, r.helperCalls())
		}
	})
}

func TestKataWarmPlan(t *testing.T) {
	e, _ := kataEnv(t)
	e.Workspace.Warm = testWarm
	p, err := kataWarmStep{e}.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Do, "\n")
	for _, want := range []string{
		"kata-helper create " + KataWarmBuild + " 5 8192 60 empty factory-agent-base:0.1.0, start it, wait for its dockerd (5s)",
		"kata-helper warm-load " + KataWarmBuild + " postgres:17.5 (pulled on the host",
		"kata-helper warm-load " + KataWarmBuild + " ghcr.io/x/agent:0.7.1@sha256:ab, docker tag it ghcr.io/x/agent:0.7.1, then docker save -o /var/lib/factory/kind-images/ghcr.io_x_agent_0.7.1.tar ghcr.io/x/agent:0.7.1 in the workspace",
		"warm-promote " + KataWarmBuild + " (its disk becomes the warm disk)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan:\n%s\nwant %q", got, want)
		}
	}
	e.Workspace.Warm.KindImages = []string{"a/b:1", "a_b:1"}
	if _, err := (kataWarmStep{e}).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "both save to") {
		t.Errorf("two kind images with one archive: %v", err)
	}
}
