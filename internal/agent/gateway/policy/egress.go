package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// The egress modes (#100's environment.egress.mode).
const (
	ModeDefault       = "default"         // every host passes the host check
	ModeDenyByDefault = "deny-by-default" // only AllowHosts pass
)

// Egress is a task's egress policy: the host check's mode and allow list, and the address
// check's extra denied prefixes and exact exceptions. The zero Egress is ModeDefault with only
// the default deny table.
type Egress struct {
	// Mode is ModeDefault, ModeDenyByDefault, or "" for ModeDefault. Any other value fails
	// closed: the host check then allows only AllowHosts.
	Mode string
	// AllowHosts are #100's environment.egress.allow entries, which v1alpha1.ValidateHost
	// accepts: a name or *.domain, either with :port.
	AllowHosts []string
	// Deny are prefixes denied on top of the default deny table and the agent host's own
	// prefixes (class extra).
	Deny []netip.Prefix
	// OpenAddrs are exact exceptions to every deny rule except a zone.
	OpenAddrs []netip.AddrPort
}

// ErrEgress is wrapped by every *EgressError.
var ErrEgress = errors.New("invalid egress policy")

// EgressError is a field of an Egress that Validate refuses. It never carries the value.
type EgressError struct {
	Field string // Mode, AllowHosts[i], Deny[i] or OpenAddrs[i]
	// Err is a v1alpha1.ErrHost* sentinel for an AllowHosts entry, else nil.
	Err error
}

func (e *EgressError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("policy: egress: %s: %v", e.Field, e.Err)
	}
	return "policy: egress: " + e.Field + " is invalid"
}

// Is matches ErrEgress.
func (e *EgressError) Is(target error) bool { return target == ErrEgress }

func (e *EgressError) Unwrap() error { return e.Err }

// Validate checks the mode, each AllowHosts entry (v1alpha1.ValidateHost), each Deny prefix and
// each OpenAddrs entry (a valid address with a non-zero port), returning an *EgressError.
func (e Egress) Validate() error {
	if e.Mode != "" && e.Mode != ModeDefault && e.Mode != ModeDenyByDefault {
		return &EgressError{Field: "Mode"}
	}
	for i, h := range e.AllowHosts {
		if err := v1alpha1.ValidateHost(h); err != nil {
			field := fmt.Sprintf("AllowHosts[%d]", i)
			if he, ok := errors.AsType[*v1alpha1.HostError](err); ok {
				return &EgressError{Field: field, Err: he.Err}
			}
			return &EgressError{Field: field}
		}
	}
	for i, p := range e.Deny {
		if !p.IsValid() {
			return &EgressError{Field: fmt.Sprintf("Deny[%d]", i)}
		}
	}
	for i, ap := range e.OpenAddrs {
		if !ap.IsValid() || ap.Port() == 0 || ap.Addr().Zone() != "" {
			return &EgressError{Field: fmt.Sprintf("OpenAddrs[%d]", i)}
		}
	}
	return nil
}

// AllowsHost is the host check for an ordinary host. ModeDefault allows every target. Otherwise
// the target must match an AllowHosts entry: an exact entry matches that name only,
// *.example.com any name under example.com at any depth but not example.com itself, an entry
// with a port only that port and one without every port. An IP literal never matches.
func (e Egress) AllowsHost(t Target) bool {
	if e.Mode == "" || e.Mode == ModeDefault {
		return true
	}
	if t.IsIP() || t.Host == "" {
		return false
	}
	host := NormalizeHost(t.Host)
	for _, entry := range e.AllowHosts {
		if allowMatches(entry, host, t.Port) {
			return true
		}
	}
	return false
}

func allowMatches(entry, host string, port uint16) bool {
	name := entry
	if i := strings.LastIndexByte(entry, ':'); i >= 0 {
		p, ok := targetPort(entry[i+1:], true, 0)
		if !ok || p != port {
			return false
		}
		name = entry[:i]
	}
	name = NormalizeHost(name)
	if suffix, ok := strings.CutPrefix(name, "*."); ok {
		sub, ok := strings.CutSuffix(host, "."+suffix)
		return ok && sub != "" && suffix != ""
	}
	return name != "" && host == name
}

// DenyClass names why an address is denied: the egress-denied body says it.
type DenyClass string

// The deny classes.
const (
	DenyUnspecified   DenyClass = "unspecified"
	DenyLoopback      DenyClass = "loopback"
	DenyPrivate       DenyClass = "private"
	DenyCGNAT         DenyClass = "cgnat"
	DenyLinkLocal     DenyClass = "link-local"
	DenyULA           DenyClass = "ula"
	DenyMulticast     DenyClass = "multicast"
	DenyReserved      DenyClass = "reserved"
	DenyDocumentation DenyClass = "documentation"
	DenyAgentHost     DenyClass = "agent-host" // the agent host's own addresses and on-link prefixes
	DenyExtra         DenyClass = "extra"      // Egress.Deny
)

// DenyClasses returns every deny class.
func DenyClasses() []DenyClass {
	return []DenyClass{
		DenyUnspecified, DenyLoopback, DenyPrivate, DenyCGNAT, DenyLinkLocal, DenyULA,
		DenyMulticast, DenyReserved, DenyDocumentation, DenyAgentHost, DenyExtra,
	}
}

