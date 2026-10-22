package policy

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// TestCheckAddrDenyTable has a row per range of the default deny table (TestDenyTableCovered
// checks that), plus the boundaries just outside them.
func TestCheckAddrDenyTable(t *testing.T) {
	tests := []struct {
		addr string
		want DenyClass
	}{
		{"0.1.2.3", DenyUnspecified},
		{"0.0.0.0", DenyUnspecified},
		{"::", DenyUnspecified},
		{"127.0.0.1", DenyLoopback},
		{"127.255.255.254", DenyLoopback},
		{"::1", DenyLoopback},
		{"10.1.2.3", DenyPrivate},
		{"172.16.0.1", DenyPrivate},
		{"172.31.255.255", DenyPrivate},
		{"192.168.1.1", DenyPrivate},
		{"100.64.0.1", DenyCGNAT},
		{"100.100.100.100", DenyCGNAT}, // Tailscale's resolver
		{"100.127.255.254", DenyCGNAT},
		{"169.254.169.254", DenyLinkLocal}, // the metadata address
		{"fe80::1", DenyLinkLocal},
		{"febf:ffff::1", DenyLinkLocal},
		{"fc00::1", DenyULA},
		{"fd12:3456::1", DenyULA},
		{"224.0.0.1", DenyMulticast},
		{"239.255.255.250", DenyMulticast},
		{"ff02::1", DenyMulticast},
		{"192.0.2.1", DenyDocumentation},
		{"198.51.100.1", DenyDocumentation},
		{"203.0.113.1", DenyDocumentation},
		{"2001:db8::1", DenyDocumentation},
		{"192.0.0.1", DenyReserved},
		{"192.88.99.1", DenyReserved},
		{"198.18.0.1", DenyReserved},
		{"198.19.255.255", DenyReserved},
		{"240.0.0.1", DenyReserved},
		{"255.255.255.255", DenyReserved},
		{"::808:808", DenyReserved}, // IPv4-compatible ::8.8.8.8
		{"100::1", DenyReserved},
		{"2001::1", DenyReserved}, // Teredo
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", DenyReserved},
		{"64:ff9b:1::1", DenyReserved},
		{"fec0::1", DenyReserved},
		// Just outside the ranges.
		{"1.0.0.1", ""},
		{"11.0.0.1", ""},
		{"126.255.255.255", ""},
		{"128.0.0.1", ""},
		{"172.15.255.255", ""},
		{"172.32.0.1", ""},
		{"100.63.255.255", ""},
		{"100.128.0.1", ""},
		{"169.253.255.255", ""},
		{"192.167.255.255", ""},
		{"192.169.0.1", ""},
		{"198.17.255.255", ""},
		{"198.20.0.1", ""},
		{"223.255.255.255", ""},
		{"2001:db9::1", ""},
		{"2001:1::1", ""},
		{"100::1:0:0:0:1", ""}, // outside 100::/64
		{"fe00::1", ""},
		{"8.8.8.8", ""},
		{"2606:4700:4700::1111", ""},
	}
	var e Egress
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			ap := netip.AddrPortFrom(netip.MustParseAddr(tt.addr), 443)
			if got := e.CheckAddr(ap, nil); got != tt.want {
				t.Errorf("CheckAddr(%s) = %q, want %q", ap, got, tt.want)
			}
		})
	}
}

