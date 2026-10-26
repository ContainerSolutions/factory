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

	"github.com/compliance-framework/factory/internal/agent/config"
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
	// oldSuffix marks the VM a build replaces, renamed aside until the new one is in place.
	oldSuffix = ".old"
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
	// AgentSHA256 is the linux factory-agent's sha256 in the warm base: the base's when it
	// was cloned, then each binary the warm base got copied in.
	AgentSHA256 string `json:"agentSHA256,omitempty"`
	BuiltAt     string `json:"builtAt"`
}

var nameUnsafeRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// BaseVMName is the base VM's name for this factory-agent: factory-agent-base-0.1.0, or
// factory-agent-base-dev for a dev build.
func (e *Env) BaseVMName() string {
	return BaseVMPrefix + nameUnsafeRE.ReplaceAllString(strings.TrimPrefix(e.AgentVersion, "v"), "_")
}

// TartBin is the tart the VM steps run, which the tart workspace driver runs too.
func (e *Env) TartBin(ctx context.Context) string { return e.tartBin(ctx) }

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
	if f, g, w := rebuildField(got, want); f != "" {
		return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: the base's %s is %s, want %s: host init rebuilds it", want.Name, f, g, w)}, nil
	}
	st, err := s.e.vmStatus(ctx, want.Name, fmt.Sprintf("%s (built %s)", want.Name, got.BuiltAt))
	if err != nil || st.State != StateOK || got.AgentSHA256 == want.AgentSHA256 {
		return st, err
	}
	return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: the base's agentSHA256 is %s, want %s: %s", want.Name, got.AgentSHA256, want.AgentSHA256, agentOnlyFix)}, nil
}

// agentOnlyFix is what host init does to a VM that differs from its record only in the agent
// binary.
const agentOnlyFix = "only the agent binary differs, so host init copies it in (no rebuild)"

// rebuildField is the first of the fields a build depends on (name, source, scriptSHA256)
// that differs between got and want, with both values; "" when none does.
func rebuildField(got, want BaseVMRecord) (field, g, w string) {
	for _, f := range []struct{ field, got, want string }{
		{"name", got.Name, want.Name},
		{"source", got.Source, want.Source},
		{"scriptSHA256", got.ScriptSHA256, want.ScriptSHA256},
	} {
		if f.got != f.want {
			return f.field, f.got, f.want
		}
	}
	return "", "", ""
}

// agentOnly reports whether the base is current but for its agent binary: basevm.json has this
// binary's name, source and script but another agentSHA256, and the VM is there, stopped. Host
// init then copies the binary in (updateAgent) instead of rebuilding the base. got is the
// record. A missing or unreadable record, or a missing linux binary, is not agentOnly.
func (s baseVMStep) agentOnly(ctx context.Context) (got BaseVMRecord, ok bool, err error) {
	if s.agentMissing() != "" {
		return got, false, nil
	}
	if found, err := readRecord(s.record(), &got); err != nil || !found {
		return got, false, nil
	}
	want, err := s.want()
	if err != nil {
		return got, false, err
	}
	if f, _, _ := rebuildField(got, want); f != "" || got.AgentSHA256 == want.AgentSHA256 {
		return got, false, nil
	}
	st, err := s.e.vmStatus(ctx, want.Name, "")
	if err != nil {
		return got, false, err
	}
	return got, st.State == StateOK, nil
}

// agentPlan is the plan of updateAgent on vm, whose record is rec.
func (e *Env) agentPlan(vm, rec string) []string {
	b := vm + buildingSuffix
	return []string{
		fmt.Sprintf("tart clone %s %s: only the agent binary differs from %s, so %s isn't rebuilt", vm, b, filepath.Base(rec), vm),
		fmt.Sprintf("tart run %s --no-graphics (in the background); wait for tart exec (%s)", b, e.VM.Boot),
		"copy " + e.AgentLinux + " to " + agentInVM + " in the VM and check its sha256",
		fmt.Sprintf("clear its machine-id and cloud-init state, shut it down (%s)", e.VM.Shutdown),
		fmt.Sprintf("tart rename %s %s (the older one is renamed aside to %s%s, then deleted), update agentSHA256 in %s", b, vm, vm, oldSuffix, rec),
	}
}

