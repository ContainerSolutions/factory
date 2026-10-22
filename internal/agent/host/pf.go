package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The pf anchor's names and paths (P1 §3: Softnet's @host lets a VM reach every port on the
// Mac's bridge address, so the anchor allows only the gateway port).
const (
	pfName       = "pf"
	PFAnchorName = "com.compliance-framework.factory"
	PFAnchorPath = "/etc/pf.anchors/" + PFAnchorName
	PFLabel      = PFAnchorName + ".pf"
	PFPlistPath  = "/Library/LaunchDaemons/" + PFLabel + ".plist"
	PFConfPath   = "/etc/pf.conf"
	PFConfBackup = PFConfPath + ".factory-backup"
	pfConfLine   = `anchor "` + PFAnchorName + `"`
)

// PFRules is the anchor, for the gateway port.
func PFRules(gatewayPort int) string {
	return fmt.Sprintf(`# %s: factory workspaces reach only their gateway on the Mac.
pass in quick on bridge proto tcp from any to (self) port %d
pass in quick on bridge proto udp from any to (self) port { 53, 67 }
block drop in quick on bridge proto { tcp, udp } from any to (self)
`, PFAnchorName, gatewayPort)
}

// pfLoad are the commands that enable pf and load the anchor: the LaunchDaemon runs them at
// boot, and host init's root script runs them itself before bootstrapping the daemon (whose
// run launchd starts asynchronously), so a failure stops the script.
func pfLoad(anchor, conf string) []string {
	return []string{"/sbin/pfctl -E -f " + quote(conf), "/sbin/pfctl -a " + PFAnchorName + " -f " + quote(anchor)}
}

// PFPlist is the LaunchDaemon that enables pf and loads the anchor at boot.
func PFPlist(anchor, conf string) string {
	cmd := strings.Join(pfLoad(anchor, conf), " && ")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + PFLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + xmlEscape(cmd) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// pfStep installs the anchor, the LaunchDaemon that loads it, and the anchor line in
// pf.conf.
type pfStep struct{ e *Env }

func (pfStep) Name() string { return pfName }

func (s pfStep) files() (anchor, plist string) {
	return PFRules(s.e.Workspace.GatewayPort), PFPlist(s.e.sys(PFAnchorPath), s.e.sys(PFConfPath))
}

func (s pfStep) Check(ctx context.Context) (Status, error) {
	anchor, plist := s.files()
	for _, f := range []struct{ path, want, what string }{
		{PFAnchorPath, anchor, fmt.Sprintf("the rendered rules (gateway port %d)", s.e.Workspace.GatewayPort)},
		{PFPlistPath, plist, "the rendered LaunchDaemon"},
	} {
		p := s.e.sys(f.path)
		got, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return Status{State: StateMissing, Detail: p}, nil
		case err != nil:
			return Status{}, err
		case string(got) != f.want:
			return Status{State: StateWrong, Detail: fmt.Sprintf("%s differs from %s", p, f.what)}, nil
		}
	}
	conf, err := os.ReadFile(s.e.sys(PFConfPath))
	if err != nil {
		return Status{}, err
	}
	if !hasLine(string(conf), pfConfLine) {
		return Status{State: StateMissing, Detail: s.e.sys(PFConfPath) + " has no " + pfConfLine}, nil
	}
	out, err := s.e.output(ctx, "launchctl", "print", "system/"+PFLabel)
	if err != nil {
		return Status{State: StateMissing, Detail: "the LaunchDaemon " + PFLabel + " isn't loaded"}, nil
	}
	if code := lastExitCode(out); code != 0 {
		return Status{State: StateWrong, Detail: fmt.Sprintf("the LaunchDaemon %s last exited with %d: pfctl didn't load the anchor", PFLabel, code)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("bridges reach only port %d (and DNS, DHCP) on this Mac", s.e.Workspace.GatewayPort)}, nil
}

// lastExitCode is the "last exit code = N" of launchctl print's output; 0 when it has none
// (the job hasn't exited yet) or it isn't a number.
func lastExitCode(out string) int {
	for l := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "last exit code = "); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

func (s pfStep) Plan(context.Context) (Plan, error) {
	a, p, conf := quote(s.e.sys(PFAnchorPath)), quote(s.e.sys(PFPlistPath)), quote(s.e.sys(PFConfPath))
	sa, sp := quote(filepath.Join(s.e.StageDir, "pf.anchor")), quote(filepath.Join(s.e.StageDir, "pf.plist"))
	plan := Plan{
		Do: []string{
			fmt.Sprintf("render the anchor (gateway port %d) and the LaunchDaemon", s.e.Workspace.GatewayPort),
			"install them, append " + pfConfLine + " to " + PFConfPath + " (backup " + PFConfBackup + ") and load the LaunchDaemon (sudo)",
		},
		Root: []string{
			"/sbin/pfctl -n -a " + PFAnchorName + " -f " + sa,
			"/usr/bin/install -o root -g wheel -m 0644 " + sa + " " + a,
			"/usr/bin/install -o root -g wheel -m 0644 " + sp + " " + p,
			// append only, after a backup of the original; the newline guard keeps the
			// last line intact, so --remove-pf's delete restores the file exactly.
			"if ! /usr/bin/grep -qxF '" + pfConfLine + "' " + conf + "; then",
			"  [ -e " + quote(s.e.sys(PFConfBackup)) + " ] || /bin/cp -p " + conf + " " + quote(s.e.sys(PFConfBackup)),
			"  [ -z \"$(/usr/bin/tail -c 1 " + conf + ")\" ] || echo >> " + conf,
			"  echo '" + pfConfLine + "' >> " + conf,
			"fi",
		},
	}
	plan.Root = append(plan.Root, pfLoad(s.e.sys(PFAnchorPath), s.e.sys(PFConfPath))...)
	plan.Root = append(plan.Root,
		"/bin/launchctl bootout system/"+PFLabel+" 2>/dev/null || true",
		"/bin/launchctl bootstrap system "+p,
	)
	return plan, nil
}

func (s pfStep) Apply(context.Context) error {
	anchor, plist := s.files()
	if err := os.WriteFile(filepath.Join(s.e.StageDir, "pf.anchor"), []byte(anchor), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.e.StageDir, "pf.plist"), []byte(plist), 0o644)
}