// TestDenyTableCovered checks that each default range has a row in TestCheckAddrDenyTable's
// shape: an address inside it that takes its class.
func TestDenyTableCovered(t *testing.T) {
	inside := map[string]string{
		"0.0.0.0/8": "0.1.2.3", "::/128": "::", "127.0.0.0/8": "127.0.0.1", "::1/128": "::1",
		"10.0.0.0/8": "10.1.2.3", "172.16.0.0/12": "172.16.0.1", "192.168.0.0/16": "192.168.1.1",
		"100.64.0.0/10": "100.64.0.1", "169.254.0.0/16": "169.254.169.254", "fe80::/10": "fe80::1",
		"fc00::/7": "fd12:3456::1", "224.0.0.0/4": "224.0.0.1", "ff00::/8": "ff02::1",
		"192.0.2.0/24": "192.0.2.1", "198.51.100.0/24": "198.51.100.1", "203.0.113.0/24": "203.0.113.1",
		"2001:db8::/32": "2001:db8::1", "192.0.0.0/24": "192.0.0.1", "192.88.99.0/24": "192.88.99.1",
		"198.18.0.0/15": "198.18.0.1", "240.0.0.0/4": "240.0.0.1", "::/96": "::808:808",
		"100::/64": "100::1", "2001::/32": "2001::1", "64:ff9b:1::/48": "64:ff9b:1::1", "fec0::/10": "fec0::1",
	}
	table := DefaultDeny()
	if len(table) != len(inside) {
		t.Errorf("DefaultDeny has %d ranges, the test %d", len(table), len(inside))
	}
	var e Egress
	for _, r := range table {
		a, ok := inside[r.Prefix.String()]
		if !ok {
			t.Errorf("no row for %s", r.Prefix)
			continue
		}
		addr := netip.MustParseAddr(a)
		if !r.Prefix.Contains(addr) {
			t.Errorf("%s is not in %s", addr, r.Prefix)
		}
		if got := e.CheckAddr(netip.AddrPortFrom(addr, 80), nil); got != r.Class {
			t.Errorf("%s (in %s) = %q, want %q", addr, r.Prefix, got, r.Class)
		}
		if !r.Class.Valid() {
			t.Errorf("%s has class %q, not a DenyClass", r.Prefix, r.Class)
		}
	}
	table[0].Class = "changed"
	if DefaultDeny()[0].Class == "changed" {
		t.Error("DefaultDeny returned the table itself, not a copy")
	}
}

func TestCheckAddrInputs(t *testing.T) {
	e := Egress{
		Deny: []netip.Prefix{netip.MustParsePrefix("93.184.0.0/16"), netip.MustParsePrefix("2a02:26f0::/32")},
		OpenAddrs: []netip.AddrPort{
			netip.MustParseAddrPort("127.0.0.1:8443"),
			netip.MustParseAddrPort("[::ffff:10.0.0.5]:9000"),
			netip.MustParseAddrPort("[2a01:4f8:c0c:1234::5]:443"),
		},
	}
	hostPrefixes := []netip.Prefix{
		netip.MustParsePrefix("81.2.69.0/24"),
		netip.MustParsePrefix("2a01:4f8:c0c:1234::/64"), // a public IPv6 address on the host
		netip.MustParsePrefix("::ffff:5.9.0.0/112"),     // IPv4-mapped: 5.9.0.0/16
		netip.MustParsePrefix("192.168.64.0/24"),        // a VM bridge
	}
	tests := []struct {
		name string
		ap   netip.AddrPort
		want DenyClass
	}{
		{"agent host IPv4", ap("81.2.69.160", 443), DenyAgentHost},
		{"agent host IPv6 neighbour", ap("2a01:4f8:c0c:1234::99", 443), DenyAgentHost},
		{"agent host mapped prefix", ap("5.9.1.2", 443), DenyAgentHost},
		{"agent host mapped address", ap("::ffff:81.2.69.1", 443), DenyAgentHost},
		{"the table wins over the host's prefixes", ap("192.168.64.1", 443), DenyPrivate},
		{"extra IPv4", ap("93.184.216.34", 80), DenyExtra},
		{"extra IPv6", ap("2a02:26f0::1", 80), DenyExtra},
		{"extra through NAT64", ap("64:ff9b::5db8:d822", 80), DenyExtra},
		{"agent host through 6to4", ap("2002:5102:45a0::1", 80), DenyAgentHost},
		{"open address", ap("127.0.0.1", 8443), ""},
		{"open address, another port", ap("127.0.0.1", 8444), DenyLoopback},
		{"open address, mapped", ap("::ffff:127.0.0.1", 8443), ""},
		{"open mapped entry", ap("10.0.0.5", 9000), ""},
		{"open mapped entry, another port", ap("10.0.0.5", 9001), DenyPrivate},
		{"open address beats the host's prefixes", ap("2a01:4f8:c0c:1234::5", 443), ""},
		{"NAT64 of an open address is not open", ap("64:ff9b::7f00:1", 8443), DenyLoopback},
		{"public", ap("8.8.8.8", 443), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.CheckAddr(tt.ap, hostPrefixes); got != tt.want {
				t.Errorf("CheckAddr(%s) = %q, want %q", tt.ap, got, tt.want)
			}
		})
	}
}

