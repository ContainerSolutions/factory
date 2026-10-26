// Package katahelper is the kata driver's root helper (design/workstreams/agent/LLD/
// workspace-kata.md#the-helper-internalagentkatahelper), and the host layout it shares with host
// init's kata steps. The helper is a root-owned copy of factory-agent, installed by host init at
// HelperPath, that the owner runs with sudo -n; factory-agent's main runs Main when its argv[0]'s
// base name is Name.
//
// It runs as root, so it trusts nothing from its environment: every path is a constant here,
// the commands it runs get an environment it builds itself (Env), and every argument is
// validated before anything runs. Its verbs take only workspace names, sizes, the image's tag
// and fixed words; image-build reads its build context as a tar on stdin, never from a path.
// The warm-disk verbs come with the warm step (P1 phase 9, PR 5b).
package katahelper

import "net/netip"

// Name is the helper's file name: factory-agent runs Main when invoked under it.
const Name = "kata-helper"

// The host's layout (workspace-kata.md#host-layout): host init installs and renders these, and
// the helper's commands use them. They are constants, never flags or environment.
const (
	// LibexecDir holds the helper and nerdctl-full.
	LibexecDir = "/usr/local/libexec/factory-agent"
	// HelperPath is the root-owned helper the sudoers rule allows.
	HelperPath = LibexecDir + "/" + Name
	// NerdctlFullDir is the extracted nerdctl-full release.
	NerdctlFullDir = LibexecDir + "/nerdctl-full"
	// Nerdctl is the pinned nerdctl.
	Nerdctl = NerdctlFullDir + "/bin/nerdctl"
	// CNIPluginDir holds nerdctl-full's CNI plugins.
	CNIPluginDir = NerdctlFullDir + "/libexec/cni"
	// KataDir is where Kata's static release unpacks (its configs name it).
	KataDir = "/opt/kata"
	// ConfigDir holds the rendered configs, the firewall and the CNI conflists.
	ConfigDir = "/etc/factory-agent"
	// CNIConfDir holds the workspaces' conflists.
	CNIConfDir = ConfigDir + "/cni/net.d"
	// StateDir is containerd's, nerdctl's, BuildKit's and CNI's state, and the workspaces' disks.
	StateDir = "/var/lib/factory-agent"
	// NerdctlDataRoot is nerdctl's own state.
	NerdctlDataRoot = StateDir + "/nerdctl"
	// RunDir holds the daemons' sockets.
	RunDir = "/run/factory-agent"
	// ContainerdSocket is the factory's containerd, apart from Docker's.
	ContainerdSocket = RunDir + "/containerd/containerd.sock"
	// BuildkitSocket is the factory's buildkitd.
	BuildkitSocket = RunDir + "/buildkit/buildkitd.sock"
	// Namespace is containerd's namespace for the workspaces and the image.
	Namespace = "factory"
	// NFTTable is the workspace firewall's table (inet), and NFTAllowSet its set of
	// (bridge, VM address, gateway address) elements, one per workspace.
	NFTTable    = "factory_workspaces"
	NFTAllowSet = "ws_allow"
	// BridgePrefix starts every workspace bridge's name: fws<slot>.
	BridgePrefix = "fws"
	// KataStateDir holds the workspaces' disks and slot files, and the warm disk.
	KataStateDir = StateDir + "/kata"
	// DisksDir holds each workspace's disk, <name>.img: its only mount, at GuestDisk.
	DisksDir = KataStateDir + "/disks"
	// SlotsDir holds a file per slot in use, named its number and holding the workspace's name.
	SlotsDir = KataStateDir + "/slots"
	// WarmDisk is the pre-filled disk create copies for a warm workspace.
	WarmDisk = KataStateDir + "/warm-docker.img"
	// BuildDir holds image-build's copies of the build contexts it receives, root-only.
	BuildDir = StateDir + "/build"
	// GuestDisk is where a workspace's disk file appears in it.
	GuestDisk = "/var/lib/factory/docker.img"
	// CNIStateDir is host-local IPAM's dataDir: a network's leases are in CNIStateDir/<network>.
	CNIStateDir = StateDir + "/cni"
	// LockFile is flocked around every slot allocation and release.
	LockFile = RunDir + "/kata.lock"
	// Slots is the number of workspace slots: slot n is 1 to Slots, its bridge BridgePrefix<n>.
	Slots = 63
	// ImageRepo is the workspace image's repository: its tag is factory-agent's version.
	ImageRepo = "factory-agent-base"
	// Runtime is Kata's containerd runtime (its runtime-rs shim, on Path).
	Runtime = "io.containerd.kata.v2"

	// The other programs the helper runs, by absolute path (Ubuntu's).
	IP       = "/usr/sbin/ip"
	NFT      = "/usr/sbin/nft"
	Sysctl   = "/usr/sbin/sysctl"
	CP       = "/usr/bin/cp"
	Truncate = "/usr/bin/truncate"
	Pkill    = "/usr/bin/pkill"

	// Path is the PATH of every command the helper runs, and of the daemons' units: Kata's
	// runtime-rs shim first, so containerd finds it for io.containerd.kata.v2.
	Path = KataDir + "/runtime-rs/bin:" + NerdctlFullDir + "/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// SlotPool is the workspaces' addresses (workspace-kata.md#the-workspace-network): slot n is
// 10.213.<n>.0/29, its bridge (the gateway) at .1 and its VM at .2.
var SlotPool = netip.MustParsePrefix("10.213.0.0/16")
