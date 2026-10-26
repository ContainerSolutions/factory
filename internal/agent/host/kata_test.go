package host

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/config"
)

// The /proc and route fixtures of a kata host that passes the platform step.
const (
	testCPUInfo = "processor\t: 0\nvendor_id\t: GenuineIntel\nflags\t\t: fpu vme de pse msr vmx ssse3 sse4_1\n\nprocessor\t: 1\nflags\t\t: fpu vme de pse msr vmx ssse3 sse4_1\n"
	// 61 GiB and a bit: the workstation's 62 GiB less the firmware's share.
	testMemInfo = "MemTotal:       64395636 kB\nMemFree:        12345678 kB\nMemAvailable:   40000000 kB\n"
	testRoutes  = "default via 192.168.1.1 dev wlp0s20f3 proto dhcp src 192.168.1.20 metric 600\n" +
		"172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1 linkdown\n" +
		"192.168.1.0/24 dev wlp0s20f3 proto kernel scope link src 192.168.1.20 metric 600\n"
)

// kataEnv is an Ubuntu amd64 host that passes the kata platform step: KVM (a link to
// /dev/null, the character device a test can make), vmx, cgroup v2, systemd, the tools, 703
// GiB free and 61 GiB memory, under a temp Root; and no XDG_STATE_HOME. Its owner is "owner"
// (uid 1000), every file under Root is root's and every directory the owner's, and the
// running factory-agent is $HOME/bin/factory-agent.
func kataEnv(t *testing.T) (*Env, *fakeRunner) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the kata host's fixtures need /dev/null")
	}
	e, r := testEnv(t)
	e.GOOS, e.GOARCH = "linux", "amd64"
	e.Workspace = config.Workspace{}.WithDefaults("linux")
	e.LookPath = func(name string) (string, error) { return "/usr/sbin/" + name, nil }
	e.Getenv = func(string) string { return "" }
	e.UID = func() int { return 1000 }
	e.Owner = func(fi fs.FileInfo) (int, int, bool) {
		if fi.IsDir() {
			return 1000, 1000, true
		}
		return 0, 0, true
	}
	e.Username = func() (string, error) { return "owner", nil }
	e.Executable = filepath.Join(e.Home, "bin", "factory-agent")
	if err := os.WriteFile(e.Executable, []byte("factory-agent linux/amd64"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.freeKB, r.routes = "737000000", testRoutes
	files := map[string]string{
		"proc/cpuinfo":                     testCPUInfo,
		"proc/meminfo":                     testMemInfo,
		"sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids rdma misc\n",
	}
	for p, body := range files {
		writeRoot(t, e, p, body)
	}
	for _, d := range []string{"run/systemd/system", "dev", "var/lib"} {
		if err := os.MkdirAll(filepath.Join(e.Root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/dev/null", filepath.Join(e.Root, "dev", "kvm")); err != nil {
		t.Fatal(err)
	}
	return e, r
}

func writeRoot(t *testing.T, e *Env, p, body string) {
	t.Helper()
	p = filepath.Join(e.Root, p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeRoot(t *testing.T, e *Env, p string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(e.Root, p)); err != nil {
		t.Fatal(err)
	}
}

func TestKataPlatform(t *testing.T) {
	missingTool := func(tool string) func(*testing.T, *Env, *fakeRunner) {
		return func(_ *testing.T, e *Env, _ *fakeRunner) {
			e.LookPath = func(name string) (string, error) {
				if name == tool {
					return "", errors.New("not found")
				}
				return "/usr/sbin/" + name, nil
			}
		}
	}
	tests := []struct {
		name   string
		set    func(*testing.T, *Env, *fakeRunner)
		state  State
		detail string
	}{
		{"ok", func(*testing.T, *Env, *fakeRunner) {}, StateOK, "linux/amd64, KVM, cgroup v2, 61 GiB memory, 702 GiB free on /var/lib"},
		{"arm64", func(_ *testing.T, e *Env, _ *fakeRunner) { e.GOARCH = "arm64" }, StateWrong,
			"the kata driver needs linux/amd64: Kata's arm64 release has no runtime-rs Cloud Hypervisor config"},
		{"darwin", func(_ *testing.T, e *Env, _ *fakeRunner) { e.GOOS, e.GOARCH = "darwin", "arm64" }, StateWrong,
			"the kata driver needs linux/amd64, this is darwin/arm64"},
		{"no /dev/kvm", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, "dev/kvm") }, StateWrong, "no /dev/kvm"},
		{"/dev/kvm a regular file", func(t *testing.T, e *Env, _ *fakeRunner) {
			removeRoot(t, e, "dev/kvm")
			writeRoot(t, e, "dev/kvm", "")
		}, StateWrong, "/dev/kvm is not a character device"},
		{"no vmx or svm", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, "proc/cpuinfo", strings.ReplaceAll(testCPUInfo, " vmx", ""))
		}, StateWrong, "no vmx or svm flag in /proc/cpuinfo"},
		{"svm", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, "proc/cpuinfo", strings.ReplaceAll(testCPUInfo, " vmx", " svm"))
		}, StateOK, "KVM"},
		{"vmx outside the flags", func(t *testing.T, e *Env, _ *fakeRunner) {
			writeRoot(t, e, "proc/cpuinfo", "model name\t: vmx svm\nflags\t\t: fpu vme\n")
		}, StateWrong, "no vmx or svm flag"},
		{"cgroup v1", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, "sys/fs/cgroup/cgroup.controllers") }, StateWrong,
			"the kata driver needs cgroup v2"},
		{"no systemd", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, "run/systemd") }, StateWrong,
			"the kata driver needs systemd as PID 1"},
		{"no nft", missingTool("nft"), StateWrong, "not on PATH: nft"},
		{"no systemctl", missingTool("systemctl"), StateWrong, "not on PATH: systemctl"},
		{"no visudo", missingTool("visudo"), StateWrong, "not on PATH: visudo"},
		{"no zstd", missingTool("zstd"), StateWrong, "not on PATH: zstd"},
		{"low disk", func(_ *testing.T, _ *Env, r *fakeRunner) { r.freeKB = "104857600" }, StateWrong,
			"100 GiB free on /var/lib's volume, want 120 GiB"},
		{"low memory", func(_ *testing.T, e *Env, _ *fakeRunner) { e.Workspace.Size.Memory = "60Gi" }, StateWrong,
			"61 GiB memory: a 60Gi workspace needs 64 GiB"},
		{"a route inside the pool", func(_ *testing.T, _ *Env, r *fakeRunner) { r.routes += "10.213.4.0/24 dev x proto kernel scope link\n" }, StateWrong,
			"routes already in the slot pool 10.213.0.0/16: 10.213.4.0/24"},
		{"a route around the pool", func(_ *testing.T, _ *Env, r *fakeRunner) { r.routes += "10.0.0.0/8 via 192.168.1.254 dev wlp0s20f3\n" }, StateWrong,
			"routes already in the slot pool 10.213.0.0/16: 10.0.0.0/8"},
		{"a host route in the pool", func(_ *testing.T, _ *Env, r *fakeRunner) { r.routes += "10.213.0.9 dev tun0 scope link\n" }, StateWrong,
			"slot pool 10.213.0.0/16: 10.213.0.9"},
		{"a blackhole in the pool", func(_ *testing.T, _ *Env, r *fakeRunner) { r.routes += "blackhole 10.213.0.0/16 proto static\n" }, StateWrong,
			"slot pool 10.213.0.0/16: 10.213.0.0/16"},
		{"a workspace's own bridge", func(_ *testing.T, _ *Env, r *fakeRunner) {
			r.routes += "10.213.3.0/29 dev fws3 proto kernel scope link src 10.213.3.1\n10.213.63.0/29 dev fws63 proto kernel scope link src 10.213.63.1 linkdown\n"
		}, StateOK, "linux/amd64"},
		{"a slot's prefix on another dev", func(_ *testing.T, _ *Env, r *fakeRunner) {
			r.routes += "10.213.3.0/29 dev br-7db5151d33fc proto kernel scope link src 10.213.3.1\n"
		}, StateWrong, "slot pool 10.213.0.0/16: 10.213.3.0/29"},
		{"a route next to the pool", func(_ *testing.T, _ *Env, r *fakeRunner) {
			r.routes += "10.214.0.0/16 dev br-1 proto kernel scope link\n10.212.0.0/16 dev br-2\n"
		}, StateOK,
			"linux/amd64"},
		{"every problem", func(t *testing.T, e *Env, r *fakeRunner) {
			removeRoot(t, e, "dev/kvm")
			removeRoot(t, e, "run/systemd")
			r.freeKB = "1048576"
		}, StateWrong, "no /dev/kvm: enable virtualization in the firmware and load kvm_intel or kvm_amd; " +
			"no /run/systemd/system: the kata driver needs systemd as PID 1; 1 GiB free on /var/lib's volume"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := kataEnv(t)
			tt.set(t, e, r)
			wantState(t, check(t, kataPlatformStep{e}), tt.state, tt.detail)
		})
	}
}

