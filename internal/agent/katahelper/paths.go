// Package katahelper is the kata driver's root helper (design/workstreams/agent/LLD/
// workspace-kata.md#the-helper-internalagentkatahelper) and the host layout it shares with host
// init's kata steps. So far it has the layout; the helper itself comes in P1 phase 9's PR 3c.
// The helper runs as root, so it trusts nothing from its environment: every path it uses is a
// constant here, never a flag or an environment variable.
package katahelper

// Name is the helper's file name, installed at HelperPath.
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
	// Path is the PATH of every command the helper runs, and of the daemons' units: Kata's
	// runtime-rs shim first, so containerd finds it for io.containerd.kata.v2.
	Path = KataDir + "/runtime-rs/bin:" + NerdctlFullDir + "/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)
