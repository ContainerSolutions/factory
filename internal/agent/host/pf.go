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
	"time"
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

// The bridges the anchor filters. macOS's pf has no interface groups (XNU's pfi_kif_match
// matches one interface, by name), so "on bridge" would match nothing: the rules name each
// bridge. Softnet and vmnet number their bridges from bridge100 up, one per running VM, and
// pf attaches a rule to a named interface when it appears. 64 is many more VMs than one Mac
// runs. Softnet's subnets vary (Phase 0 saw 192.168.96.x, 192.168.17.x, 192.168.83.x and
// 172.23.33.x), so the rules match the interface, not a subnet.
const (
	pfFirstBridge = 100
	pfBridges     = 64
)

// pfBridgeRange is the bridges' names for the notice and the docs: "bridge100 to bridge163".
func pfBridgeRange() string {
	return fmt.Sprintf("bridge%d to bridge%d", pfFirstBridge, pfFirstBridge+pfBridges-1)
}

// pfBridgeMacro is the anchor's bridges macro, eight interfaces a line.
func pfBridgeMacro() string {
	var b strings.Builder
	b.WriteString(`bridges = "{`)
	for i := range pfBridges {
		if i > 0 && i%8 == 0 {
			b.WriteString(" \\\n ")
		}
		fmt.Fprintf(&b, " bridge%d", pfFirstBridge+i)
	}
	b.WriteString(` }"`)
	return b.String()
}

