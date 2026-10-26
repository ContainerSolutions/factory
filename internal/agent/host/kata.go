package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// KataSlotPool is the kata driver's workspace addresses: slot n (1 to 63) is 10.213.<n>.0/29,
// its bridge at .1 and its VM at .2 (workspace-kata.md#the-workspace-network). The platform
// step refuses a host that already routes any of it.
var KataSlotPool = netip.MustParsePrefix("10.213.0.0/16")

const (
	// kataMinFreeGiB is the free disk /var/lib's volume needs: containerd's state, the image,
	// the warm disk and the workspaces' disks.
	kataMinFreeGiB = 120
	// kataVarLib is the volume the kata driver's state is on.
	kataVarLib = "/var/lib"
)

// kataTools are the programs the kata steps run.
var kataTools = []string{"nft", "systemctl", "visudo"}

// kataPlatformStep checks the Linux host (workspace-kata.md, step 1): linux/amd64, KVM,
// cgroup v2, systemd, the tools, free disk and memory, and the slot pool unrouted. It has
// nothing to apply: a failed check stops Init.
type kataPlatformStep struct{ e *Env }

func (kataPlatformStep) Name() string                       { return platformName }
func (kataPlatformStep) Plan(context.Context) (Plan, error) { return Plan{}, nil }
func (kataPlatformStep) Apply(context.Context) error {
	return errors.New("the platform can't be fixed by host init")
}

func (s kataPlatformStep) Check(ctx context.Context) (Status, error) {
	e := s.e
	switch {
	case e.GOOS == "linux" && e.GOARCH == "arm64":
		return Status{State: StateWrong, Detail: "the kata driver needs linux/amd64: Kata's arm64 release has no runtime-rs Cloud Hypervisor config"}, nil
	case e.GOOS != "linux" || e.GOARCH != "amd64":
		return Status{State: StateWrong, Detail: fmt.Sprintf("the kata driver needs linux/amd64, this is %s/%s", e.GOOS, e.GOARCH)}, nil
	}
	var probs []string
	switch fi, err := os.Stat(e.sys("/dev/kvm")); {
	case errors.Is(err, fs.ErrNotExist):
		probs = append(probs, "no /dev/kvm: enable virtualization in the firmware and load kvm_intel or kvm_amd")
	case err != nil:
		return Status{}, err
	case fi.Mode()&fs.ModeCharDevice == 0:
		probs = append(probs, "/dev/kvm is not a character device")
	}
	virt, err := cpuVirt(e.sys("/proc/cpuinfo"))
	if err != nil {
		return Status{}, err
	}
	if !virt {
		probs = append(probs, "no vmx or svm flag in /proc/cpuinfo: the CPU has no hardware virtualization")
	}
	if _, err := os.Stat(e.sys("/sys/fs/cgroup/cgroup.controllers")); errors.Is(err, fs.ErrNotExist) {
		probs = append(probs, "no /sys/fs/cgroup/cgroup.controllers: the kata driver needs cgroup v2")
	} else if err != nil {
		return Status{}, err
	}
	if fi, err := os.Stat(e.sys("/run/systemd/system")); errors.Is(err, fs.ErrNotExist) || err == nil && !fi.IsDir() {
		probs = append(probs, "no /run/systemd/system: the kata driver needs systemd as PID 1")
	} else if err != nil {
		return Status{}, err
	}
	var missing []string
	for _, t := range kataTools {
		if _, err := e.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		probs = append(probs, "not on PATH: "+strings.Join(missing, ", "))
	}
	free, err := freeBytes(ctx, e, e.sys(kataVarLib))
	if err != nil {
		return Status{}, err
	}
	if free < kataMinFreeGiB<<30 {
		probs = append(probs, fmt.Sprintf("%d GiB free on %s's volume, want %d GiB", free>>30, kataVarLib, kataMinFreeGiB))
	}
	mem, err := memTotal(e.sys("/proc/meminfo"))
	if err != nil {
		return Status{}, err
	}
	if want := uint64(e.Workspace.Size.MemoryGiB()+headroomGiB) << 30; mem < want {
		probs = append(probs, fmt.Sprintf("%d GiB memory: a %s workspace needs %d GiB", mem>>30, e.Workspace.Size.Memory, want>>30))
	}
	routes, err := e.output(ctx, "ip", "-4", "route", "show")
	if err != nil {
		return Status{}, err
	}
	if in := routesIn(routes, KataSlotPool); len(in) > 0 {
		probs = append(probs, fmt.Sprintf("routes already in the slot pool %s: %s", KataSlotPool, strings.Join(in, ", ")))
	}
	if len(probs) > 0 {
		return Status{State: StateWrong, Detail: strings.Join(probs, "; ")}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("linux/amd64, KVM, cgroup v2, %d GiB memory, %d GiB free on %s", mem>>30, free>>30, kataVarLib)}, nil
}

// cpuVirt reports whether a flags line of the cpuinfo at p has vmx (Intel VT-x) or svm (AMD-V).
func cpuVirt(p string) (bool, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return false, fmt.Errorf("the CPU's flags: %w", err)
	}
	for l := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(l, ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}
		for f := range strings.FieldsSeq(v) {
			if f == "vmx" || f == "svm" {
				return true, nil
			}
		}
	}
	return false, nil
}

// memTotal is the physical memory in bytes, from the meminfo at p's MemTotal line (in kB).
func memTotal(p string) (uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, fmt.Errorf("the physical memory: %w", err)
	}
	for l := range strings.Lines(string(b)) {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != "MemTotal:" {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || len(f) > 2 && f[2] != "kB" {
			return 0, fmt.Errorf("%s: unexpected MemTotal line %q", p, strings.TrimSpace(l))
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("%s has no MemTotal line", p)
}

// ipRouteTypes are the route types ip route prints before a route's destination.
var ipRouteTypes = map[string]bool{
	"unicast": true, "local": true, "broadcast": true, "multicast": true, "throw": true,
	"unreachable": true, "prohibit": true, "blackhole": true, "nat": true, "anycast": true,
}

// routesIn returns the destinations among ip -4 route show's lines that overlap pool. A line's
// destination is its first field, after a route type; default and other lines whose
// destination isn't an address or a prefix (multipath's nexthop lines) are never in it.
func routesIn(out string, pool netip.Prefix) []string {
	var in []string
	for l := range strings.Lines(out) {
		f := strings.Fields(l)
		if len(f) > 1 && ipRouteTypes[f[0]] {
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		dst, err := netip.ParsePrefix(f[0])
		if err != nil {
			a, aerr := netip.ParseAddr(f[0])
			if aerr != nil {
				continue
			}
			dst = netip.PrefixFrom(a, a.BitLen())
		}
		if dst.Overlaps(pool) {
			in = append(in, f[0])
		}
	}
	return in
}