// TestKataPlatformErrors: a host whose facts can't be read fails the check (Check reports it
// wrong, with the error).
func TestKataPlatformErrors(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T, *Env, *fakeRunner)
		err  string
	}{
		{"no cpuinfo", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, "proc/cpuinfo") }, "the CPU's flags"},
		{"no meminfo", func(t *testing.T, e *Env, _ *fakeRunner) { removeRoot(t, e, "proc/meminfo") }, "the physical memory"},
		{"no MemTotal", func(t *testing.T, e *Env, _ *fakeRunner) { writeRoot(t, e, "proc/meminfo", "MemFree: 1 kB\n") }, "no MemTotal line"},
		{"MemTotal in MB", func(t *testing.T, e *Env, _ *fakeRunner) { writeRoot(t, e, "proc/meminfo", "MemTotal: 62000 MB\n") }, "unexpected MemTotal line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, r := kataEnv(t)
			tt.set(t, e, r)
			sts := Check(context.Background(), []Step{kataPlatformStep{e}})
			wantState(t, sts[0], StateWrong, tt.err)
		})
	}
}

func TestRoutesIn(t *testing.T) {
	out := testRoutes +
		"unreachable 10.213.8.0/22 proto static\n" +
		"10.208.0.0/12 via 10.0.0.1 dev eth0\n" + // 10.208.0.0 to 10.223.255.255: holds the pool
		"nexthop via 10.213.0.1 dev eth1 weight 1\n" +
		"10.213.255.255 dev eth0 scope link\n" +
		"10.214.0.1 dev eth0 scope link\n" +
		// the workspaces' own slot routes are skipped; anything else on their bridges isn't.
		"10.213.1.0/29 dev fws1 proto kernel scope link src 10.213.1.1\n" +
		"10.213.4.0/29 dev fws3 proto kernel scope link src 10.213.4.1\n" +
		"10.213.0.0/16 dev fws1\n" +
		"10.213.64.0/29 dev fws64\n" +
		"10.213.5.0/29 dev fws05\n" +
		"10.213.6.0/28 dev fws6\n" +
		"10.213.7.0/29 via 10.213.7.1\n"
	got := routesIn(out, KataSlotPool)
	want := []string{"10.213.8.0/22", "10.208.0.0/12", "10.213.255.255",
		"10.213.4.0/29", "10.213.0.0/16", "10.213.64.0/29", "10.213.5.0/29", "10.213.6.0/28", "10.213.7.0/29"}
	if !slices.Equal(got, want) {
		t.Errorf("routesIn: %q, want %q", got, want)
	}
	if KataSlotPool != netip.MustParsePrefix("10.213.0.0/16") {
		t.Errorf("KataSlotPool is %s", KataSlotPool)
	}
}

