package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// installServices puts the containerd and network steps' files in place, their units enabled
// and active.
func installServices(t *testing.T, e *Env, r *fakeRunner) {
	t.Helper()
	for _, f := range append(containerdStep{e}.files(), networkStep{e}.files()...) {
		writeMode(t, e, f.path, f.content, 0o644)
	}
	r.units = map[string][2]string{}
	for _, u := range []string{containerdUnit, buildkitUnit, nftUnit} {
		r.units[u] = [2]string{"enabled", "active"}
	}
}

func TestKataServicesCheck(t *testing.T) {
	runStepCases(t, []stepCase{
		{"containerd", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "factory-agent-containerd.service and factory-agent-buildkit.service running"},
		{"containerd", "no config", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, containerdConfigPath) }, StateMissing, "containerd.toml"},
		{"containerd", "a drifted unit", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, systemdDir+"/"+buildkitUnit, BuildkitUnit()+"# edited\n")
		}, StateWrong, "factory-agent-buildkit.service differs from the rendered file"},
		{"containerd", "disabled", func(_ *testing.T, _ *Env, r *fakeRunner) { r.units[containerdUnit] = [2]string{"disabled", "active"} }, StateWrong,
			"factory-agent-containerd.service isn't enabled (systemctl is-enabled: disabled)"},
		{"containerd", "buildkit inactive", func(_ *testing.T, _ *Env, r *fakeRunner) { r.units[buildkitUnit] = [2]string{"enabled", "failed"} }, StateWrong,
			"factory-agent-buildkit.service isn't active (systemctl is-active: failed): journalctl -u factory-agent-buildkit.service"},
		{"containerd", "no unit", func(_ *testing.T, _ *Env, r *fakeRunner) { delete(r.units, containerdUnit) }, StateWrong,
			"isn't enabled (systemctl is-enabled: Failed to get unit file state"},

		{"network", "ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "fws* bridges reach only port 3128 on their gateway"},
		{"network", "missing", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, nftPath) }, StateMissing, "workspaces.nft"},
		{"network", "another gateway port", func(_ *testing.T, e *Env, _ *fakeRunner) { e.Workspace.GatewayPort = 3129 }, StateWrong,
			"workspaces.nft differs from the rendered file"},
		{"network", "inactive", func(_ *testing.T, _ *Env, r *fakeRunner) { r.units[nftUnit] = [2]string{"enabled", "inactive"} }, StateWrong,
			"factory-agent-nft.service isn't active"},
	})
}

// previousBuildkitdConfig is buildkitd.toml as host init wrote it before the [dns] section.
const previousBuildkitdConfig = `# /etc/factory-agent/buildkitd.toml (factory-agent host init)
# factory-agent-buildkit.service sets everything with flags; buildkitd reads this file instead
# of /etc/buildkit/buildkitd.toml, so another BuildKit's config never applies.
`

// TestBuildkitdConfigUpgrade: a host with the buildkitd.toml from before the [dns] section
// checks wrong, and the plan rewrites the file with no-aaaa and restarts both units.
func TestBuildkitdConfigUpgrade(t *testing.T) {
	e, r := kataEnv(t)
	installKata(t, e, r)
	writeRoot(t, e, buildkitdConfigPath, previousBuildkitdConfig)
	wantState(t, check(t, containerdStep{e}), StateWrong, "buildkitd.toml differs from the rendered file")
	p, err := containerdStep{e}.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(p.Root, "\n")
	for _, want := range []string{
		"[dns]\n  options = [\"no-aaaa\"]\n",
		"/usr/bin/install -o root -g root -m 0644 \"$T/buildkitd.toml\" " + quote(e.sys(buildkitdConfigPath)),
		"/usr/bin/systemctl restart " + containerdUnit + " " + buildkitUnit,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the plan has no %q:\n%s", want, script)
		}
	}
}

// TestNFTRules: the rules parse (nft -c -f) in a user and network namespace, load, and a
// reload replaces the table, emptying the set.
func TestNFTRules(t *testing.T) {
	bin, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("no nft")
	}
	if b, err := exec.Command("unshare", "-rn", bin, "-c", "-f", "/dev/null").CombinedOutput(); err != nil && !strings.Contains(string(b), "Not a regular file") {
		t.Skipf("no user and network namespace for nft: %v %s", err, b)
	}
	nft := filepath.Join(t.TempDir(), "workspaces.nft")
	if err := os.WriteFile(nft, []byte(NFTRules(3128)), 0o644); err != nil {
		t.Fatal(err)
	}
	script := bin + " -c -f " + nft + " && " + bin + " -f " + nft + " && " + bin + " add element inet " + katahelper.NFTTable + " " + katahelper.NFTAllowSet +
		` '{ "fws1" . 10.213.1.2 . 10.213.1.1 }' && ` + bin + " -f " + nft + " && " + bin + " list table inet " + katahelper.NFTTable
	b, err := exec.Command("unshare", "-rn", "/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("nft: %v\n%s", err, b)
	}
	if strings.Contains(string(b), "10.213.1.2") || !strings.Contains(string(b), "tcp dport 3128") {
		t.Errorf("after the reload:\n%s", b)
	}
}

// TestServiceHeredocs: every rendered file can go into the sudo script as a heredoc.
func TestServiceHeredocs(t *testing.T) {
	for name, s := range map[string]string{
		"containerd.toml": ContainerdConfig(), "buildkitd.toml": BuildkitdConfig(), "containerd unit": ContainerdUnit(),
		"buildkit unit": BuildkitUnit(), "nft": NFTRules(65535), "nft unit": NFTUnit(),
	} {
		if _, err := heredoc(`"$T/x"`, s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
