package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/agent/katahelper"
)

// The kata driver's services (workspace-kata.md#containerd-and-buildkitd,
// #the-workspace-network): the factory's containerd and buildkitd, and the workspace firewall.

const (
	containerdName = "containerd"
	networkName    = "network"
)

const (
	containerdConfigPath = katahelper.ConfigDir + "/containerd.toml"
	buildkitdConfigPath  = katahelper.ConfigDir + "/buildkitd.toml"
	nftPath              = katahelper.ConfigDir + "/workspaces.nft"
	systemdDir           = "/etc/systemd/system"
	containerdUnit       = "factory-agent-containerd.service"
	buildkitUnit         = "factory-agent-buildkit.service"
	nftUnit              = "factory-agent-nft.service"
	// socketWait is how long the sudo script waits for containerd's and buildkitd's sockets,
	// in half seconds (30 s).
	socketWait = 60
)

// docURL names the LLD in the units.
const docURL = "https://github.com/ContainerSolutions/factory/blob/main/design/workstreams/agent/LLD/workspace-kata.md"

// sysFile is a file a step renders, at its path on the host.
type sysFile struct{ path, content string }

// checkFiles reports the first of files that is missing or differs from its rendering; nil
// when they all hold exactly the rendered text.
func checkFiles(e *Env, files []sysFile) (*Status, error) {
	for _, f := range files {
		p := e.sys(f.path)
		got, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return &Status{State: StateMissing, Detail: p}, nil
		case err != nil:
			return nil, err
		case string(got) != f.content:
			return &Status{State: StateWrong, Detail: p + " differs from the rendered file"}, nil
		}
	}
	return nil, nil
}

// rootInstallFiles are the root lines that write files from heredocs into $T, and those that
// install them from there root:root 0644.
func rootInstallFiles(e *Env, files []sysFile) (write, install []string, err error) {
	for _, f := range files {
		tmp := `"$T/` + filepath.Base(f.path) + `"`
		h, err := heredoc(tmp, f.content)
		if err != nil {
			return nil, nil, err
		}
		write = append(write, h)
		install = append(install, "/usr/bin/install -o root -g root -m 0644 "+tmp+" "+quote(e.sys(f.path)))
	}
	return write, install, nil
}

// unitStatus is wrong when the systemd unit isn't enabled and active; nil when it is.
func unitStatus(ctx context.Context, e *Env, unit string) *Status {
	if got := e.outputAnyway(ctx, "systemctl", "is-enabled", unit); got != "enabled" {
		return &Status{State: StateWrong, Detail: fmt.Sprintf("%s isn't enabled (systemctl is-enabled: %s)", unit, got)}
	}
	if got := e.outputAnyway(ctx, "systemctl", "is-active", unit); got != "active" {
		return &Status{State: StateWrong, Detail: fmt.Sprintf("%s isn't active (systemctl is-active: %s): journalctl -u %s", unit, got, unit)}
	}
	return nil
}

// outputAnyway runs a command and returns its first line of output, also when it fails
// (systemctl is-enabled and is-active exit non-zero with the state on stdout).
func (e *Env) outputAnyway(ctx context.Context, name string, args ...string) string {
	var b bytes.Buffer
	_ = e.Runner.Run(ctx, &b, name, args...)
	if l := firstLine(b.String()); l != "" {
		return l
	}
	return "no output"
}

// ContainerdConfig is the factory's containerd's config: its own root, state, sockets and opt
// dir, apart from Docker's containerd, without CRI or NRI.
func ContainerdConfig() string {
	run := filepath.Dir(katahelper.ContainerdSocket)
	return `# ` + containerdConfigPath + ` (factory-agent host init)
version = 3
root = "` + katahelper.StateDir + `/containerd"
state = "` + run + `"
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime", "io.containerd.nri.v1.nri"]
[grpc]
  address = "` + katahelper.ContainerdSocket + `"
[ttrpc]
  address = "` + katahelper.ContainerdSocket + `.ttrpc"
[debug]
  address = "` + run + `/debug.sock"
[plugins."io.containerd.internal.v1.opt"]
  path = "` + katahelper.StateDir + `/containerd-opt"
`
}