// Valid reports whether c is one of the DenyClasses.
func (c DenyClass) Valid() bool {
	switch c {
	case DenyUnspecified, DenyLoopback, DenyPrivate, DenyCGNAT, DenyLinkLocal, DenyULA,
		DenyMulticast, DenyReserved, DenyDocumentation, DenyAgentHost, DenyExtra:
		return true
	}
	return false
}

// DenyRange is one row of the default deny table.
type DenyRange struct {
	Prefix netip.Prefix
	Class  DenyClass
}

var defaultDeny = func() []DenyRange {
	rows := []struct {
		class    DenyClass
		prefixes []string
	}{
		{DenyUnspecified, []string{"0.0.0.0/8", "::/128"}},
		{DenyLoopback, []string{"127.0.0.0/8", "::1/128"}},
		{DenyPrivate, []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}},
		{DenyCGNAT, []string{"100.64.0.0/10"}},
		{DenyLinkLocal, []string{"169.254.0.0/16", "fe80::/10"}},
		{DenyULA, []string{"fc00::/7"}},
		{DenyMulticast, []string{"224.0.0.0/4", "ff00::/8"}},
		{DenyDocumentation, []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"}},
		{DenyReserved, []string{
			"192.0.0.0/24", "192.88.99.0/24", "198.18.0.0/15", "240.0.0.0/4",
			"::/96", "100::/64", "2001::/32", "64:ff9b:1::/48", "fec0::/10",
		}},
	}
	var out []DenyRange
	for _, r := range rows {
		for _, p := range r.prefixes {
			out = append(out, DenyRange{Prefix: netip.MustParsePrefix(p), Class: r.class})
		}
	}
	return out
}()

// DefaultDeny returns the default deny table. An address takes the class of the most specific
// range that contains it (::1 is loopback, not reserved).
func DefaultDeny() []DenyRange { return append([]DenyRange(nil), defaultDeny...) }

var (
	nat64   = netip.MustParsePrefix("64:ff9b::/96") // well-known NAT64: the IPv4 address in the last 32 bits
	sixToV4 = netip.MustParsePrefix("2002::/16")    // 6to4: the IPv4 address in bits 16-47
)

// CheckAddr is the address check for one destination: "" when it may be dialled, else the
// class that denies it. hostPrefixes are the agent host's own addresses and on-link prefixes,
// which the caller reads (class agent-host).
//
// In order: an invalid address is unspecified; an address with a zone is always denied (its
// table class, else link-local); an IPv4-mapped address is unmapped; an exact OpenAddrs match is
// allowed; then the default table's most specific range, a NAT64 (64:ff9b::/96) or 6to4
// (2002::/16) address whose embedded IPv4 address is denied (that address's class),
// hostPrefixes (agent-host) and Egress.Deny (extra).
func (e Egress) CheckAddr(ap netip.AddrPort, hostPrefixes []netip.Prefix) DenyClass {
	a := ap.Addr()
	if !a.IsValid() {
		return DenyUnspecified
	}
	if a.Zone() != "" { // before Unmap, which drops the zone
		if c := tableClass(a.WithZone("").Unmap()); c != "" {
			return c
		}
		return DenyLinkLocal
	}
	a = a.Unmap()
	for _, open := range e.OpenAddrs {
		if open.Addr().Zone() == "" && open.Addr().Unmap() == a && open.Port() == ap.Port() {
			return ""
		}
	}
	if c := e.denied(a, hostPrefixes); c != "" {
		return c
	}
	if v4, ok := embeddedV4(a); ok {
		return e.denied(v4, hostPrefixes)
	}
	return ""
}

// CheckResolved is the address check for every address a destination resolved to, on one port:
// CodeResolve when there is none, CodeEgressDenied with the first denied address's class when
// any is denied (a name with mixed public and private answers is refused, not filtered), else
// "" and "".
func (e Egress) CheckResolved(addrs []netip.Addr, port uint16, hostPrefixes []netip.Prefix) (Code, DenyClass) {
	if len(addrs) == 0 {
		return CodeResolve, ""
	}
	for _, a := range addrs {
		if c := e.CheckAddr(netip.AddrPortFrom(a, port), hostPrefixes); c != "" {
			return CodeEgressDenied, c
		}
	}
	return "", ""
}

// denied checks an unmapped, zone-free address against the table, hostPrefixes and e.Deny.
func (e Egress) denied(a netip.Addr, hostPrefixes []netip.Prefix) DenyClass {
	if c := tableClass(a); c != "" {
		return c
	}
	if containsAny(hostPrefixes, a) {
		return DenyAgentHost
	}
	if containsAny(e.Deny, a) {
		return DenyExtra
	}
	return ""
}

// tableClass is the class of the most specific default range containing a, or "".
func tableClass(a netip.Addr) DenyClass {
	best, bits := DenyClass(""), -1
	for _, r := range defaultDeny {
		if r.Prefix.Bits() > bits && r.Prefix.Contains(a) {
			best, bits = r.Class, r.Prefix.Bits()
		}
	}
	return best
}

func containsAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if unmapPrefix(p).Contains(a) {
			return true
		}
	}
	return false
}

// unmapPrefix turns an IPv4-mapped prefix (::ffff:a.b.c.d/n, n >= 96) into its IPv4 prefix, so
// it contains the unmapped addresses the check compares.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	if p.IsValid() && p.Addr().Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96).Masked()
	}
	return p
}

// embeddedV4 is the IPv4 address a NAT64 or 6to4 address embeds.
func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case !a.Is6():
		return netip.Addr{}, false
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToV4.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}
