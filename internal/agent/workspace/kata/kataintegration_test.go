//go:build kataintegration

// The kata driver against the real helper, containerd and Kata (workspace-kata.md#tests), on a
// host where `factory-agent host check --driver kata` passes and the base image exists:
//
//	go test -tags kataintegration -count=1 -v -timeout 30m ./internal/agent/workspace/kata/
//
// FACTORY_KATA_IMAGE names the image (default factory-agent-base:<this build's version>),
// FACTORY_KATA_DISK_GIB the empty disks' size (default 60, the config's default size.disk) and
// FACTORY_KATA_GATEWAY_PORT the config's workspace.gatewayPort (default 3128), where the test's
// stand-in gateways listen. Commands run as the workspace's admin, as factory-agent run's do. The isolation checks need the host's own internet (I01, I02). Each
// test destroys its workspaces; `sudo -n kata-helper list` shows a leftover.
package kata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/agent/katahelper"
	"github.com/compliance-framework/factory/internal/agent/workspace"
	"github.com/compliance-framework/factory/internal/version"
)

// spoofSetupFailed is a spoof check's exit when it couldn't add the spoofed address: outside
// curl's and timeout's statuses.
const spoofSetupFailed = 222

// itDriver returns a driver for this host, or skips the test saying what's missing.
func itDriver(t *testing.T) *Driver {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("kataintegration runs on a linux/amd64 kata agent host")
	}
	helper := func(args ...string) (string, error) {
		out, err := exec.Command("sudo", append([]string{"-n", katahelper.HelperPath}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := helper("protocol"); err != nil || out != strconv.Itoa(katahelper.Protocol) {
		t.Skipf("the helper isn't this build's (sudo -n %s protocol: %q, %v; want %d): run factory-agent host init --driver kata, then host check", katahelper.HelperPath, out, err, katahelper.Protocol)
	}
	image := os.Getenv("FACTORY_KATA_IMAGE")
	if image == "" {
		image = katahelper.ImageRepo + ":" + strings.TrimPrefix(version.Version, "v")
	}
	if out, err := helper("image-exists", image); err != nil {
		t.Skipf("no base image %s (kata-helper image-exists: %q, %v): host init's kataimage step builds it (P1 phase 9, PR 5)", image, out, err)
	}
	_, werr := helper("warm-exists")
	disk := 60
	if v := os.Getenv("FACTORY_KATA_DISK_GIB"); v != "" {
		var err error
		if disk, err = strconv.Atoi(v); err != nil {
			t.Fatalf("FACTORY_KATA_DISK_GIB: %v", err)
		}
	}
	return New(Config{Image: image, DiskGiB: disk, Warm: werr == nil})
}

// itWorkspace creates and starts a workspace whose name has tag, destroyed when the test ends.
// It may run in a goroutine of the test's: it reports, never stops the test.
func itWorkspace(t *testing.T, d *Driver, tag string) (string, workspace.Network, error) {
	name := fmt.Sprintf("ws-tsk_kit-%s-%d", tag, time.Now().UnixNano()%1e9)
	ctx := context.Background()
	t.Cleanup(func() {
		if err := d.Destroy(context.Background(), name); err != nil {
			t.Errorf("destroying %s: %v", name, err)
		}
	})
	if err := d.Create(ctx, name, workspace.Spec{CPU: 2, MemoryMiB: 2048}); err != nil {
		return name, workspace.Network{}, err
	}
	nw, err := d.Start(ctx, name)
	return name, nw, err
}

// sh runs cmd with bash in the workspace, for at most limit, and returns its output.
func sh(d *Driver, name string, limit time.Duration, cmd string) (string, error) {
	var out bytes.Buffer
	err := d.Exec(context.Background(), name, []string{"timeout", "--kill-after=5", strconv.Itoa(int(limit.Seconds())), "bash", "-c", cmd}, nil, &out, &out)
	return out.String(), err
}

func TestKataLifecycle(t *testing.T) {
	d := itDriver(t)
	ctx := context.Background()
	name, nw, err := itWorkspace(t, d, "life")
	if err != nil {
		t.Fatal(err)
	}
	if !nw.VM.IsValid() || nw.Gateway == nw.VM {
		t.Errorf("network %+v", nw)
	}
	if out, err := sh(d, name, 10*time.Second, "readlink "+AgentLink+"; test -x "+AgentDir+"/factory-agent"); err != nil || strings.TrimSpace(out) != AgentDir+"/factory-agent" {
		t.Errorf("the agent binary: %q, %v", out, err)
	}
	var ee *workspace.ExitError
	if _, err := sh(d, name, 10*time.Second, "exit 7"); !errors.As(err, &ee) || ee.Code != 7 {
		t.Errorf("exit 7: %v", err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, name, src, "/var/lib/factory/run"); err != nil {
		t.Fatal(err)
	}
	if out, err := sh(d, name, 10*time.Second, "mkdir -p /tmp/kit-out && cp /var/lib/factory/run/f /tmp/kit-out/g && stat -c %a /var/lib/factory/run/f"); err != nil || strings.TrimSpace(out) != "444" {
		t.Errorf("the put file: %q, %v", out, err)
	}
	var out bytes.Buffer
	if err := d.Exec(ctx, name, []string{"cat"}, strings.NewReader("stdin"), &out, io.Discard); err != nil || out.String() != "stdin" {
		t.Errorf("exec -i: %q, %v", out.String(), err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := d.Get(ctx, name, "/tmp/kit-out", dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "g")); err != nil || string(b) != "hello" {
		t.Errorf("got %q, %v", b, err)
	}
	if names, err := d.List(ctx); err != nil || !slices.Contains(names, name) {
		t.Errorf("List %v, %v", names, err)
	}
	if err := d.Stop(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, name); err != nil {
		t.Fatal(err)
	}
	if names, err := d.List(ctx); err != nil || slices.Contains(names, name) {
		t.Errorf("after Destroy: List %v, %v", names, err)
	}
}

// TestKataIsolation runs two workspaces at once, each behind a stand-in gateway (a plain
// forward proxy) on its slot's gateway address, and Q8's checks I01-I18 from inside each; N01:
// neither gateway saw a client but its own VM.
func TestKataIsolation(t *testing.T) {
	d := itDriver(t)
	port := 3128
	if p := os.Getenv("FACTORY_KATA_GATEWAY_PORT"); p != "" {
		var err error
		if port, err = strconv.Atoi(p); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	names, nws, errs := make([]string, 2), make([]workspace.Network, 2), make([]error, 2)
	for i := range 2 {
		wg.Go(func() { names[i], nws[i], errs[i] = itWorkspace(t, d, "iso"+strconv.Itoa(i)) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if nws[0].VM == nws[1].VM || nws[0].Gateway == nws[1].Gateway {
		t.Fatalf("two workspaces share addresses: %+v", nws)
	}
	proxies := []*standIn{startStandIn(t, nws[0].Gateway.String(), port), startStandIn(t, nws[1].Gateway.String(), port)}
	canary := startCanary(t)
	hostIP := outboundIP(t)
	for i := range 2 {
		own, other := nws[i], nws[1-i]
		dev := `$(ip -4 route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1)}')`
		// spoof adds addr (as root: exec runs as admin), runs cmd from it, and removes it. A failed
		// add exits spoofSetupFailed, which fails the check: only the firewall may refuse cmd.
		spoof := func(addr, cmd string) string {
			return fmt.Sprintf("sudo -n ip addr add %s/32 dev %s || exit %d; %s; rc=$?; sudo -n ip addr del %s/32 dev %s; exit $rc", addr, dev, spoofSetupFailed, cmd, addr, dev)
		}
		ownSpoof := own.Gateway.As4()
		ownSpoof[3] = 3
		px := func(gw string) string { return fmt.Sprintf("-x http://%s:%d", gw, port) }
		checks := []struct {
			id, cmd string
			ok      bool
		}{
			{"I01", "curl -fsS -o /dev/null " + px(own.Gateway.String()) + " http://example.com/", true},
			{"I02", "curl -fsS -o /dev/null " + px(own.Gateway.String()) + " https://example.com/", true},
			{"I03", fmt.Sprintf("curl -fsS -o /dev/null --noproxy '*' --connect-timeout 5 http://%s:%d/", own.Gateway, canary), false},
			{"I04", fmt.Sprintf("exec 3<>/dev/tcp/%s/22", own.Gateway), false},
			{"I05", fmt.Sprintf("curl -fsS -o /dev/null --noproxy '*' --connect-timeout 5 http://%s:%d/", hostIP, canary), false},
			{"I06", fmt.Sprintf("exec 3<>/dev/tcp/%s/%d", hostIP, port), false},
			{"I07", fmt.Sprintf("curl -fsS -o /dev/null --noproxy '*' --connect-timeout 5 http://172.17.0.1:%d/", canary), false},
			{"I08", "curl -fsS -o /dev/null --connect-timeout 5 " + px(other.Gateway.String()) + " http://example.com/", false},
			{"I09", fmt.Sprintf("exec 3<>/dev/tcp/%s/22", other.VM), false},
			{"I10", "curl -fsS -o /dev/null --noproxy '*' --connect-timeout 5 https://1.1.1.1/", false},
			{"I11", "curl -fsS -o /dev/null --noproxy '*' --connect-timeout 5 http://169.254.169.254/", false},
			{"I12", spoof(net.IP(ownSpoof[:]).String(), fmt.Sprintf("curl -fsS -o /dev/null --connect-timeout 5 --interface %s %s http://example.com/", net.IP(ownSpoof[:]), px(own.Gateway.String()))), false},
			{"I13", spoof(other.VM.String(), fmt.Sprintf("curl -fsS -o /dev/null --connect-timeout 5 --interface %s %s http://example.com/", other.VM, px(other.Gateway.String()))), false},
			{"I14", spoof(other.VM.String(), fmt.Sprintf("curl -fsS -o /dev/null --connect-timeout 5 --interface %s %s http://example.com/", other.VM, px(own.Gateway.String()))), false},
			{"I15", "curl -fsS -o /dev/null -6 --noproxy '*' --connect-timeout 5 'https://[2606:4700:4700::1111]/'", false},
			{"I16", "timeout 2 getent hosts github.com; rc=$?; test $rc -ne 0 -a $rc -ne 124", true}, // DNS fails, and fast
			{"I17", "test -e /dev/kvm", false},
			{"I18", "ls /dev/nvme* /dev/sd* 2>/dev/null | grep -q .", false},
		}
		for _, c := range checks {
			out, err := sh(d, names[i], 30*time.Second, c.cmd)
			var ee *workspace.ExitError
			if (err == nil) != c.ok || (errors.As(err, &ee) && ee.Code == spoofSetupFailed) {
				t.Errorf("ws%d %s: want ok=%v, got %v: %s\n%s", i, c.id, c.ok, err, c.cmd, out)
			}
		}
	}
	for i, p := range proxies {
		for _, c := range p.seen() {
			if c != nws[i].VM.String() {
				t.Errorf("N01: gateway %d saw client %s, not only its VM %s", i, c, nws[i].VM)
			}
		}
		if len(p.seen()) == 0 {
			t.Errorf("gateway %d saw no client: I01 and I02 didn't reach it", i)
		}
	}
}

// TestKataDestroyHung: Destroy removes a workspace whose systemd ignores the stop (PID 1
// stopped with SIGSTOP), within the helper's bounds.
func TestKataDestroyHung(t *testing.T) {
	d := itDriver(t)
	name, _, err := itWorkspace(t, d, "hung")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sh(d, name, 10*time.Second, "sudo -n kill -STOP 1"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := d.Destroy(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	t.Logf("Destroy took %s", time.Since(start).Round(time.Second))
	if names, err := d.List(context.Background()); err != nil || slices.Contains(names, name) {
		t.Errorf("after Destroy: List %v, %v", names, err)
	}
}

// standIn is a plain forward proxy (absolute-URI requests and CONNECT) that records each
// client's address.
type standIn struct {
	mu      sync.Mutex
	clients []string
}

func (s *standIn) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.clients...)
}

func startStandIn(t *testing.T, addr string, port int) *standIn {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("the stand-in gateway: %v", err)
	}
	s := &standIn{}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.mu.Lock()
		s.clients = append(s.clients, host)
		s.mu.Unlock()
		if r.Method == http.MethodConnect {
			up, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			c, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				_ = up.Close()
				return
			}
			_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			_, _ = io.Copy(c, up)
			_ = c.Close()
			return
		}
		r.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// startCanary listens on every host address (Q8's canary) and returns its port: a workspace
// reaching it is a hole.
func startCanary(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "canary\n")
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// outboundIP is the host's address on its default route (no packet is sent).
func outboundIP(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
