package host

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// provisionScript is the base VM's provisioning script (provision/base.sh). basevm.json
// records its sha256, so editing it rebuilds the base.
//
//go:embed provision/base.sh
var provisionScript []byte

// ProvisionScriptSHA256 is the embedded provisioning script's sha256: basevm.json's
// scriptSHA256.
func ProvisionScriptSHA256() string {
	s := sha256.Sum256(provisionScript)
	return hex.EncodeToString(s[:])
}

// The VM steps' names, records and the files they use (P1 §2, workspace-host.md).
const (
	baseVMName   = "basevm"
	warmBaseName = "warmbase"
	// BaseVMPrefix names the base VM, followed by factory-agent's version. The names avoid
	// factory-ws-base, factory-ws-warm, factory-agent-host, ws-* and ws-gw: P1 phase 0's VMs,
	// which host init never touches.
	BaseVMPrefix = "factory-agent-base-"
	// WarmBaseVM is the warm base's name.
	WarmBaseVM = "factory-agent-warm"
	// buildingSuffix marks a VM being built; it is renamed into place once complete.
	buildingSuffix = ".building"
	// AgentLinuxName is the linux/arm64 factory-agent's default file name, next to the running
	// binary (make tart-up builds both).
	AgentLinuxName = "factory-agent-linux-arm64"
	// agentInVM is where the base VM has factory-agent.
	agentInVM = "/usr/local/bin/factory-agent"
	// KindImagesDir is where the warm base saves warm.kindImages, for kind load image-archive.
	KindImagesDir = "/var/lib/factory/kind-images"
)

// BaseVMRecord is <statedir>/basevm.json: what the base VM was built from.
type BaseVMRecord struct {
	Name         string `json:"name"`
	Source       string `json:"source"`
	ScriptSHA256 string `json:"scriptSHA256"`
	AgentSHA256  string `json:"agentSHA256"`
	AgentVersion string `json:"agentVersion"`
	BuiltAt      string `json:"builtAt"`
}

// WarmBaseRecord is <statedir>/warmbase.json.
type WarmBaseRecord struct {
	Base string `json:"base"`
	// BaseBuiltAt is the base's builtAt: a base rebuilt under the same name rebuilds the warm
	// base too.
	BaseBuiltAt  string `json:"baseBuiltAt"`
	ImagesSHA256 string `json:"imagesSHA256"`
	BuiltAt      string `json:"builtAt"`
}

var nameUnsafeRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// BaseVMName is the base VM's name for this factory-agent: factory-agent-base-0.1.0, or
// factory-agent-base-dev for a dev build.
func (e *Env) BaseVMName() string {
	return BaseVMPrefix + nameUnsafeRE.ReplaceAllString(strings.TrimPrefix(e.AgentVersion, "v"), "_")
}