// updateAgent copies the linux factory-agent into vm without rebuilding it: buildVM clones vm
// into vm.building, boots the clone, copies the binary in and checks its sha256 (want), clears
// the machine-id and cloud-init state the boot gave it, shuts it down and renames it into
// place. vm itself is never booted, and a failure leaves it as it was.
func (e *Env) updateAgent(ctx context.Context, vm, want string) error {
	fmt.Fprintf(e.Out, "%s: copying the new agent binary in (no rebuild)\n", vm)
	return e.buildVM(ctx, vm, vm, nil, func(ctx context.Context, b *vmBuild) error {
		pctx, cancel := context.WithTimeout(ctx, e.VM.Provision)
		defer cancel()
		if err := b.copyAgent(pctx, want); err != nil {
			return err
		}
		return b.clearIdentity(pctx)
	})
}

func (s baseVMStep) Plan(ctx context.Context) (Plan, error) {
	e, name := s.e, s.e.BaseVMName()
	if _, ok, err := s.agentOnly(ctx); err != nil || ok {
		return Plan{Do: e.agentPlan(name, s.record())}, err
	}
	b, sz := name+buildingSuffix, e.Workspace.Size
	do := []string{
		"tart clone " + e.Pins.BaseImage + " " + b,
		fmt.Sprintf("tart set %s --cpu %d --memory %d --disk-size %d", b, sz.CPUs, sz.MemoryGiB()*1024, sz.DiskGiB()),
		fmt.Sprintf("tart run %s --no-graphics (in the background); wait for tart exec (%s)", b, e.VM.Boot),
		fmt.Sprintf("tart exec -i %s bash -s < the embedded provisioning script (%s)", b, e.VM.Provision),
		"copy " + e.AgentLinux + " to " + agentInVM + " in the VM and check its sha256",
		fmt.Sprintf("shut it down (%s), tart rename %s %s (an older %s is renamed aside to %s%s, then deleted), write %s", e.VM.Shutdown, b, name, name, name, oldSuffix, s.record()),
	}
	if prev := s.previous(); prev != "" {
		do = append(do, "tart delete "+prev+" (the previous base, named in basevm.json), unless it's running")
	}
	return Plan{Do: do}, nil
}

// previous is the base VM the current basevm.json names when it's an earlier one of host
// init's (BaseVMPrefix, another name): a new version's build deletes it. "" otherwise, also
// for an unreadable record, which names nothing to delete.
func (s baseVMStep) previous() string {
	var rec BaseVMRecord
	if ok, err := readRecord(s.record(), &rec); err != nil || !ok {
		return ""
	}
	if rec.Name == s.e.BaseVMName() || !strings.HasPrefix(rec.Name, BaseVMPrefix) || strings.HasSuffix(rec.Name, buildingSuffix) || strings.HasSuffix(rec.Name, oldSuffix) {
		return ""
	}
	return rec.Name
}

