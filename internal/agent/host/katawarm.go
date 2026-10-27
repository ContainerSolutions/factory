package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// The kata driver's warm step (workspace-kata.md, step 10): the warm disk, a workspace disk
// pre-filled with workspace.warm's images that create copies for every warm workspace.

// KataWarmStep is the warm step's name: supervise gives the kata driver the warm disk when
// host check reports it ok.
const KataWarmStep = "katawarm"

// KataWarmBuild is the warm step's build workspace. supervise's orphan cleanup leaves it alone:
// host init holds host.lock while it exists, and removes it on every exit path but
// --keep-failed.
const KataWarmBuild = "ws-tsk_factory-warm-build"

// warmCleanup bounds removing the build workspace after a failure or an interrupt: the helper's
// rm waits up to a minute for each of nerdctl stop and rm -f.
const warmCleanup = 5 * time.Minute

// KataWarmRecord is <statedir>/katawarm.json: what the warm disk was built from.
type KataWarmRecord struct {
	Image string `json:"image"`
	// ImageBuiltAt is the image's builtAt: an image rebuilt under the same tag rebuilds the warm
	// disk too.
	ImageBuiltAt string `json:"imageBuiltAt"`
	// ImagesSHA256 is workspace.warm's hash, as warmbase.json's.
	ImagesSHA256 string `json:"imagesSHA256"`
	// DiskGiB is workspace.size.disk when it was built: every warm workspace's disk is a copy
	// of the warm disk, so a new size rebuilds it.
	DiskGiB int    `json:"diskGiB"`
	BuiltAt string `json:"builtAt"`
}

// KataWarmRecordPath is katawarm.json.
func (e *Env) KataWarmRecordPath() string { return filepath.Join(e.StateDir(), "katawarm.json") }

// helper runs the helper's verb with sudo -n; e.Out gets its output.
func (e *Env) helper(ctx context.Context, args ...string) error {
	out, stop := e.watchSudo(ctx, "kata-helper "+args[0])
	err := e.Runner.Run(ctx, out, "sudo", e.helperArgv(args...)...)
	stop()
	if err != nil {
		return fmt.Errorf("kata-helper %s: %w", args[0], err)
	}
	return nil
}

// kataWarmStep builds the warm disk.
type kataWarmStep struct{ e *Env }

func (kataWarmStep) Name() string { return KataWarmStep }

func (kataWarmStep) throughHelper() string { return "the warm disk is built" }

func (s kataWarmStep) Check(ctx context.Context) (Status, error) {
	e, p := s.e, s.e.KataWarmRecordPath()
	var got KataWarmRecord
	if ok, err := readRecord(p, &got); err != nil || !ok {
		return Status{State: StateMissing, Detail: "no " + p}, err
	}
	img, err := kataImageStep{e}.Check(ctx)
	if err != nil {
		return Status{}, err
	}
	if img.State != StateOK {
		return Status{State: StateOutdated, Detail: "the image is " + string(img.State) + ": the warm disk follows it"}, nil
	}
	var b KataBaseRecord
	if _, err := readRecord(e.KataBaseRecordPath(), &b); err != nil {
		return Status{}, err
	}
	for _, f := range []struct{ field, got, want string }{
		{"image", got.Image, b.Image},
		{"imageBuiltAt", got.ImageBuiltAt, b.BuiltAt},
		{"imagesSHA256", got.ImagesSHA256, warmBaseStep{e}.imagesSHA256()},
		{"diskGiB", strconv.Itoa(got.DiskGiB), strconv.Itoa(e.Workspace.Size.DiskGiB())},
	} {
		if f.got != f.want {
			return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: its %s is %s, want %s: host init rebuilds it", filepath.Base(p), f.field, f.got, f.want)}, nil
		}
	}
	if err := e.helperOK(ctx, "warm-exists"); err != nil {
		return Status{State: StateMissing, Detail: fmt.Sprintf("no warm disk (%v)", err)}, nil
	}
	w := e.Workspace.Warm
	return Status{State: StateOK, Detail: fmt.Sprintf("the warm disk from %s, %d images, %d kind images (built %s)", b.Image, len(w.Images), len(w.KindImages), got.BuiltAt)}, nil
}

// size is the build workspace's create arguments: the workspace size's, CPUs less the one Kata
// adds, as the driver passes them.
func (s kataWarmStep) size() (cpus, memMiB, diskGiB string) {
	sz := s.e.Workspace.Size
	return strconv.Itoa(max(sz.CPUs-1, 1)), strconv.Itoa(sz.MemoryGiB() * 1024), strconv.Itoa(sz.DiskGiB())
}