func TestCheckAddrEmbeddingAndZones(t *testing.T) {
	e := Egress{OpenAddrs: []netip.AddrPort{netip.MustParseAddrPort("[fe80::1]:443")}}
	tests := []struct {
		name string
		ap   netip.AddrPort
		want DenyClass
	}{
		{"mapped private", ap("::ffff:10.0.0.1", 443), DenyPrivate},
		{"mapped loopback", ap("::ffff:127.0.0.1", 443), DenyLoopback},
		{"mapped metadata", ap("::ffff:169.254.169.254", 80), DenyLinkLocal},
		{"mapped public", ap("::ffff:8.8.8.8", 443), ""},
		{"NAT64 loopback", ap("64:ff9b::7f00:1", 443), DenyLoopback},
		{"NAT64 metadata", ap("64:ff9b::a9fe:a9fe", 80), DenyLinkLocal},
		{"NAT64 private", ap("64:ff9b::c0a8:101", 443), DenyPrivate},
		{"NAT64 public", ap("64:ff9b::808:808", 443), ""},
		{"6to4 private", ap("2002:c0a8:101::1", 443), DenyPrivate},
		{"6to4 loopback", ap("2002:7f00:1::1", 443), DenyLoopback},
		{"6to4 public", ap("2002:808:808::1", 443), ""},
		{"zone link-local, even when open", netip.AddrPortFrom(netip.MustParseAddr("fe80::1%en0"), 443), DenyLinkLocal},
		{"zone loopback", netip.AddrPortFrom(netip.MustParseAddr("::1%lo0"), 443), DenyLoopback},
		{"zone on a public address", netip.AddrPortFrom(netip.MustParseAddr("2606:4700::1%en0"), 443), DenyLinkLocal},
		{"open link-local without a zone", ap("fe80::1", 443), ""},
		{"invalid address", netip.AddrPort{}, DenyUnspecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.CheckAddr(tt.ap, nil); got != tt.want {
				t.Errorf("CheckAddr(%s) = %q, want %q", tt.ap, got, tt.want)
			}
		})
	}
}

func TestCheckResolved(t *testing.T) {
	addrs := func(ss ...string) []netip.Addr {
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out
	}
	tests := []struct {
		name      string
		addrs     []netip.Addr
		wantCode  Code
		wantClass DenyClass
	}{
		{"no answer", nil, CodeResolve, ""},
		{"public answers", addrs("8.8.8.8", "2606:4700:4700::1111"), "", ""},
		{"mixed public and private", addrs("8.8.8.8", "10.0.0.1"), CodeEgressDenied, DenyPrivate},
		{"the first denied class", addrs("169.254.169.254", "127.0.0.1"), CodeEgressDenied, DenyLinkLocal},
		{"mapped public", addrs("::ffff:8.8.8.8"), "", ""},
	}
	var e Egress
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, class := e.CheckResolved(tt.addrs, 443, nil)
			if code != tt.wantCode || class != tt.wantClass {
				t.Errorf("CheckResolved = %q, %q; want %q, %q", code, class, tt.wantCode, tt.wantClass)
			}
		})
	}
}