// tartBin is the tart the VM steps run, chosen as the tart step's check chooses: the state
// dir's when it is the pin, else PATH's.
func (e *Env) tartBin(ctx context.Context) string {
	s := tartStep{e}
	if fileExists(s.bin()) {
		if v, err := s.version(ctx, s.bin()); err == nil && v == e.Pins.TartVersion {
			return s.bin()
		}
	}
	if p, err := e.LookPath("tart"); err == nil {
		return p
	}
	return "tart"
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

type tartVM struct {
	Name, State, Source string
}

// vms is tart list's local VMs by name.
func (e *Env) vms(ctx context.Context, tart string) (map[string]tartVM, error) {
	out, err := e.output(ctx, tart, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var list []tartVM
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	m := map[string]tartVM{}
	for _, v := range list {
		if v.Source == "local" {
			m[v.Name] = v
		}
	}
	return m, nil
}

// vmStatus is the state of a built VM: ok when it exists and is stopped.
func (e *Env) vmStatus(ctx context.Context, name, okDetail string) (Status, error) {
	vms, err := e.vms(ctx, e.tartBin(ctx))
	if err != nil {
		return Status{}, err
	}
	v, ok := vms[name]
	switch {
	case !ok:
		return Status{State: StateMissing, Detail: "no VM " + name + " in tart list"}, nil
	case v.State != "stopped":
		return Status{State: StateWrong, Detail: fmt.Sprintf("%s is %s: stop it (tart stop %s)", name, v.State, name)}, nil
	}
	return Status{State: StateOK, Detail: okDetail}, nil
}

func readRecord(p string, v any) (bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", p, err)
	}
	return true, nil
}

// writeRecord writes v as JSON, 0600, through a temp file and a rename.
func writeRecord(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (e *Env) stateDir() stateDirStep { return stateDirStep{e} }
func (e *Env) baseVM() baseVMStep     { return baseVMStep{e} }

// baseVMStep builds the base VM from the pinned image, the provisioning script and the
// linux factory-agent.
type baseVMStep struct{ e *Env }

func (baseVMStep) Name() string { return baseVMName }

func (s baseVMStep) record() string { return filepath.Join(s.e.StateDir(), "basevm.json") }

// want is the record this binary would write, without builtAt.
func (s baseVMStep) want() (BaseVMRecord, error) {
	sum, err := fileSHA256(s.e.AgentLinux)
	if err != nil {
		return BaseVMRecord{}, err
	}
	return BaseVMRecord{Name: s.e.BaseVMName(), Source: s.e.Pins.BaseImage, ScriptSHA256: ProvisionScriptSHA256(), AgentSHA256: sum, AgentVersion: s.e.AgentVersion}, nil
}

// agentMissing is the linux binary's problem, or "" when it's there.
func (s baseVMStep) agentMissing() string {
	if s.e.AgentLinux == "" || !fileExists(s.e.AgentLinux) {
		return fmt.Sprintf("no linux/arm64 factory-agent at %q: build it with make tart-up, or pass --agent-linux", s.e.AgentLinux)
	}
	return ""
}

func (s baseVMStep) Check(ctx context.Context) (Status, error) {
	if p := s.agentMissing(); p != "" {
		return Status{State: StateWrong, Detail: p}, nil
	}
	want, err := s.want()
	if err != nil {
		return Status{}, err
	}
	var got BaseVMRecord
	if ok, err := readRecord(s.record(), &got); err != nil || !ok {
		return Status{State: StateMissing, Detail: "no " + s.record()}, err
	}
	for _, f := range []struct{ field, got, want string }{
		{"name", got.Name, want.Name},
		{"source", got.Source, want.Source},
		{"scriptSHA256", got.ScriptSHA256, want.ScriptSHA256},
		{"agentSHA256", got.AgentSHA256, want.AgentSHA256},
	} {
		if f.got != f.want {
			return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: the base's %s is %s, want %s", want.Name, f.field, f.got, f.want)}, nil
		}
	}
	return s.e.vmStatus(ctx, want.Name, fmt.Sprintf("%s (built %s)", want.Name, got.BuiltAt))
}

func (s baseVMStep) Plan(context.Context) (Plan, error) {
	e, name := s.e, s.e.BaseVMName()
	b, sz := name+buildingSuffix, e.Workspace.Size
	return Plan{Do: []string{
		"tart clone " + e.Pins.BaseImage + " " + b,
		fmt.Sprintf("tart set %s --cpu %d --memory %d --disk-size %d", b, sz.CPUs, sz.MemoryGiB()*1024, sz.DiskGiB()),
		fmt.Sprintf("tart run %s --no-graphics (in the background); wait for tart exec (%s)", b, e.VM.Boot),
		fmt.Sprintf("tart exec -i %s bash -s < the embedded provisioning script (%s)", b, e.VM.Provision),
		"copy " + e.AgentLinux + " to " + agentInVM + " in the VM and check its sha256",
		fmt.Sprintf("shut it down (%s), tart rename %s %s (replacing an older %s), write %s", e.VM.Shutdown, b, name, name, s.record()),
	}}, nil
}

func (s baseVMStep) Apply(ctx context.Context) error {
	if p := s.agentMissing(); p != "" {
		return errors.New(p)
	}
	// the record goes there: make sure of it before a 30-minute build (--only basevm).
	if err := s.e.stateDir().Apply(ctx); err != nil {
		return err
	}
	rec, err := s.want()
	if err != nil {
		return err
	}
	sz := s.e.Workspace.Size
	set := []string{"--cpu", strconv.Itoa(sz.CPUs), "--memory", strconv.Itoa(sz.MemoryGiB() * 1024), "--disk-size", strconv.Itoa(sz.DiskGiB())}
	err = s.e.buildVM(ctx, rec.Source, rec.Name, set, func(ctx context.Context, b *vmBuild) error {
		pctx, cancel := context.WithTimeout(ctx, s.e.VM.Provision)
		defer cancel()
		fmt.Fprintf(b.out, "running the provisioning script in %s\n", b.vm)
		if err := b.e.Spawner.RunInput(pctx, bytes.NewReader(provisionScript), b.out, b.bin, "exec", "-i", b.vm, "bash", "-s"); err != nil {
			return fmt.Errorf("the provisioning script: %w", err)
		}
		return b.copyAgent(ctx, rec.AgentSHA256)
	})
	if err != nil {
		return err
	}
	rec.BuiltAt = s.e.Clock.Now().UTC().Format(time.RFC3339)
	return writeRecord(s.record(), rec)
}

// warmBaseStep clones the base into the warm base and fills its Docker cache with
// workspace.warm's images.
type warmBaseStep struct{ e *Env }

func (warmBaseStep) Name() string { return warmBaseName }

func (s warmBaseStep) record() string { return filepath.Join(s.e.StateDir(), "warmbase.json") }

// imagesSHA256 is the sha256 of the warm lists: "image <ref>" and "kind <ref>" lines, sorted
// and newline-joined.
func (s warmBaseStep) imagesSHA256() string {
	var lines []string
	for _, im := range s.e.Workspace.Warm.Images {
		lines = append(lines, "image "+im)
	}
	for _, im := range s.e.Workspace.Warm.KindImages {
		lines = append(lines, "kind "+im)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func (s warmBaseStep) Check(ctx context.Context) (Status, error) {
	var got WarmBaseRecord
	if ok, err := readRecord(s.record(), &got); err != nil || !ok {
		return Status{State: StateMissing, Detail: "no " + s.record()}, err
	}
	base, err := s.e.baseVM().Check(ctx)
	if err != nil {
		return Status{}, err
	}
	if base.State != StateOK {
		return Status{State: StateOutdated, Detail: "the base VM is " + string(base.State) + ": the warm base follows it"}, nil
	}
	var b BaseVMRecord
	if _, err := readRecord(s.e.baseVM().record(), &b); err != nil {
		return Status{}, err
	}
	for _, f := range []struct{ field, got, want string }{
		{"base", got.Base, b.Name},
		{"baseBuiltAt", got.BaseBuiltAt, b.BuiltAt},
		{"imagesSHA256", got.ImagesSHA256, s.imagesSHA256()},
	} {
		if f.got != f.want {
			return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: its %s is %s, want %s", WarmBaseVM, f.field, f.got, f.want)}, nil
		}
	}
	w := s.e.Workspace.Warm
	return s.e.vmStatus(ctx, WarmBaseVM, fmt.Sprintf("%s from %s, %d images, %d kind images", WarmBaseVM, b.Name, len(w.Images), len(w.KindImages)))
}

// kindArchive is a kind image's archive in the VM: the image with [^A-Za-z0-9._-] mapped to _.
func kindArchive(image string) string {
	return KindImagesDir + "/" + nameUnsafeRE.ReplaceAllString(image, "_") + ".tar"
}

// archives maps each kind image to its archive; two images with one archive are an error.
func (s warmBaseStep) archives() ([]string, error) {
	seen := map[string]string{}
	var out []string
	for _, im := range s.e.Workspace.Warm.KindImages {
		a := kindArchive(im)
		if prev, ok := seen[a]; ok && prev != im {
			return nil, fmt.Errorf("warm.kindImages %q and %q both save to %s", prev, im, a)
		}
		seen[a] = im
		out = append(out, a)
	}
	return out, nil
}

func (s warmBaseStep) Plan(context.Context) (Plan, error) {
	e := s.e
	b, w := WarmBaseVM+buildingSuffix, e.Workspace.Warm
	if _, err := s.archives(); err != nil {
		return Plan{}, err
	}
	do := []string{"tart clone " + e.BaseVMName() + " " + b}
	if len(w.Images)+len(w.KindImages) == 0 {
		do = append(do, "no warm images: the warm base is a plain clone of the base")
	} else {
		do = append(do, fmt.Sprintf("tart run %s --no-graphics (in the background); wait for tart exec (%s)", b, e.VM.Boot))
		for _, im := range w.Images {
			do = append(do, "tart exec "+b+" sudo docker pull "+im)
		}
		for _, im := range w.KindImages {
			do = append(do, "tart exec "+b+" sudo docker pull "+im+", then docker save -o "+kindArchive(im))
		}
		do = append(do, fmt.Sprintf("clear its machine-id and cloud-init state, shut it down (%s)", e.VM.Shutdown))
	}
	do = append(do, fmt.Sprintf("tart rename %s %s (replacing an older one), write %s", b, WarmBaseVM, s.record()))
	return Plan{Do: do}, nil
}

func (s warmBaseStep) Apply(ctx context.Context) error {
	// init applies basevm first; with --only warmbase a stale base is refused, not cloned.
	st, err := s.e.baseVM().Check(ctx)
	if err != nil {
		return err
	}
	if st.State != StateOK {
		return fmt.Errorf("the base VM is %s (%s): build it first (--only basevm,warmbase)", st.State, st.Detail)
	}
	var base BaseVMRecord
	if _, err := readRecord(s.e.baseVM().record(), &base); err != nil {
		return err
	}
	archives, err := s.archives()
	if err != nil {
		return err
	}
	w := s.e.Workspace.Warm
	var pull func(context.Context, *vmBuild) error
	if len(w.Images)+len(w.KindImages) > 0 {
		pull = func(ctx context.Context, b *vmBuild) error {
			pctx, cancel := context.WithTimeout(ctx, s.e.VM.Provision)
			defer cancel()
			for _, im := range w.Images {
				if err := b.exec(pctx, "sudo", "docker", "pull", im); err != nil {
					return err
				}
			}
			for i, im := range w.KindImages {
				if err := b.exec(pctx, "sudo", "docker", "pull", im); err != nil {
					return err
				}
				if err := b.exec(pctx, "sudo", "docker", "save", "-o", archives[i], im); err != nil {
					return err
				}
			}
			// booting gave the clone a machine-id and cloud-init's state: clear them again, so
			// the workspaces cloned from it get their own (as the base's script does).
			if err := b.exec(pctx, "sudo", "cloud-init", "clean", "--logs"); err != nil {
				return err
			}
			return b.exec(pctx, "sudo", "truncate", "-s", "0", "/etc/machine-id")
		}
	}
	if err := s.e.buildVM(ctx, base.Name, WarmBaseVM, nil, pull); err != nil {
		return err
	}
	return writeRecord(s.record(), WarmBaseRecord{
		Base: base.Name, BaseBuiltAt: base.BuiltAt, ImagesSHA256: s.imagesSHA256(),
		BuiltAt: s.e.Clock.Now().UTC().Format(time.RFC3339),
	})
}

// vmBuild is a VM being built: vm is its .building name, bin the tart that builds it.
type vmBuild struct {
	e       *Env
	vm, bin string
	out     io.Writer
	proc    Process
}

// lockedWriter serialises writes: tart run's output arrives from another goroutine.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// buildVM makes final from src: it clones src into final.building, runs tart set with set
// (when given) and, when provision is set, boots the VM, runs provision, and shuts it down.
// Then it renames final.building to final, replacing an older final. Any failure deletes
// final.building, unless KeepFailed.
func (e *Env) buildVM(ctx context.Context, src, final string, set []string, provision func(context.Context, *vmBuild) error) (err error) {
	b := &vmBuild{e: e, vm: final + buildingSuffix, bin: e.tartBin(ctx), out: &lockedWriter{w: e.Out}}
	vms, err := e.vms(ctx, b.bin)
	if err != nil {
		return err
	}
	if v, ok := vms[b.vm]; ok {
		// an earlier build's leftover: it failed with --keep-failed, or factory-agent was
		// killed, and then its tart run (in its own process group) may still run it.
		fmt.Fprintf(b.out, "deleting the leftover %s\n", b.vm)
		if v.State != "stopped" {
			if err := b.tart(ctx, "stop", b.vm); err != nil {
				return err
			}
		}
		if err := b.tart(ctx, "delete", b.vm); err != nil {
			return err
		}
	}
	fmt.Fprintf(b.out, "cloning %s into %s\n", src, b.vm)
	if err := b.tart(ctx, "clone", src, b.vm); err != nil {
		return err
	}
	defer func() {
		if b.proc != nil {
			b.proc.Stop()
		}
		if err == nil {
			return
		}
		if e.KeepFailed {
			fmt.Fprintf(b.out, "--keep-failed: kept %s for debugging; tart delete %s when done\n", b.vm, b.vm)
			return
		}
		// cleanup runs after an interrupt too, so it gets its own context.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		fmt.Fprintf(b.out, "the build failed: deleting %s (--keep-failed keeps it)\n", b.vm)
		if derr := b.tart(cctx, "delete", b.vm); derr != nil {
			err = errors.Join(err, fmt.Errorf("deleting %s: %w", b.vm, derr))
		}
	}()
	if len(set) > 0 {
		if err := b.tart(ctx, append([]string{"set", b.vm}, set...)...); err != nil {
			return err
		}
	}
	if provision != nil {
		if err := b.boot(ctx); err != nil {
			return err
		}
		if err := provision(ctx, b); err != nil {
			return err
		}
		if err := b.shutdown(ctx); err != nil {
			return err
		}
	}
	if vms, err = e.vms(ctx, b.bin); err != nil {
		return err
	}
	if _, ok := vms[final]; ok {
		fmt.Fprintf(b.out, "deleting the older %s\n", final)
		if err := b.tart(ctx, "delete", final); err != nil {
			return err
		}
	}
	return b.tart(ctx, "rename", b.vm, final)
}

// tart runs tart with args, its output to the build's.
func (b *vmBuild) tart(ctx context.Context, args ...string) error {
	return b.e.Runner.Run(ctx, b.out, b.bin, args...)
}

// exec runs a command in the VM.
func (b *vmBuild) exec(ctx context.Context, args ...string) error {
	if err := b.tart(ctx, append([]string{"exec", b.vm}, args...)...); err != nil {
		return fmt.Errorf("in %s: %s: %w", b.vm, strings.Join(args, " "), err)
	}
	return nil
}

// boot starts tart run in the background and waits until tart exec works; tart run exiting
// first fails it.
func (b *vmBuild) boot(ctx context.Context) error {
	fmt.Fprintf(b.out, "booting %s\n", b.vm)
	p, err := b.e.Spawner.Start(ctx, b.out, b.bin, "run", b.vm, "--no-graphics")
	if err != nil {
		return err
	}
	b.proc = p
	ok, exited, err := b.poll(ctx, b.e.VM.Boot, func(ctx context.Context) bool {
		return b.e.Runner.Run(ctx, io.Discard, b.bin, "exec", b.vm, "true") == nil
	})
	switch {
	case err != nil:
		return err
	case exited:
		why := b.proc.Err()
		if why == nil {
			why = errors.New("exit status 0")
		}
		return fmt.Errorf("%s: tart run exited before its guest agent answered: %w", b.vm, why)
	case !ok:
		return fmt.Errorf("%s: timed out after %s waiting for its guest agent", b.vm, b.e.VM.Boot)
	}
	return nil
}

// poll checks cond every VM.Poll until it's true, timeout passes (ok false) or tart run
// exits (exited). err is set only when ctx is done.
func (b *vmBuild) poll(ctx context.Context, timeout time.Duration, cond func(context.Context) bool) (ok, exited bool, err error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if cond(wctx) {
			return true, false, nil
		}
		select {
		case <-wctx.Done():
			return false, false, ctx.Err()
		case <-b.proc.Exited():
			return false, true, nil
		case <-time.After(b.e.VM.Poll):
		}
	}
}

// shutdown shuts the VM down and waits until tart list shows it stopped (tart run exiting
// means it has); after VM.Shutdown it runs tart stop. tart run is stopped in any case.
func (b *vmBuild) shutdown(ctx context.Context) error {
	fmt.Fprintf(b.out, "shutting %s down\n", b.vm)
	// the VM goes away under the command, so its error says nothing.
	_ = b.tart(ctx, "exec", b.vm, "sudo", "shutdown", "-h", "now")
	ok, _, err := b.poll(ctx, b.e.VM.Shutdown, func(ctx context.Context) bool {
		vms, err := b.e.vms(ctx, b.bin)
		return err == nil && vms[b.vm].State == "stopped"
	})
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(b.out, "%s didn't stop within %s: tart stop\n", b.vm, b.e.VM.Shutdown)
		if err := b.tart(ctx, "stop", b.vm); err != nil {
			return err
		}
	}
	b.proc.Stop()
	return nil
}

// copyAgent copies the linux factory-agent into the VM with a tar stream, then checks its
// sha256 there.
func (b *vmBuild) copyAgent(ctx context.Context, want string) error {
	f, err := os.Open(b.e.AgentLinux)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: filepath.Base(agentInVM), Mode: 0o755, Size: fi.Size(), Uname: "root", Gname: "root", ModTime: fi.ModTime()})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	fmt.Fprintf(b.out, "copying %s into %s\n", b.e.AgentLinux, b.vm)
	err = b.e.Spawner.RunInput(ctx, pr, b.out, b.bin, "exec", "-i", b.vm, "sudo", "tar", "-xf", "-", "-C", filepath.Dir(agentInVM))
	_ = pr.CloseWithError(errors.New("tar exited"))
	if err != nil {
		return fmt.Errorf("copying factory-agent: %w", err)
	}
	out, err := b.e.output(ctx, b.bin, "exec", b.vm, "sha256sum", agentInVM)
	if err != nil {
		return fmt.Errorf("in %s: sha256sum: %w", b.vm, err)
	}
	if got, _, _ := strings.Cut(out, " "); got != want {
		return fmt.Errorf("%s in %s has sha256 %s, want the host file's %s", agentInVM, b.vm, got, want)
	}
	return nil
}