// BuildkitdConfig is buildkitd's config. Its unit passes every setting but the build steps'
// resolver option as a flag; the file is there so buildkitd reads it (--config) and never
// /etc/buildkit/buildkitd.toml, another BuildKit's. [dns] sets only options: buildkitd keeps the
// host's nameservers and search domains in the resolv.conf it writes for host-network steps.
func BuildkitdConfig() string {
	return `# ` + buildkitdConfigPath + ` (factory-agent host init)
# factory-agent-buildkit.service sets everything else with flags; buildkitd reads this file
# instead of /etc/buildkit/buildkitd.toml, so another BuildKit's config never applies.

# Build steps run on the host network. no-aaaa makes their glibc lookups return IPv4 addresses
# only, as in Docker's builds (an IPv4-only bridge), so a host whose IPv6 route is advertised
# but broken can't stall a download (Playwright waits 5 s on each address). The nameservers
# stay the host's.
[dns]
  options = ["no-aaaa"]
`
}

// ContainerdUnit runs the factory's containerd. KillMode=process, as containerd's own unit:
// the shims and their VMs outlive a restart. PATH puts Kata's runtime-rs shim first.
func ContainerdUnit() string {
	return `# ` + systemdDir + `/` + containerdUnit + ` (factory-agent host init)
[Unit]
Description=factory-agent's containerd for the kata workspaces (apart from Docker's)
Documentation=` + docURL + `
After=network.target local-fs.target

[Service]
Environment=PATH=` + katahelper.Path + `
ExecStartPre=/usr/bin/install -d -m 0711 ` + filepath.Dir(katahelper.ContainerdSocket) + `
ExecStart=` + katahelper.NerdctlFullDir + `/bin/containerd --config ` + containerdConfigPath + `
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`
}

// BuildkitUnit runs the factory's buildkitd, with the factory's containerd as its only worker.
func BuildkitUnit() string {
	return `# ` + systemdDir + `/` + buildkitUnit + ` (factory-agent host init)
[Unit]
Description=factory-agent's BuildKit for the kata workspace image
Documentation=` + docURL + `
Requires=` + containerdUnit + `
After=` + containerdUnit + `

[Service]
Environment=PATH=` + katahelper.Path + `
ExecStartPre=/usr/bin/install -d -m 0711 ` + filepath.Dir(katahelper.BuildkitSocket) + `
ExecStart=` + katahelper.NerdctlFullDir + `/bin/buildkitd --config ` + buildkitdConfigPath + ` \
  --addr unix://` + katahelper.BuildkitSocket + ` --root ` + katahelper.StateDir + `/buildkit \
  --oci-worker=false --containerd-worker=true --containerd-worker-addr=` + katahelper.ContainerdSocket + ` \
  --containerd-worker-namespace=` + katahelper.Namespace + ` --containerd-worker-net=host
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`
}

// containerdStep writes containerd's and buildkitd's configs and units, and runs both.
type containerdStep struct{ e *Env }

func (containerdStep) Name() string { return containerdName }

func (containerdStep) files() []sysFile {
	return []sysFile{
		{containerdConfigPath, ContainerdConfig()},
		{buildkitdConfigPath, BuildkitdConfig()},
		{systemdDir + "/" + containerdUnit, ContainerdUnit()},
		{systemdDir + "/" + buildkitUnit, BuildkitUnit()},
	}
}

func (s containerdStep) Check(ctx context.Context) (Status, error) {
	if st, err := checkFiles(s.e, s.files()); st != nil || err != nil {
		return deref(st), err
	}
	for _, u := range []string{containerdUnit, buildkitUnit} {
		if st := unitStatus(ctx, s.e, u); st != nil {
			return *st, nil
		}
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("%s and %s running (%s)", containerdUnit, buildkitUnit, katahelper.ContainerdSocket)}, nil
}

func (s containerdStep) Plan(context.Context) (Plan, error) {
	e := s.e
	write, install, err := rootInstallFiles(e, s.files())
	if err != nil {
		return Plan{}, err
	}
	root := rootTemp(rootTmp)
	root = append(root, "/usr/bin/install -d -o root -g root -m 0755 "+quote(e.sys(katahelper.ConfigDir)))
	root = append(append(root, write...), install...)
	root = append(root,
		"/usr/bin/systemctl daemon-reload",
		"/usr/bin/systemctl enable "+containerdUnit+" "+buildkitUnit,
		// a restart (not enable --now) so a changed config or unit takes effect; the shims and
		// their VMs outlive it (KillMode=process).
		"/usr/bin/systemctl restart "+containerdUnit+" "+buildkitUnit,
		fmt.Sprintf(`i=0; until [ -S %s ] && [ -S %s ]; do i=$((i+1)); if [ "$i" -gt %d ]; then %s; fi; /usr/bin/sleep 0.5; done`,
			katahelper.ContainerdSocket, katahelper.BuildkitSocket, socketWait,
			rootFail(fmt.Sprintf("containerd and buildkitd didn't open their sockets within %d s: journalctl -u %s -u %s", socketWait/2, containerdUnit, buildkitUnit))),
		rootTempDone,
	)
	return Plan{
		Do: []string{
			fmt.Sprintf("write %s, %s and the units %s and %s (sudo)", e.sys(containerdConfigPath), e.sys(buildkitdConfigPath), containerdUnit, buildkitUnit),
			fmt.Sprintf("enable and restart them, and wait for their sockets (%d s)", socketWait/2),
		},
		Root: root,
	}, nil
}