// RemovePFScript undoes the pf step: unloads the LaunchDaemon, removes it and the anchor,
// flushes the anchor's rules, deletes the anchor line from pf.conf (keeping any other edit)
// and reloads it. The backup goes only when pf.conf is back to it. pf itself stays enabled
// until the next boot.
func RemovePFScript(e *Env) string {
	conf, backup := quote(e.sys(PFConfPath)), quote(e.sys(PFConfBackup))
	return Script([]string{
		"/bin/launchctl bootout system/" + PFLabel + " 2>/dev/null || true",
		"/bin/rm -f " + quote(e.sys(PFPlistPath)) + " " + quote(e.sys(PFAnchorPath)),
		"/sbin/pfctl -a " + PFAnchorName + " -F all 2>/dev/null || true",
		"/usr/bin/sed -i '' '/^" + strings.ReplaceAll(pfConfLine, ".", `\.`) + "$/d' " + conf,
		"if [ -e " + backup + " ] && /usr/bin/cmp -s " + conf + " " + backup + "; then /bin/rm -f " + backup + "; fi",
		"/sbin/pfctl -f " + conf,
	})
}

// RemovePF runs RemovePFScript with one sudo, or only prints it with dryRun.
func RemovePF(ctx context.Context, e *Env, dryRun bool) error {
	script := RemovePFScript(e)
	if dryRun {
		fmt.Fprintf(e.Out, "sudo script (one sudo prompt):\n%s", script)
		return nil
	}
	dir, err := os.MkdirTemp("", "factory-agent-host-")
	if err != nil {
		return err
	}
	e.StageDir = dir
	defer func() { _ = os.RemoveAll(dir); e.StageDir = "" }()
	return runRoot(ctx, e, script)
}
