package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/gateway/policy"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// hostAddrsMaxAge is how long the agent host's interface addresses are reused before a re-read:
// a new VM bridge appears with each workspace.
const hostAddrsMaxAge = 30 * time.Second

// egressError is a refusal the egress dialer decided; it never holds an address.
type egressError struct {
	code  policy.Code
	class policy.DenyClass // egress-denied only
	err   error            // the resolver's or dialer's, logged at debug only
}

func (e *egressError) Error() string {
	if e.class != "" {
		return fmt.Sprintf("gateway: egress: %s (%s)", e.code, e.class)
	}
	return "gateway: egress: " + string(e.code)
}

func (e *egressError) Unwrap() error { return e.err }

// dialEgress is the one dialer for every destination the VM names: addr (host:port, as the
// target was checked) is resolved once with a name's every address checked (any denied one
// refuses it), then the checked addresses are dialled in order as literals, so nothing is
// resolved again. controlDial checks each address once more before its connect. The connection
// is tracked for Close.
func (g *Gateway) dialEgress(ctx context.Context, _, addr string) (net.Conn, error) {
	t, ok := policy.ParseConnectTarget(addr)
	if !ok {
		return nil, &egressError{code: policy.CodeBadTarget}
	}
	addrs := []netip.Addr{t.Addr}
	if !t.IsIP() {
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		var err error
		addrs, err = g.cfg.Resolver.LookupNetIP(rctx, "ip", t.Host)
		cancel()
		if err != nil {
			return nil, &egressError{code: policy.CodeResolve, err: err}
		}
	}
	if code, class := g.cfg.Egress.CheckResolved(addrs, t.Port, g.ifaces.prefixes()); code != "" {
		return nil, &egressError{code: code, class: class}
	}
	var last error
	for _, a := range addrs {
		c, err := g.dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), t.Port).String())
		if err == nil {
			if tc := g.track(c); tc != nil {
				return tc, nil
			}
			return nil, &egressError{code: policy.CodeClosing}
		}
		if ee, ok := errors.AsType[*egressError](err); ok {
			return nil, ee
		}
		last = err
	}
	return nil, fmt.Errorf("gateway: dial: %w", last)
}

// controlDial checks the address a connect is about to reach, in case anything but dialEgress's
// checked literals is ever dialled.
func (g *Gateway) controlDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &egressError{code: policy.CodeEgressDenied, class: policy.DenyUnspecified}
	}
	if c := g.cfg.Egress.CheckAddr(ap, g.ifaces.prefixes()); c != "" {
		return &egressError{code: policy.CodeEgressDenied, class: c}
	}
	return nil
}

// refusalOf maps an upstream error to the refusal the client gets: closing once the gateway is,
// the egress dialer's own, timeout for a timeout, else upstream. The error itself is logged
// at debug only: it can hold addresses.
func (g *Gateway) refusalOf(err error) (policy.Code, policy.DenyClass) {
	g.log.Debug("gateway: upstream error", "err", err)
	if g.isClosed() {
		return policy.CodeClosing, ""
	}
	if ee, ok := errors.AsType[*egressError](err); ok {
		return ee.code, ee.class
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return policy.CodeTimeout, ""
	}
	return policy.CodeUpstream, ""
}

// hostAddrs caches the agent host's interface addresses and their on-link prefixes.
type hostAddrs struct {
	read func() ([]net.Addr, error)
	clk  clock.Clock
	log  *slog.Logger

	mu     sync.Mutex
	at     time.Time
	loaded bool
	own    map[netip.Addr]bool
	pfx    []netip.Prefix
}

// addrs is the host's own addresses, re-read when force is set or the cache is older than
// hostAddrsMaxAge. A failed re-read keeps the last addresses (logged); a failed first read is
// an error.
func (h *hostAddrs) addrs(force bool) (map[netip.Addr]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clk.Now()
	if !force && h.loaded && now.Sub(h.at) < hostAddrsMaxAge {
		return h.own, nil
	}
	as, err := h.read()
	if err != nil {
		if h.loaded {
			h.log.Warn("gateway: re-reading the agent host's addresses failed; keeping the last", "err", err)
			return h.own, nil
		}
		return nil, fmt.Errorf("gateway: interface addresses: %w", err)
	}
	h.own, h.pfx = parseIfaceAddrs(as)
	h.at, h.loaded = now, true
	return h.own, nil
}

// prefixes is the host's addresses and on-link prefixes for the address check.
func (h *hostAddrs) prefixes() []netip.Prefix {
	_, _ = h.addrs(false) // a failed first read leaves none; Serve reads first and fails then
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pfx
}

// parseIfaceAddrs turns net.InterfaceAddrs' answers into the addresses (unmapped, without a
// zone) and their prefixes: each on-link prefix, and the address itself as a single-address one.
func parseIfaceAddrs(as []net.Addr) (map[netip.Addr]bool, []netip.Prefix) {
	own := map[netip.Addr]bool{}
	var pfx []netip.Prefix
	for _, a := range as {
		var ip net.IP
		var mask net.IPMask
		switch v := a.(type) {
		case *net.IPNet:
			ip, mask = v.IP, v.Mask
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		own[addr] = true
		pfx = append(pfx, netip.PrefixFrom(addr, addr.BitLen()))
		if ones, bits := mask.Size(); bits != 0 {
			if bits == 128 && addr.Is4() {
				ones -= 96
			}
			if p, err := addr.Prefix(ones); err == nil {
				pfx = append(pfx, p)
			}
		}
	}
	return own, pfx
}