// deletePrevious deletes the previous version's base, unless it's gone or running; a failure
// is a warning: the new base is built.
func (s baseVMStep) deletePrevious(ctx context.Context, prev string) {
	tart := s.e.tartBin(ctx)
	vms, err := s.e.vms(ctx, tart)
	if err != nil {
		fmt.Fprintf(s.e.Out, "warning: the previous base %s is left: %v\n", prev, err)
		return
	}
	v, ok := vms[prev]
	switch {
	case !ok:
		return
	case v.State != "stopped":
		fmt.Fprintf(s.e.Out, "warning: the previous base %s is %s: left in place (tart delete %s when it's stopped)\n", prev, v.State, prev)
		return
	}
	fmt.Fprintf(s.e.Out, "deleting the previous base %s\n", prev)
	if err := s.e.Runner.Run(ctx, s.e.Out, tart, "delete", prev); err != nil {
		fmt.Fprintf(s.e.Out, "warning: deleting the previous base %s: %v\n", prev, err)
	}
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
	got, only, err := s.agentOnly(ctx)
	if err != nil {
		return err
	}
	if only {
		if err := s.e.updateAgent(ctx, rec.Name, rec.AgentSHA256); err != nil {
			return err
		}
		got.AgentSHA256, got.AgentVersion = rec.AgentSHA256, rec.AgentVersion
		return writeRecord(s.record(), got)
	}
	prev := s.previous()
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
	if err := writeRecord(s.record(), rec); err != nil {
		return err
	}
	if prev != "" {
		s.deletePrevious(ctx, prev)
	}
	return nil
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
		d := "the base VM is " + string(base.State) + ": the warm base follows it"
		if _, ok, err := s.agentOnly(ctx); err != nil {
			return Status{}, err
		} else if ok {
			d += "; " + agentOnlyFix
		}
		return Status{State: StateOutdated, Detail: d}, nil
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
			return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: its %s is %s, want %s: host init rebuilds it", WarmBaseVM, f.field, f.got, f.want)}, nil
		}
	}
	w := s.e.Workspace.Warm
	st, err := s.e.vmStatus(ctx, WarmBaseVM, fmt.Sprintf("%s from %s, %d images, %d kind images", WarmBaseVM, b.Name, len(w.Images), len(w.KindImages)))
	if err != nil || st.State != StateOK || got.AgentSHA256 == b.AgentSHA256 {
		return st, err
	}
	fix := agentOnlyFix
	if len(w.Images)+len(w.KindImages) == 0 {
		fix = "host init clones the base again (no warm images)"
	}
	have := got.AgentSHA256
	if have == "" {
		have = "unrecorded"
	}
	return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: its agentSHA256 is %s, want the base's %s: %s", WarmBaseVM, have, b.AgentSHA256, fix)}, nil
}

// agentOnly reports whether the warm base is current but for its agent binary: warmbase.json
// names the base basevm.json describes, with its builtAt and these warm images, but another
// agentSHA256 than this linux binary's; the base is ok, or itself agentOnly (its update keeps
// builtAt); and the warm base VM is there, stopped. Host init then copies the binary in. A warm
// base without images is never agentOnly: cloning the base again is cheaper than a boot.
func (s warmBaseStep) agentOnly(ctx context.Context) (got WarmBaseRecord, ok bool, err error) {
	w := s.e.Workspace.Warm
	if len(w.Images)+len(w.KindImages) == 0 || s.e.baseVM().agentMissing() != "" {
		return got, false, nil
	}
	if found, err := readRecord(s.record(), &got); err != nil || !found {
		return got, false, nil
	}
	var b BaseVMRecord
	if found, err := readRecord(s.e.baseVM().record(), &b); err != nil || !found {
		return got, false, nil
	}
	want, err := s.e.baseVM().want()
	if err != nil {
		return got, false, err
	}
	if got.Base != b.Name || b.Name != want.Name || got.BaseBuiltAt != b.BuiltAt || got.ImagesSHA256 != s.imagesSHA256() || got.AgentSHA256 == want.AgentSHA256 {
		return got, false, nil
	}
	base, err := s.e.baseVM().Check(ctx)
	if err != nil {
		return got, false, err
	}
	if base.State != StateOK {
		if _, ok, err := s.e.baseVM().agentOnly(ctx); err != nil || !ok {
			return got, false, err
		}
	}
	st, err := s.e.vmStatus(ctx, WarmBaseVM, "")
	if err != nil {
		return got, false, err
	}
	return got, st.State == StateOK, nil
}

// kindArchive is a kind image's archive in the VM: its tag (config.KindImageTag) with
// [^A-Za-z0-9._-] mapped to _.
func kindArchive(tag string) string {
	return KindImagesDir + "/" + nameUnsafeRE.ReplaceAllString(tag, "_") + ".tar"
}

// kindSave is how a kind image is saved: pulled as ref, then saved by tag, the reference
// kind's nodes find it by. docker save of a ref with a digest writes an archive without a
// repo tag, so a digest-pinned ref is tagged first (docker tag ref tag).
type kindSave struct{ ref, tag, archive string }

// kindSaves maps each kind image to its save; two images with one tag or archive are an error.
func (s warmBaseStep) kindSaves() ([]kindSave, error) {
	seen := map[string]string{}
	var out []kindSave
	for _, im := range s.e.Workspace.Warm.KindImages {
		tag, ok := config.KindImageTag(im)
		if !ok {
			return nil, fmt.Errorf("warm.kindImages %q has a digest but no tag", im)
		}
		a := kindArchive(tag)
		if prev, ok := seen[a]; ok && prev != im {
			return nil, fmt.Errorf("warm.kindImages %q and %q both save to %s", prev, im, a)
		}
		seen[a] = im
		out = append(out, kindSave{ref: im, tag: tag, archive: a})
	}
	return out, nil
}

