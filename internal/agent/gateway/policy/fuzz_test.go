package policy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func FuzzParseTarget(f *testing.F) {
	for _, s := range []string{
		"github.com:443", "GitHub.COM.:443", "1.2.3.4:80", "[2606:4700::1]:443", "[fe80::1%25en0]:443",
		"[::ffff:127.0.0.1]:443", "example.com", "::1:443", "[::1", "a..b:1", "example.com:0443", "Key.com:443",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, parse := range []func(string) (Target, bool){
			ParseConnectTarget,
			func(s string) (Target, bool) { return ParseHostPort(s, 80) },
		} {
			got, ok := parse(s)
			if !ok {
				continue
			}
			if got.Port == 0 || got.Host == "" {
				t.Fatalf("%q parsed to %+v", s, got)
			}
			if !got.IsIP() && (got.Host != NormalizeHost(got.Host) || !validName(got.Host)) {
				t.Fatalf("%q parsed to a name that isn't normalised: %q", s, got.Host)
			}
			again, ok := ParseConnectTarget(got.String())
			if !ok || again != got {
				t.Fatalf("%q: %+v formats as %q, which parses to %+v (%v)", s, got, got.String(), again, ok)
			}
			if c := ClassOf(got.Host); c != ClassOf(strings.ToUpper(got.Host)+".") || got.IsIP() && c != Ordinary {
				t.Fatalf("%q: the class %s of %q depends on case or a trailing dot", s, c, got.Host)
			}
		}
	})
}

func FuzzFilterCookies(f *testing.F) {
	for _, s := range []string{
		"__oailb=lb1; _cfuvid=u1; session=s3cr3t", "cf_chl_x=1", "__cf_bm =a", "__cf_bm=a\x01", ";;", `__cf_bm="a;b"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := FilterCookies([]string{s})
		if got == nil {
			return
		}
		if len(got) != 1 || got[0] == "" {
			t.Fatalf("FilterCookies(%q) = %q", s, got)
		}
		for part := range strings.SplitSeq(got[0], "; ") {
			name, value, ok := strings.Cut(part, "=")
			if !ok || !IsInfraCookie(name) || hasCTL(value) || strings.Contains(value, ";") {
				t.Fatalf("FilterCookies(%q) kept %q", s, part)
			}
		}
		if again := FilterCookies(got); !slices.Equal(again, got) {
			t.Fatalf("FilterCookies is not idempotent on %q: %q then %q", s, got, again)
		}
	})
}

func FuzzCheckAddr(f *testing.F) {
	f.Add([]byte{127, 0, 0, 1}, uint16(443))
	f.Add([]byte{0, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 1}, uint16(80))
	f.Add([]byte{0x20, 0x02, 192, 168, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, uint16(80))
	f.Fuzz(func(t *testing.T, b []byte, port uint16) {
		a, ok := netip.AddrFromSlice(b)
		if !ok {
			return
		}
		var e Egress
		got := e.CheckAddr(netip.AddrPortFrom(a, port), nil)
		if got != "" && !got.Valid() {
			t.Fatalf("CheckAddr(%s) = %q, not a DenyClass", a, got)
		}
		if a.Is4() {
			mapped := netip.AddrFrom16(a.As16())
			if m := e.CheckAddr(netip.AddrPortFrom(mapped, port), nil); m != got {
				t.Fatalf("CheckAddr(%s) = %q but CheckAddr(%s) = %q", a, got, mapped, m)
			}
			if got != "" {
				if r, ok := CodeEgressDenied.Response(got); !ok || strings.Contains(r.Body, a.String()) {
					t.Fatalf("egress-denied body for %s: %q, %v", a, r.Body, ok)
				}
			}
		}
		for _, r := range DefaultDeny() {
			if r.Prefix.Contains(a.Unmap()) && got == "" {
				t.Fatalf("CheckAddr(%s) allowed an address in %s", a, r.Prefix)
			}
		}
	})
}