func TestAllowsHost(t *testing.T) {
	allow := []string{"example.com", "*.example.org", "api.example.net:8443", "*.ports.example.io:9000"}
	deny := Egress{Mode: ModeDenyByDefault, AllowHosts: allow}
	tests := []struct {
		name   string
		e      Egress
		target string
		want   bool
	}{
		{"exact", deny, "example.com:443", true},
		{"exact, normalised", deny, "EXAMPLE.COM.:443", true},
		{"exact, any port", deny, "example.com:8080", true},
		{"exact is not a suffix", deny, "www.example.com:443", false},
		{"exact is not a substring", deny, "xexample.com:443", false},
		{"exact is not a prefix", deny, "example.com.evil.io:443", false},
		{"wildcard, one label", deny, "a.example.org:443", true},
		{"wildcard, two labels", deny, "a.b.example.org:443", true},
		{"wildcard, normalised", deny, "A.Example.ORG.:443", true},
		{"wildcard refuses the apex", deny, "example.org:443", false},
		{"wildcard needs a label boundary", deny, "aexample.org:443", false},
		{"port entry, its port", deny, "api.example.net:8443", true},
		{"port entry, another port", deny, "api.example.net:443", false},
		{"wildcard port entry", deny, "x.ports.example.io:9000", true},
		{"wildcard port entry, another port", deny, "x.ports.example.io:9001", false},
		{"wildcard port entry, the apex", deny, "ports.example.io:9000", false},
		{"IPv4 literal", deny, "93.184.216.34:443", false},
		{"IPv6 literal", deny, "[2606:4700::1]:443", false},
		{"unlisted", deny, "other.io:443", false},
		{"deny-by-default, empty list", Egress{Mode: ModeDenyByDefault}, "example.com:443", false},
		{"default", Egress{Mode: ModeDefault}, "other.io:443", true},
		{"default, IP literal", Egress{Mode: ModeDefault}, "93.184.216.34:443", true},
		{"zero mode is default", Egress{}, "other.io:443", true},
		{"unknown mode fails closed", Egress{Mode: "open", AllowHosts: allow}, "other.io:443", false},
		{"unknown mode keeps the list", Egress{Mode: "open", AllowHosts: allow}, "example.com:443", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := ParseConnectTarget(tt.target)
			if !ok {
				t.Fatalf("ParseConnectTarget(%q) failed", tt.target)
			}
			if got := tt.e.AllowsHost(target); got != tt.want {
				t.Errorf("AllowsHost(%s) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
	if (Egress{Mode: ModeDenyByDefault, AllowHosts: allow}).AllowsHost(Target{}) {
		t.Error("AllowsHost(zero Target) = true")
	}
}

func TestEgressValidate(t *testing.T) {
	tests := []struct {
		name      string
		e         Egress
		wantField string
		wantErr   error
	}{
		{name: "zero", e: Egress{}},
		{name: "full", e: Egress{
			Mode: ModeDenyByDefault, AllowHosts: []string{"example.com", "*.example.org:8443"},
			Deny:      []netip.Prefix{netip.MustParsePrefix("5.6.0.0/16")},
			OpenAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:5000")},
		}},
		{name: "mode", e: Egress{Mode: "Deny-By-Default"}, wantField: "Mode"},
		{name: "upper case host", e: Egress{AllowHosts: []string{"example.com", "Example.com"}},
			wantField: "AllowHosts[1]", wantErr: v1alpha1.ErrHostSyntax},
		{name: "IP literal", e: Egress{AllowHosts: []string{"10.0.0.1"}}, wantField: "AllowHosts[0]", wantErr: v1alpha1.ErrHostIP},
		{name: "localhost", e: Egress{AllowHosts: []string{"localhost:80"}}, wantField: "AllowHosts[0]", wantErr: v1alpha1.ErrHostLocalhost},
		{name: "wildcard on a TLD", e: Egress{AllowHosts: []string{"*.com"}}, wantField: "AllowHosts[0]", wantErr: v1alpha1.ErrHostWildcard},
		{name: "deny prefix", e: Egress{Deny: []netip.Prefix{{}}}, wantField: "Deny[0]"},
		{name: "open address port", e: Egress{OpenAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}}, wantField: "OpenAddrs[0]"},
		{name: "open address zone", e: Egress{OpenAddrs: []netip.AddrPort{netip.MustParseAddrPort("[fe80::1%en0]:443")}}, wantField: "OpenAddrs[0]"},
		{name: "open address invalid", e: Egress{OpenAddrs: []netip.AddrPort{{}}}, wantField: "OpenAddrs[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.e.Validate()
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			ee, ok := errors.AsType[*EgressError](err)
			if !ok {
				t.Fatalf("Validate() = %v, want an *EgressError", err)
			}
			if ee.Field != tt.wantField || !errors.Is(err, ErrEgress) {
				t.Errorf("Validate() = %v, want field %s wrapping ErrEgress", err, tt.wantField)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
			for _, v := range tt.e.AllowHosts {
				if strings.Contains(err.Error(), v) {
					t.Errorf("Validate() = %q names the value %q", err, v)
				}
			}
		})
	}
}

func ap(addr string, port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(addr), port)
}