// PFRules is the anchor, for the gateway port.
func PFRules(gatewayPort int) string {
	return fmt.Sprintf(`# %s: factory workspaces reach only their gateway on the Mac.
# macOS pf has no interface groups, so the rules name %s: Softnet and vmnet
# number their bridges from bridge%d up, one per running VM.
%s
pass in quick on $bridges proto tcp from any to (self) port %d
pass in quick on $bridges proto udp from any to (self) port { 53, 67 }
block drop in quick on $bridges proto { tcp, udp } from any to (self)
`, PFAnchorName, pfBridgeRange(), pfFirstBridge, pfBridgeMacro(), gatewayPort)
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

// Check reports the pf step's state. When pf isn't fully installed and the owner opted out
// (pf.optout in the state dir), it's skipped instead.
func (s pfStep) Check(ctx context.Context) (Status, error) {
	st, err := s.installed(ctx)
	if err == nil && st.State == StateOK {
		return st, nil
	}
	if out, ok := readPFOptOut(s.e); ok {
		return out, nil
	}
	return st, err
}

func (s pfStep) installed(ctx context.Context) (Status, error) {
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
	return Status{State: StateOK, Detail: fmt.Sprintf("%s reach only port %d (and DNS, DHCP) on this Mac", pfBridgeRange(), s.e.Workspace.GatewayPort)}, nil
}

// pfOptOutPath records the owner's opt-out of the pf step: host init --no-pf, or a declined
// prompt, writes it; a host init that installs pf removes it.
func pfOptOutPath(e *Env) string { return filepath.Join(e.StateDir(), "pf.optout") }

// writePFOptOut records the opt-out, with the date and the reason.
func writePFOptOut(e *Env, reason string) error {
	b := fmt.Sprintf("# factory-agent host init skipped the pf step; host init without --no-pf asks again.\ndate=%s\nreason=%s\n",
		e.Clock.Now().UTC().Format(time.RFC3339), reason)
	return os.WriteFile(pfOptOutPath(e), []byte(b), 0o600)
}

func removePFOptOut(e *Env) error {
	if err := os.Remove(pfOptOutPath(e)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// readPFOptOut is the skipped status when the opt-out exists (ok false when it doesn't).
func readPFOptOut(e *Env) (Status, bool) {
	b, err := os.ReadFile(pfOptOutPath(e))
	if err != nil {
		return Status{}, false
	}
	date, reason := "an unknown date", "no reason recorded"
	for l := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "date="); ok && v != "" {
			date = v
		} else if v, ok := strings.CutPrefix(strings.TrimSpace(l), "reason="); ok && v != "" {
			reason = v
		}
	}
	return Status{State: StateSkipped, Detail: fmt.Sprintf("opted out on %s (%s, %s): workspaces can reach every listening service on this Mac's bridge address; host init without --no-pf asks again",
		date, reason, pfOptOutPath(e))}, true
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

// Plan's root lines write the anchor and the plist from quoted heredocs in the sudo script
// itself, into a temp dir root makes, and install them from there. Nothing root installs
// comes from the user-writable stage dir, so a file swapped there can't become a root
// LaunchDaemon, and --dry-run shows the exact bytes root installs (a sha256 check, as for
// Softnet's binary, would show only a hash).
func (s pfStep) Plan(context.Context) (Plan, error) {
	anchor, plist := s.files()
	a, p := quote(s.e.sys(PFAnchorPath)), quote(s.e.sys(PFPlistPath))
	ta, tp := `"$T/pf.anchor"`, `"$T/pf.plist"`
	ha, err := heredoc(ta, anchor)
	if err != nil {
		return Plan{}, err
	}
	hp, err := heredoc(tp, plist)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		Do: []string{
			fmt.Sprintf("render the anchor (gateway port %d, %s) and the LaunchDaemon into the sudo script", s.e.Workspace.GatewayPort, pfBridgeRange()),
			"install them, append " + pfConfLine + " to " + PFConfPath + " (backup " + PFConfBackup + ") and load the LaunchDaemon (sudo)",
		},
		Root: []string{
			"T=$(/usr/bin/mktemp -d /tmp/factory-agent-root.XXXXXX)",
			`trap '/bin/rm -rf "$T"' EXIT`,
			ha,
			hp,
			"/sbin/pfctl -n -a " + PFAnchorName + " -f " + ta,
			"/usr/bin/install -o root -g wheel -m 0644 " + ta + " " + a,
			"/usr/bin/install -o root -g wheel -m 0644 " + tp + " " + p,
		},
	}
	plan.Root = append(plan.Root, pfConfAppend(s.e)...)
	plan.Root = append(plan.Root, pfLoad(s.e.sys(PFAnchorPath), s.e.sys(PFConfPath))...)
	plan.Root = append(plan.Root,
		"/bin/launchctl bootout system/"+PFLabel+" 2>/dev/null || true",
		"/bin/launchctl bootstrap system "+p,
	)
	return plan, nil
}

// heredocEOF ends the heredocs in the sudo script.
const heredocEOF = "FACTORY_AGENT_EOF"

// heredoc is the sh line that writes content, which must end with a newline, to path: a
// quoted heredoc, so sh expands nothing in it.
func heredoc(path, content string) (string, error) {
	if !strings.HasSuffix(content, "\n") || hasLine(content, heredocEOF) {
		return "", fmt.Errorf("can't put %s in a heredoc", path)
	}
	return "/bin/cat > " + path + " <<'" + heredocEOF + "'\n" + content + heredocEOF, nil
}

// pfConfAppend are the root lines that append the anchor line to pf.conf, after a backup of
// the original. When pf.conf doesn't end with a newline, one goes before the line (so its
// last line stays intact); pfConfRestore takes it out again.
func pfConfAppend(e *Env) []string {
	conf, backup := quote(e.sys(PFConfPath)), quote(e.sys(PFConfBackup))
	return []string{
		"if ! /usr/bin/grep -qxF '" + pfConfLine + "' " + conf + "; then",
		"  [ -e " + backup + " ] || /bin/cp -p " + conf + " " + backup,
		"  [ -z \"$(/usr/bin/tail -c 1 " + conf + ")\" ] || echo >> " + conf,
		"  echo '" + pfConfLine + "' >> " + conf,
		"fi",
	}
}

// pfConfRestore are the root lines that delete the anchor line from pf.conf, keeping any
// other edit. When that leaves the backup plus the newline pfConfAppend added (the backup
// doesn't end with one), the backup is copied back. The backup goes once pf.conf is back to
// it.
func pfConfRestore(e *Env) []string {
	conf, backup := quote(e.sys(PFConfPath)), quote(e.sys(PFConfBackup))
	return []string{
		"/usr/bin/sed -i '' '/^" + strings.ReplaceAll(pfConfLine, ".", `\.`) + "$/d' " + conf,
		"if [ -e " + backup + " ] && [ -n \"$(/usr/bin/tail -c 1 " + backup + ")\" ] && { /bin/cat " + backup + "; echo; } | /usr/bin/cmp -s - " + conf + "; then /bin/cp -p " + backup + " " + conf + "; fi",
		"if [ -e " + backup + " ] && /usr/bin/cmp -s " + conf + " " + backup + "; then /bin/rm -f " + backup + "; fi",
	}
}

// Apply has nothing to stage: the sudo script carries the anchor and the plist (see Plan).
func (pfStep) Apply(context.Context) error { return nil }

// RemovePFScript undoes the pf step: unloads the LaunchDaemon, removes it and the anchor,
// flushes the anchor's rules, deletes the anchor line from pf.conf (keeping any other edit)
// and reloads it. The backup goes only when pf.conf is back to it. pf itself stays enabled
// until the next boot.
func RemovePFScript(e *Env) string {
	lines := []string{
		"/bin/launchctl bootout system/" + PFLabel + " 2>/dev/null || true",
		"/bin/rm -f " + quote(e.sys(PFPlistPath)) + " " + quote(e.sys(PFAnchorPath)),
		"/sbin/pfctl -a " + PFAnchorName + " -F all 2>/dev/null || true",
	}
	lines = append(lines, pfConfRestore(e)...)
	return Script(append(lines, "/sbin/pfctl -f "+quote(e.sys(PFConfPath))))
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