func TestStateDirPath(t *testing.T) {
	tests := []struct {
		name, goos, xdg, want string
	}{
		{"darwin", "darwin", "/x/state", "/home/o/Library/Application Support/factory-agent"},
		{"linux", "linux", "", "/home/o/.local/state/factory-agent"},
		{"linux with XDG_STATE_HOME", "linux", "/x/state", "/x/state/factory-agent"},
		{"linux with a relative XDG_STATE_HOME", "linux", "x/state", "/home/o/.local/state/factory-agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Env{Home: "/home/o", GOOS: tt.goos, Getenv: func(k string) string {
				if k != "XDG_STATE_HOME" {
					t.Errorf("Getenv(%q)", k)
				}
				return tt.xdg
			}}
			if got := e.StateDir(); got != filepath.FromSlash(tt.want) {
				t.Errorf("StateDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKataSteps(t *testing.T) {
	e, _ := kataEnv(t)
	steps, err := Steps(e)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name())
	}
	if !slices.Equal(names, []string{"platform", "statedir", "nerdctl", "kata", "kataconfig", "containerd", "network", "privileges", "kataimage", "katawarm"}) {
		t.Errorf("kata steps %v", names)
	}
	e.Workspace.Driver = "docker"
	if _, err := Steps(e); err == nil || !strings.Contains(err.Error(), `no host steps for the "docker" driver`) {
		t.Errorf("Steps for docker: %v", err)
	}
	e.Workspace.Driver = config.DriverKata

	// init --only statedir applies the state dir at the XDG path, and nothing else.
	e.Getenv = func(string) string { return filepath.Join(e.Home, "xdg") }
	sts, err := Init(context.Background(), e, InitOptions{Only: []string{"statedir"}})
	if err != nil || !AllOK(sts[:2]) || sts[2].State != StateMissing {
		t.Fatalf("Init: %v %+v", err, sts)
	}
	if fi, err := os.Stat(filepath.Join(e.Home, "xdg", "factory-agent")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the state dir: %v %v", fi, err)
	}
}