func (s kataWarmStep) Plan(context.Context) (Plan, error) {
	e, b, w := s.e, KataWarmBuild, s.e.Workspace.Warm
	saves, err := warmBaseStep{e}.kindSaves()
	if err != nil {
		return Plan{}, err
	}
	cpus, mem, diskGiB := s.size()
	do := []string{
		"kata-helper rm " + b + " (a build workspace left by an earlier run)",
		fmt.Sprintf("kata-helper create %s %s %s %s empty %s, start it, wait for its dockerd (%s)", b, cpus, mem, diskGiB, e.KataImage(), e.VM.Boot),
	}
	for _, im := range w.Images {
		do = append(do, "kata-helper warm-load "+b+" "+im+" (pulled on the host, docker load in the workspace)")
	}
	for _, k := range saves {
		tag := ""
		if k.tag != k.ref {
			tag = ", docker tag it " + k.tag
		}
		do = append(do, "kata-helper warm-load "+b+" "+k.ref+tag+", then docker save --platform "+katahelper.WarmPlatform+" -o "+k.archive+" "+k.tag+" and chmod 0644 it in the workspace")
	}
	if len(w.Images)+len(w.KindImages) == 0 {
		do = append(do, "no warm images: the warm disk is the build workspace's freshly formatted, empty one")
	}
	return Plan{Do: append(do,
		fmt.Sprintf("kata-helper stop %s, warm-promote %s (its disk becomes the warm disk), rm %s; write %s", b, b, b, e.KataWarmRecordPath()))}, nil
}

// Apply builds the warm disk in the build workspace, which it removes on any failure, unless
// KeepFailed. It refuses to start unless the image step is ok (--only katawarm on a stale
// image).
func (s kataWarmStep) Apply(ctx context.Context) (err error) {
	e, b := s.e, KataWarmBuild
	if st, err := (kataImageStep{e}).Check(ctx); err != nil {
		return err
	} else if st.State != StateOK {
		return fmt.Errorf("the image is %s (%s): build it first (--only kataimage,katawarm)", st.State, st.Detail)
	}
	var img KataBaseRecord
	if _, err := readRecord(e.KataBaseRecordPath(), &img); err != nil {
		return err
	}
	saves, err := warmBaseStep{e}.kindSaves()
	if err != nil {
		return err
	}
	// a leftover of --keep-failed or a killed init.
	if err := e.helper(ctx, "rm", b); err != nil {
		return err
	}
	defer func() {
		switch {
		case err == nil:
		case e.KeepFailed:
			fmt.Fprintf(e.Out, "keeping the build workspace %s (--keep-failed): sudo -n %s rm %s removes it\n", b, e.sys(katahelper.HelperPath), b)
		default:
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), warmCleanup)
			defer cancel()
			// the helper's rm stops the workspace first (nerdctl stop -t 30): after an interrupt,
			// say why init waits.
			if ctx.Err() != nil {
				fmt.Fprintf(e.Out, "interrupted: removing the build workspace %s (it can take a minute; Ctrl-C again leaves it, and the next host init removes it)\n", b)
			}
			if rerr := e.helper(cctx, "rm", b); rerr != nil {
				err = errors.Join(err, fmt.Errorf("removing %s: %w", b, rerr))
			}
		}
	}()
	cpus, mem, diskGiB := s.size()
	fmt.Fprintf(e.Out, "building the warm disk in %s\n", b)
	for _, args := range [][]string{{"create", b, cpus, mem, diskGiB, "empty", img.Image}, {"start", b}} {
		if err := e.helper(ctx, args...); err != nil {
			return err
		}
	}
	if err := s.waitDocker(ctx); err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, e.VM.Provision)
	defer cancel()
	for _, im := range e.Workspace.Warm.Images {
		if err := e.helper(pctx, "warm-load", b, im); err != nil {
			return err
		}
	}
	for _, k := range saves {
		cmds := [][]string{{"warm-load", b, k.ref}}
		if k.tag != k.ref {
			cmds = append(cmds, []string{"exec", b, "sudo", "-n", "docker", "tag", k.ref, k.tag})
		}
		// warm-load pulled one platform, so the archive names only it (kind's import needs every
		// manifest an index names); up loads it as admin, so it's readable.
		cmds = append(cmds, []string{"exec", b, "sudo", "-n", "docker", "save", "--platform", katahelper.WarmPlatform, "-o", k.archive, k.tag},
			[]string{"exec", b, "sudo", "-n", "chmod", "0644", k.archive})
		for _, c := range cmds {
			if err := e.helper(pctx, c...); err != nil {
				return err
			}
		}
	}
	for _, verb := range []string{"stop", "warm-promote", "rm"} {
		if err := e.helper(ctx, verb, b); err != nil {
			return err
		}
	}
	return writeRecord(e.KataWarmRecordPath(), KataWarmRecord{
		Image: img.Image, ImageBuiltAt: img.BuiltAt, ImagesSHA256: warmBaseStep{e}.imagesSHA256(),
		DiskGiB: e.Workspace.Size.DiskGiB(), BuiltAt: e.Clock.Now().UTC().Format(time.RFC3339),
	})
}

// waitDocker polls docker info in the build workspace every VM.Poll until it answers, for
// VM.Boot: the exec itself fails until the VM's systemd is up, then docker info until dockerd
// is. The last failure is the error.
func (s kataWarmStep) waitDocker(ctx context.Context) error {
	e := s.e
	wctx, cancel := context.WithTimeout(ctx, e.VM.Boot)
	defer cancel()
	t := time.NewTicker(e.VM.Poll)
	defer t.Stop()
	for {
		err := e.helperOK(wctx, "exec", KataWarmBuild, "sudo", "-n", "docker", "info")
		if err == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s's dockerd didn't answer in %s: %w", KataWarmBuild, e.VM.Boot, err)
		case <-t.C:
		}
	}
}
