package katahelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/workspace"
)

// The sizes create accepts: the config's bounds (config.Workspace's size.cpus, size.memory,
// size.disk).
const (
	minCPUs, maxCPUs       = 1, 16
	minMemMiB, maxMemMiB   = 2 << 10, 64 << 10
	minDiskGiB, maxDiskGiB = 20, 500
)

// imageRE is the workspace image's reference: ImageRepo and factory-agent's version as its tag.
var imageRE = regexp.MustCompile(`^` + ImageRepo + `:[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Info is create's and inspect's output: the workspace's slot and addresses; inspect adds
// whether it runs.
type Info struct {
	Slot    int        `json:"slot"`
	VM      netip.Addr `json:"vm"`
	Gateway netip.Addr `json:"gateway"`
	Running *bool      `json:"running,omitempty"`
}

// slotInfo is slot n's addresses.
func slotInfo(n int) Info {
	p := SlotPool.Addr().As4()
	return Info{Slot: n, VM: netip.AddrFrom4([4]byte{p[0], p[1], byte(n), 2}), Gateway: netip.AddrFrom4([4]byte{p[0], p[1], byte(n), 1})}
}

// Slot n's bridge, CNI network, slot file and conflist, and a workspace's disk.
func bridge(n int) string     { return BridgePrefix + strconv.Itoa(n) }
func network(n int) string    { return "ws-" + strconv.Itoa(n) }
func slotFile(n int) string   { return SlotsDir + "/" + strconv.Itoa(n) }
func conflist(n int) string   { return CNIConfDir + "/" + network(n) + ".conflist" }
func disk(name string) string { return DisksDir + "/" + name + ".img" }

// nftElement is slot n's ws_allow element: its bridge, its VM's address, its gateway's.
func nftElement(n int) string {
	i := slotInfo(n)
	return fmt.Sprintf(`{ "%s" . %s . %s }`, bridge(n), i.VM, i.Gateway)
}

// createArgs are create's validated arguments.
type createArgs struct {
	name            string
	cpus, mem, disk int
	warm            bool
	image           string
}

// parseCreate validates <name> <cpus> <memMiB> <diskGiB> warm|empty <image>.
func parseCreate(a []string) (createArgs, error) {
	c := createArgs{name: a[0], image: a[5]}
	if err := workspace.ValidName(c.name); err != nil {
		return c, badArg("%v", err)
	}
	var err error
	if c.cpus, err = intIn(a[1], "cpus", minCPUs, maxCPUs); err != nil {
		return c, err
	}
	if c.mem, err = intIn(a[2], "memMiB", minMemMiB, maxMemMiB); err != nil {
		return c, err
	}
	if c.disk, err = intIn(a[3], "diskGiB", minDiskGiB, maxDiskGiB); err != nil {
		return c, err
	}
	switch a[4] {
	case "warm":
		c.warm = true
	case "empty":
	default:
		return c, badArg("%q is neither warm nor empty", a[4])
	}
	return c, validImage(c.image)
}

// intIn parses a plain decimal from lo to hi: no sign, no leading zero.
func intIn(s, what string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || strconv.Itoa(n) != s || n < lo || n > hi {
		return 0, badArg("%s %q isn't a number from %d to %d", what, s, lo, hi)
	}
	return n, nil
}

// create makes workspace a.name: its slot, bridge, conflist, nft element and disk, then the
// container. A failure undoes what it made.
func (h *helper) create(ctx context.Context, a createArgs) (err error) {
	// a disk under the name would be another workspace's: never overwritten, nor removed below.
	if ok, err := h.exists(disk(a.name)); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s already has a disk", a.name)
	}
	if ok, err := h.exists(WarmDisk); err != nil {
		return err
	} else if a.warm && !ok {
		return fmt.Errorf("no warm disk %s", WarmDisk)
	}
	n, err := h.allocate(ctx, a.name)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmdTimeout)
			defer cancel()
			if ferr := h.free(cctx, n, a.name); ferr != nil {
				err = errors.Join(err, fmt.Errorf("undoing: %w", ferr))
			}
		}
	}()
	info := slotInfo(n)
	for _, c := range [][]string{
		{IP, "link", "add", bridge(n), "type", "bridge"},
		{Sysctl, "-qw", "net.ipv6.conf." + bridge(n) + ".disable_ipv6=1"},
		{IP, "addr", "add", info.Gateway.String() + "/29", "dev", bridge(n)},
		{IP, "link", "set", bridge(n), "up"},
	} {
		if _, err := h.run(ctx, cmdTimeout, filepath.Base(c[0])+" "+c[1], c[0], c[1:]...); err != nil {
			return err
		}
	}
	if err := h.writeConflist(n); err != nil {
		return err
	}
	if _, err := h.run(ctx, cmdTimeout, "nft add element", NFT, "add", "element", "inet", NFTTable, NFTAllowSet, nftElement(n)); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(DisksDir), 0o700); err != nil {
		return err
	}
	if a.warm {
		_, err = h.run(ctx, cmdTimeout, "cp", CP, "--sparse=always", "--", WarmDisk, disk(a.name))
	} else {
		_, err = h.run(ctx, cmdTimeout, "truncate", Truncate, "-s", strconv.Itoa(a.disk)+"G", "--", disk(a.name))
	}
	if err != nil {
		return err
	}
	if err := h.nerdctl(ctx, nil, "create", "--name", a.name, "--hostname", a.name, "--runtime", Runtime,
		"--privileged", "--security-opt", "privileged-without-host-devices",
		"--cpus", strconv.Itoa(a.cpus), "--memory", strconv.Itoa(a.mem)+"m",
		"--network", network(n), "--dns", info.Gateway.String(),
		"-e", "container=docker", "--tmpfs", "/run", "--tmpfs", "/run/lock",
		"-v", disk(a.name)+":"+GuestDisk, a.image); err != nil {
		return err
	}
	return json.NewEncoder(h.o.Stdout).Encode(info)
}

// writeConflist writes slot n's CNI network: its bridge, which create made, and the VM's one
// address (workspace-kata.md#the-workspace-network).
func (h *helper) writeConflist(n int) error {
	i := slotInfo(n)
	sub := netip.PrefixFrom(i.Gateway, 29).Masked()
	type m = map[string]any
	b, err := json.MarshalIndent(m{"cniVersion": "1.0.0", "name": network(n), "plugins": []m{{
		"type": "bridge", "bridge": bridge(n), "isGateway": true, "ipMasq": false, "hairpinMode": false,
		"ipam": m{"type": "host-local", "dataDir": CNIStateDir,
			"ranges": [][]m{{{"subnet": sub.String(), "gateway": i.Gateway.String(), "rangeStart": i.VM.String(), "rangeEnd": i.VM.String()}}},
			"routes": []m{{"dst": "0.0.0.0/0"}}},
	}}}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(CNIConfDir), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.path(conflist(n)), append(b, '\n'), 0o644)
}

// slots reads the slot files: slot -> name.
func (h *helper) slots() (map[int]string, error) {
	m := map[int]string{}
	for n := 1; n <= Slots; n++ {
		b, err := os.ReadFile(h.path(slotFile(n)))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			m[n] = strings.TrimSpace(string(b))
		}
	}
	return m, nil
}

// slotOf is name's slot, or 0.
func (h *helper) slotOf(name string) (int, error) {
	m, err := h.slots()
	for n, s := range m {
		if s == name {
			return n, err
		}
	}
	return 0, err
}

// allocate takes the lowest free slot for name under the lock, refusing a name that has one.
func (h *helper) allocate(ctx context.Context, name string) (int, error) {
	unlock, err := h.lock(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	m, err := h.slots()
	if err != nil {
		return 0, err
	}
	for n := 1; n <= Slots; n++ {
		if m[n] == name {
			return 0, fmt.Errorf("%s already exists (slot %d)", name, n)
		}
	}
	for n := 1; n <= Slots; n++ {
		if _, used := m[n]; used {
			continue
		}
		if err := os.MkdirAll(h.path(SlotsDir), 0o700); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(h.path(slotFile(n)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return 0, err
		}
		_, err = f.WriteString(name + "\n")
		return n, errors.Join(err, f.Close())
	}
	return 0, fmt.Errorf("no free slot: all %d are in use", Slots)
}

// free removes what create made for name in slot n (n 0: only the disk); a part that's
// missing is skipped, so it also finishes an earlier free or create. The slot file goes last,
// under the lock, so the slot isn't reused before the rest is gone.
func (h *helper) free(ctx context.Context, n int, name string) error {
	if err := os.Remove(h.path(disk(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if n == 0 {
		return nil
	}
	// destroy, unlike delete, accepts a missing element.
	if _, err := h.run(ctx, cmdTimeout, "nft destroy element", NFT, "destroy", "element", "inet", NFTTable, NFTAllowSet, nftElement(n)); err != nil {
		return err
	}
	if err := os.Remove(h.path(conflist(n))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if ok, err := h.exists("/sys/class/net/" + bridge(n)); err != nil {
		return err
	} else if ok {
		if _, err := h.run(ctx, cmdTimeout, "ip link del", IP, "link", "del", bridge(n)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(h.path(CNIStateDir + "/" + network(n))); err != nil {
		return err
	}
	unlock, err := h.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(h.path(slotFile(n))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// containerIDRE is a full container id.
var containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rm removes the workspace: its container (killing its processes when nerdctl hangs), then
// its slot. A workspace that doesn't exist is no error.
func (h *helper) rm(ctx context.Context, name string) error {
	var out bytes.Buffer
	if err := h.nerdctl(ctx, &out, "ps", "-a", "--no-trunc", "--format", "{{.ID}} {{.Names}}"); err != nil {
		return err
	}
	id := ""
	for l := range strings.Lines(out.String()) {
		if f := strings.Fields(l); len(f) == 2 && f[1] == name {
			id = f[0]
		}
	}
	if id != "" {
		if !containerIDRE.MatchString(id) {
			return fmt.Errorf("nerdctl ps: %s's id %q isn't one", name, id)
		}
		if err := h.removeContainer(ctx, name, id); err != nil {
			return err
		}
	}
	n, err := h.slotOf(name)
	if err != nil {
		return err
	}
	return h.free(ctx, n, name)
}

// removeContainer stops and removes the container, each bounded by Bound. When rm -f fails or
// hangs (Q8 saw it wait forever on a systemd PID 1), it kills every process whose command line
// holds the container's id (its shim, cloud-hypervisor, virtiofsd) and tries rm -f again.
func (h *helper) removeContainer(ctx context.Context, name, id string) error {
	g := append([]string{}, NerdctlGlobal...)
	// a failed or hung stop leaves it to rm -f.
	_, _ = h.run(ctx, h.o.Bound, "nerdctl stop", Nerdctl, append(g, "stop", "-t", "30", name)...)
	if _, err := h.run(ctx, h.o.Bound, "nerdctl rm", Nerdctl, append(g, "rm", "-f", name)...); err == nil {
		return nil
	}
	// pkill exits 1 when nothing matched.
	if _, err := h.run(ctx, cmdTimeout, "pkill", Pkill, "-KILL", "-f", "--", id); err != nil && !exitedWith(err, 1) {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.o.KillWait):
	}
	if _, err := h.run(ctx, h.o.Bound, "nerdctl rm", Nerdctl, append(g, "rm", "-f", name)...); err != nil {
		return fmt.Errorf("after killing its processes: %w", err)
	}
	return nil
}

// exitedWith reports whether err is an exit with status code.
func exitedWith(err error, code int) bool {
	var ec interface{ ExitCode() int }
	return errors.As(err, &ec) && ec.ExitCode() == code
}

// inspect prints the workspace's slot, addresses and whether it runs.
func (h *helper) inspect(ctx context.Context, name string) error {
	n, err := h.slotOf(name)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s has no slot", name)
	}
	var out bytes.Buffer
	if err := h.nerdctl(ctx, &out, "inspect", "--format", "{{.State.Running}}", name); err != nil {
		return err
	}
	running, err := strconv.ParseBool(strings.TrimSpace(out.String()))
	if err != nil {
		return fmt.Errorf("nerdctl inspect: %w", err)
	}
	info := slotInfo(n)
	info.Running = &running
	return json.NewEncoder(h.o.Stdout).Encode(info)
}