func (s warmBaseStep) Plan(ctx context.Context) (Plan, error) {
	e := s.e
	if _, ok, err := s.agentOnly(ctx); err != nil || ok {
		return Plan{Do: e.agentPlan(WarmBaseVM, s.record())}, err
	}
	b, w := WarmBaseVM+buildingSuffix, e.Workspace.Warm
	saves, err := s.kindSaves()
	if err != nil {
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
		for _, k := range saves {
			tag := ""
			if k.tag != k.ref {
				tag = " and docker tag it " + k.tag
			}
			do = append(do, "tart exec "+b+" sudo docker pull "+k.ref+tag+", then docker save -o "+k.archive+" "+k.tag)
		}
		do = append(do, fmt.Sprintf("clear its machine-id and cloud-init state, shut it down (%s)", e.VM.Shutdown))
	}
	do = append(do, fmt.Sprintf("tart rename %s %s (an older one is renamed aside to %s%s, then deleted), write %s", b, WarmBaseVM, WarmBaseVM, oldSuffix, s.record()))
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
	got, only, err := s.agentOnly(ctx)
	if err != nil {
		return err
	}
	if only {
		if err := s.e.updateAgent(ctx, WarmBaseVM, base.AgentSHA256); err != nil {
			return err
		}
		got.AgentSHA256 = base.AgentSHA256
		return writeRecord(s.record(), got)
	}
	saves, err := s.kindSaves()
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
			for _, k := range saves {
				if err := b.exec(pctx, "sudo", "docker", "pull", k.ref); err != nil {
					return err
				}
				if k.tag != k.ref {
					if err := b.exec(pctx, "sudo", "docker", "tag", k.ref, k.tag); err != nil {
						return err
					}
				}
				if err := b.exec(pctx, "sudo", "docker", "save", "-o", k.archive, k.tag); err != nil {
					return err
				}
			}
			return b.clearIdentity(pctx)
		}
	}
	if err := s.e.buildVM(ctx, base.Name, WarmBaseVM, nil, pull); err != nil {
		return err
	}
	return writeRecord(s.record(), WarmBaseRecord{
		Base: base.Name, BaseBuiltAt: base.BuiltAt, ImagesSHA256: s.imagesSHA256(), AgentSHA256: base.AgentSHA256,
		BuiltAt: s.e.Clock.Now().UTC().Format(time.RFC3339),
	})
}

// vmBuild is a VM being built: vm is its .building name, bin the tart that builds it.
type vmBuild struct {
	e       *Env
	vm, bin string
	out     io.Writer
	proc    Process
	// renamed is set once vm has become the final VM: nothing is left to clean up.
	renamed bool
}

// LockedWriter serialises writes to W: tart run's output arrives from its own goroutines (here
// and in the tart workspace driver, where every VM's goes to one log).
type LockedWriter struct {
	mu sync.Mutex
	W  io.Writer
}

func (l *LockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.W.Write(p)
}