// Apply has nothing to stage: the sudo script carries the files.
func (containerdStep) Apply(context.Context) error { return nil }

// NFTRules is the workspace firewall (workspace-kata.md#the-workspace-network), for the
// gateway port. From a workspace bridge (fws<n>) only TCP to the gateway port passes, and only
// from that bridge's VM address to its gateway address (the set elements the helper adds);
// DNS is rejected so lookups fail at once; nothing is forwarded to or from a workspace. Its
// first two lines make loading it again replace the table, which also empties the set.
func NFTRules(gatewayPort int) string {
	t, br := katahelper.NFTTable, katahelper.BridgePrefix+"*"
	return fmt.Sprintf(`# %s (factory-agent host init)
# Kata workspaces reach only their own gateway's port, from their own address on their own
# bridge; nothing is forwarded to or from them. The first two lines make loading this file
# again replace the table.
table inet %[2]s
delete table inet %[2]s
table inet %[2]s {
	set %[3]s {
		type ifname . ipv4_addr . ipv4_addr
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname != "%[4]s" accept
		tcp dport %[5]d iifname . ip saddr . ip daddr @%[3]s accept
		udp dport 53 reject
		tcp dport 53 reject with tcp reset
		meta l4proto tcp reject with tcp reset
		reject
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "%[4]s" drop
		oifname "%[4]s" drop
	}
}
`, nftPath, t, katahelper.NFTAllowSet, br, gatewayPort)
}

// NFTUnit loads the firewall at boot, before the factory's containerd, which requires it: no
// workspace starts without the rules. It has no ExecStop: stopping it leaves the rules, since
// the VMs outlive containerd.
func NFTUnit() string {
	return `# ` + systemdDir + `/` + nftUnit + ` (factory-agent host init)
[Unit]
Description=factory-agent's kata workspace firewall (nft table inet ` + katahelper.NFTTable + `)
Documentation=` + docURL + `
Before=` + containerdUnit + `

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f ` + nftPath + `

[Install]
WantedBy=multi-user.target
RequiredBy=` + containerdUnit + `
`
}

// networkStep installs the workspace firewall and the unit that loads it.
type networkStep struct{ e *Env }

func (networkStep) Name() string { return networkName }

func (s networkStep) files() []sysFile {
	return []sysFile{{nftPath, NFTRules(s.e.Workspace.GatewayPort)}, {systemdDir + "/" + nftUnit, NFTUnit()}}
}

func (s networkStep) Check(ctx context.Context) (Status, error) {
	if st, err := checkFiles(s.e, s.files()); st != nil || err != nil {
		return deref(st), err
	}
	if st := unitStatus(ctx, s.e, nftUnit); st != nil {
		return *st, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("%s* bridges reach only port %d on their gateway (%s)", katahelper.BridgePrefix, s.e.Workspace.GatewayPort, nftUnit)}, nil
}

func (s networkStep) Plan(context.Context) (Plan, error) {
	e := s.e
	write, install, err := rootInstallFiles(e, s.files())
	if err != nil {
		return Plan{}, err
	}
	root := rootTemp(rootTmp)
	root = append(root, "/usr/bin/install -d -o root -g root -m 0755 "+quote(e.sys(katahelper.ConfigDir)))
	// nft parses root's copy before anything is installed.
	root = append(append(root, write...), `/usr/sbin/nft -c -f "$T/`+filepath.Base(nftPath)+`"`)
	root = append(root, install...)
	root = append(root,
		"/usr/bin/systemctl daemon-reload",
		// loaded here, since enable --now doesn't run an active unit again.
		"/usr/sbin/nft -f "+quote(e.sys(nftPath)),
		"/usr/bin/systemctl enable --now "+nftUnit,
		rootTempDone,
	)
	return Plan{
		Do: []string{fmt.Sprintf("write the workspace firewall %s (table inet %s, %s* bridges, gateway port %d) and %s; load it and enable the unit (sudo)",
			e.sys(nftPath), katahelper.NFTTable, katahelper.BridgePrefix, e.Workspace.GatewayPort, nftUnit)},
		Root: root,
	}, nil
}

// Apply has nothing to stage: the sudo script carries the files.
func (networkStep) Apply(context.Context) error { return nil }