// buildVM makes final from src: it clones src into final.building, runs tart set with set
// (when given) and, when provision is set, boots the VM, runs provision, and shuts it down.
// Then it renames final.building to final, replacing an older final (see replace). Any
// failure before that deletes final.building, unless KeepFailed.
func (e *Env) buildVM(ctx context.Context, src, final string, set []string, provision func(context.Context, *vmBuild) error) (err error) {
	b := &vmBuild{e: e, vm: final + buildingSuffix, bin: e.tartBin(ctx), out: &LockedWriter{W: e.Out}}
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
		if err == nil || b.renamed {
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
	return b.replace(ctx, final)
}

// replace renames the built VM to final. An older final is renamed aside to final.old first,
// and deleted only once the new one is in place; when the rename fails, final.old is renamed
// back, so a failed last step leaves the older VM working. A final.old left by an interrupted
// run is the older VM when final is missing, and is deleted when final is there.
func (b *vmBuild) replace(ctx context.Context, final string) error {
	old := final + oldSuffix
	vms, err := b.e.vms(ctx, b.bin)
	if err != nil {
		return err
	}
	_, haveFinal := vms[final]
	_, haveOld := vms[old]
	if haveOld && haveFinal {
		fmt.Fprintf(b.out, "deleting the leftover %s\n", old)
		if err := b.tart(ctx, "delete", old); err != nil {
			return err
		}
		haveOld = false
	}
	if haveFinal {
		fmt.Fprintf(b.out, "renaming the older %s aside to %s\n", final, old)
		if err := b.tart(ctx, "rename", final, old); err != nil {
			return err
		}
		haveOld = true
	}
	if err := b.tart(ctx, "rename", b.vm, final); err != nil {
		if haveOld {
			// cleanup runs after an interrupt too, so it gets its own context.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			fmt.Fprintf(b.out, "putting the older %s back\n", final)
			if rerr := b.tart(cctx, "rename", old, final); rerr != nil {
				return errors.Join(err, fmt.Errorf("putting back %s: %w", final, rerr))
			}
		}
		return err
	}
	b.renamed = true
	if haveOld {
		fmt.Fprintf(b.out, "deleting the older %s\n", old)
		// the new VM is in place: a failed delete leaves only the older one's disk, which the
		// next build deletes.
		if err := b.tart(ctx, "delete", old); err != nil {
			fmt.Fprintf(b.out, "warning: deleting %s: %v (tart delete %s)\n", old, err, old)
		}
	}
	return nil
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

// clearIdentity clears the machine-id and cloud-init state booting gave the VM, so the
// workspaces cloned from it get their own (as the base's script does).
func (b *vmBuild) clearIdentity(ctx context.Context) error {
	if err := b.exec(ctx, "sudo", "cloud-init", "clean", "--logs"); err != nil {
		return err
	}
	return b.exec(ctx, "sudo", "truncate", "-s", "0", "/etc/machine-id")
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
	ok, exited, err := b.poll(ctx, b.e.VM.Boot, b.proc.Exited(), func(ctx context.Context) bool {
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

// poll checks cond every VM.Poll until it's true, timeout passes (ok false) or exit is closed
// (exited; a nil exit is never closed). err is set only when ctx is done.
func (b *vmBuild) poll(ctx context.Context, timeout time.Duration, exit <-chan struct{}, cond func(context.Context) bool) (ok, exited bool, err error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if cond(wctx) {
			return true, false, nil
		}
		select {
		case <-wctx.Done():
			return false, false, ctx.Err()
		case <-exit:
			return false, true, nil
		case <-time.After(b.e.VM.Poll):
		}
	}
}

// shutdown shuts the VM down and waits until tart list shows it stopped. tart run exits once
// the guest has powered off, often before tart list says stopped: then it waits for tart list
// alone. After VM.Shutdown it runs tart stop, which fails on a VM that has stopped meanwhile
// ("is not running"): that is a stopped VM too. tart run is stopped in any case.
func (b *vmBuild) shutdown(ctx context.Context) error {
	fmt.Fprintf(b.out, "shutting %s down\n", b.vm)
	// the VM goes away under the command, so its error says nothing.
	_ = b.tart(ctx, "exec", b.vm, "sudo", "shutdown", "-h", "now")
	stopped := func(ctx context.Context) bool {
		vms, err := b.e.vms(ctx, b.bin)
		return err == nil && vms[b.vm].State == "stopped"
	}
	ok, exited, err := b.poll(ctx, b.e.VM.Shutdown, b.proc.Exited(), stopped)
	if err == nil && exited {
		fmt.Fprintf(b.out, "tart run exited: waiting for tart list to show %s stopped\n", b.vm)
		ok, _, err = b.poll(ctx, b.e.VM.Shutdown, nil, stopped)
	}
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(b.out, "%s didn't stop within %s: tart stop\n", b.vm, b.e.VM.Shutdown)
		var msg bytes.Buffer
		if err := b.e.Runner.Run(ctx, io.MultiWriter(b.out, &msg), b.bin, "stop", b.vm); err != nil && !strings.Contains(msg.String(), "is not running") {
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
