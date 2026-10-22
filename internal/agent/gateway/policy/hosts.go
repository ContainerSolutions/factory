// Package policy holds the workspace gateway's decisions as data (gateway.md): the host
// classes and what a CONNECT or a plain HTTP request to each gets, the egress check (the
// task's mode and allow list, the deny table, the open addresses), the refusal codes and
// bodies, and the cookie filter of the model hosts.
//
// It is pure, like internal/rules: no network, no I/O, no clock and no goroutines. The
// gateway package resolves names, reads the agent host's interface addresses and dials; this
// package only decides.
package policy

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The hosts the gateway serves itself. Names are compared lower-cased and without a trailing
// dot, exactly: a name under one of them (downloads.claude.ai) is an ordinary host.
const (
	// GitHost is the git host: the only host whose git credentials the gateway uses, and the
	// one the CA's name constraints list.
	GitHost = "github.com"
	// GitHubAPIHost gets the task token on GET and HEAD.
	GitHubAPIHost = "api.github.com"
	// ChatGPTHost gets the ChatGPT login (codex).
	ChatGPTHost = "chatgpt.com"
	// AuthOpenAIHost is refused: the gateway owns the ChatGPT refresh.
	AuthOpenAIHost = "auth.openai.com"
	// DispatcherHost is runbundle.DefaultDispatcherURL's host: the dispatcher as the VM sees it.
	DispatcherHost = "dispatcher.factory.internal"
)

// The ports the gateway's own routes are served on.
const (
	InterceptPort  = 443 // the intercepted hosts' CONNECT port
	DispatcherPort = 80  // the dispatcher's plain HTTP port
	httpPort       = 80  // an absolute-form http URL's port when it names none
)

// anthropicHosts get the Anthropic credential (claude).
var anthropicHosts = []string{"api.anthropic.com", "claude.ai", "console.anthropic.com", "platform.claude.com"}

// AnthropicHosts returns the hosts that get the Anthropic credential, sorted.
func AnthropicHosts() []string { return slices.Clone(anthropicHosts) }

// InterceptedHosts returns every host the gateway TLS-intercepts, sorted: the Anthropic hosts,
// chatgpt.com, github.com and api.github.com. The CA's name constraints list exactly these.
func InterceptedHosts() []string {
	hs := append(slices.Clone(anthropicHosts), ChatGPTHost, GitHost, GitHubAPIHost)
	slices.Sort(hs)
	return hs
}

// Class is a host's row in the host table.
type Class uint8

// The host classes.
const (
	Ordinary   Class = iota // every other host: a tunnel or plain forwarding, after the egress check
	Anthropic               // the Anthropic hosts: intercepted, the Anthropic credential
	ChatGPT                 // chatgpt.com: intercepted, the ChatGPT login
	AuthOpenAI              // auth.openai.com: refused
	GitHub                  // github.com: intercepted, git credentials by repo
	GitHubAPI               // api.github.com: intercepted, the task token on GET and HEAD
	Dispatcher              // the dispatcher: plain HTTP only, the run token
)

var classNames = [...]string{"ordinary", "anthropic", "chatgpt", "auth-openai", "github", "github-api", "dispatcher"}

func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "unknown"
}

// ClassOf is host's class. host is normalised first (NormalizeHost); an IP literal is Ordinary.
func ClassOf(host string) Class {
	switch h := NormalizeHost(host); h {
	case ChatGPTHost:
		return ChatGPT
	case AuthOpenAIHost:
		return AuthOpenAI
	case GitHost:
		return GitHub
	case GitHubAPIHost:
		return GitHubAPI
	case DispatcherHost:
		return Dispatcher
	default:
		if slices.Contains(anthropicHosts, h) {
			return Anthropic
		}
		return Ordinary
	}
}

// Intercepted reports whether a CONNECT to the class is TLS-intercepted (on InterceptPort).
func (c Class) Intercepted() bool {
	return c == Anthropic || c == ChatGPT || c == GitHub || c == GitHubAPI
}

// StripsCarriers reports whether the class's routes remove every credential carrier
// (CarrierHeaders and URL user info) before setting the gateway's credential, so the VM's
// placeholder is replaced and never compared. An ordinary host keeps them, except
// Proxy-Authorization, which the gateway always drops.
func (c Class) StripsCarriers() bool { return c.Intercepted() || c == Dispatcher }

// CarrierHeaders returns the request headers that can carry a credential. URL user info is a
// carrier too.
func CarrierHeaders() []string { return []string{"Authorization", "Proxy-Authorization", "X-Api-Key"} }

// CookieRule is what the gateway does with a request's Cookie header.
type CookieRule uint8

// The cookie rules.
const (
	CookiesUntouched CookieRule = iota // tunnels and plain forwarding carry no credential
	CookiesInfraOnly                   // only InfraCookies pass (FilterCookies)
	CookiesStripped                    // the header is removed
)

// Cookies is the class's cookie rule: the model hosts keep only the infrastructure cookies,
// github.com, api.github.com and the dispatcher lose the header, and ordinary hosts keep it.
func (c Class) Cookies() CookieRule {
	switch c {
	case Anthropic, ChatGPT:
		return CookiesInfraOnly
	case GitHub, GitHubAPI, Dispatcher:
		return CookiesStripped
	default:
		return CookiesUntouched
	}
}

// NormalizeHost lower-cases an ASCII host name and removes one trailing dot. Non-ASCII bytes are
// kept as they are, so a name that only folds to a known host under Unicode rules
// (conſole.anthropic.com, with a long s, which strings.EqualFold matches) never matches one.
func NormalizeHost(h string) string {
	return strings.TrimSuffix(asciiLower(h), ".")
}

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// Target is a destination the VM named: a normalised host name, or an IP literal, and a port.
type Target struct {
	// Host is the name, lower-cased without a trailing dot, or the IP literal's canonical form
	// (netip.Addr.String, without brackets).
	Host string
	// Addr is the IP literal; the zero Addr for a name.
	Addr netip.Addr
	Port uint16
}

// IsIP reports whether the target is an IP literal.
func (t Target) IsIP() bool { return t.Addr.IsValid() }

// String is the target as host:port, an IPv6 literal in brackets.
func (t Target) String() string {
	p := strconv.Itoa(int(t.Port))
	if t.Addr.Is6() {
		return "[" + t.Host + "]:" + p
	}
	return t.Host + ":" + p
}

// ParseConnectTarget parses a CONNECT's target, host:port with the port required.
func ParseConnectTarget(s string) (Target, bool) { return parseHostPort(s, 0) }

// ParseHostPort parses an absolute-form URL's host, [host]:port or host, with defaultPort
// when it has none.
func ParseHostPort(s string, defaultPort uint16) (Target, bool) {
	if defaultPort == 0 {
		return Target{}, false
	}
	return parseHostPort(s, defaultPort)
}

// parseHostPort accepts a host name (ASCII letters, digits, '-' and '_', in non-empty labels of
// at most 63 bytes, at most 253 bytes in all after its trailing dot), an IPv4 literal, or an
// IPv6 literal in brackets (a zone of interface-name bytes allowed: the egress check denies it). The port is decimal,
// 1-65535, without a sign or a leading zero. defaultPort 0 makes the port required.
func parseHostPort(s string, defaultPort uint16) (Target, bool) {
	host, port := s, ""
	hasPort := false
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return Target{}, false
		}
		host = s[1:end]
		switch rest := s[end+1:]; {
		case rest == "":
		case rest[0] == ':':
			port, hasPort = rest[1:], true
		default:
			return Target{}, false
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || !validZone(a.Zone()) {
			return Target{}, false
		}
		p, ok := targetPort(port, hasPort, defaultPort)
		if !ok {
			return Target{}, false
		}
		return Target{Host: a.String(), Addr: a, Port: p}, true
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host, port, hasPort = s[:i], s[i+1:], true
	}
	p, ok := targetPort(port, hasPort, defaultPort)
	if !ok {
		return Target{}, false
	}
	// An IPv4 literal with a trailing dot (1.2.3.4.) is that literal, not a name.
	h := NormalizeHost(host)
	if a, err := netip.ParseAddr(h); err == nil {
		if !a.Is4() { // an unbracketed IPv6 literal
			return Target{}, false
		}
		return Target{Host: a.String(), Addr: a, Port: p}, true
	}
	if !validName(h) {
		return Target{}, false
	}
	return Target{Host: h, Port: p}, true
}

func targetPort(s string, present bool, defaultPort uint16) (uint16, bool) {
	if !present {
		return defaultPort, defaultPort != 0
	}
	if s == "" || len(s) > 5 || s[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	if n > 65535 {
		return 0, false
	}
	return uint16(n), true
}

// validName reports whether h (normalised) is a syntactically valid host name. Its last label
// must start with a letter, as v1alpha1.ValidateHost requires, so a numeric form a resolver
// might read as an IPv4 address (127.1, 0x7f000001, 2130706433) is refused, not resolved.
func validName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if last := h[strings.LastIndexByte(h, '.')+1:]; last == "" || last[0] < 'a' || last[0] > 'z' {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !nameByte(label[i]) {
				return false
			}
		}
	}
	return true
}

// validZone reports whether an IPv6 zone is empty or shaped like an interface name (ASCII
// letters, digits, '.', '-', '_' and '%', as in %25en0), so Target.Host holds no other text.
func validZone(z string) bool {
	for i := 0; i < len(z); i++ {
		if c := z[i]; !nameByte(c) && !('A' <= c && c <= 'Z') && c != '.' && c != '%' {
			return false
		}
	}
	return true
}

func nameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// CheckHost checks the Host header of a request inside an intercepted tunnel against the
// CONNECT's host: both normalised, and the header's port, when it has one, removed only when it
// is 443. It returns "" when they match, else CodeHostMismatch (another name, or another port).
func CheckHost(hostHeader, connectHost string) Code {
	t, ok := ParseHostPort(hostHeader, InterceptPort)
	if !ok || t.IsIP() || t.Port != InterceptPort || t.Host != NormalizeHost(connectHost) {
		return CodeHostMismatch
	}
	return ""
}

// Action is what the gateway does with a CONNECT or a plain HTTP request.
type Action uint8

// The actions.
const (
	Refuse       Action = iota // answer Decision.Refusal (and close a CONNECT)
	Intercept                  // CONNECT: TLS-terminate and apply the class's request rules
	Tunnel                     // CONNECT: resolve, check every address (Egress.CheckResolved), tunnel
	Forward                    // plain HTTP: resolve, check every address, forward
	ToDispatcher               // plain HTTP to the dispatcher: its route rule, then Config.Dispatcher
)

var actionNames = [...]string{"refuse", "intercept", "tunnel", "forward", "dispatcher"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return "unknown"
}

// Decision is the host table's answer for one CONNECT or plain HTTP request.
type Decision struct {
	Action  Action
	Refusal Code // set exactly when Action is Refuse
	Class   Class
	Target  Target // the zero Target when the target didn't parse
}

func refuse(code Code, c Class, t Target) Decision {
	return Decision{Action: Refuse, Refusal: code, Class: c, Target: t}
}

// DecideConnect applies the host table to a CONNECT's target (host:port): bad-target when it
// doesn't parse; auth.openai.com refused (auth-host); the dispatcher refused (plain-only); an
// intercepted host intercepted on 443 and refused on any other port (port); any other host
// tunnelled when e's host check allows it, else egress-not-allowed. The intercepted hosts and
// the dispatcher are the gateway's own routes, so the egress mode never applies to them.
func DecideConnect(target string, e Egress) Decision {
	t, ok := ParseConnectTarget(target)
	if !ok {
		return refuse(CodeBadTarget, Ordinary, Target{})
	}
	switch c := ClassOf(t.Host); {
	case c == AuthOpenAI:
		return refuse(CodeAuthHost, c, t)
	case c == Dispatcher:
		return refuse(CodePlainOnly, c, t)
	case c.Intercepted():
		if t.Port != InterceptPort {
			return refuse(CodePort, c, t)
		}
		return Decision{Action: Intercept, Class: c, Target: t}
	default:
		if !e.AllowsHost(t) {
			return refuse(CodeEgressNotAllowed, c, t)
		}
		return Decision{Action: Tunnel, Class: c, Target: t}
	}
}

// DecidePlain applies the host table to an absolute-form request's URL scheme and host (port 80
// when it has none): not-proxy for a scheme other than http or a host that doesn't parse (an
// origin-form request has neither); auth.openai.com refused (auth-host); an intercepted host
// refused (plain-http); the dispatcher on port 80 only, else dispatcher-route; any other host
// forwarded when e's host check allows it, else egress-not-allowed.
func DecidePlain(scheme, hostport string, e Egress) Decision {
	if asciiLower(scheme) != "http" {
		return refuse(CodeNotProxy, Ordinary, Target{})
	}
	t, ok := ParseHostPort(hostport, httpPort)
	if !ok {
		return refuse(CodeNotProxy, Ordinary, Target{})
	}
	switch c := ClassOf(t.Host); {
	case c == AuthOpenAI:
		return refuse(CodeAuthHost, c, t)
	case c.Intercepted():
		return refuse(CodePlainHTTP, c, t)
	case c == Dispatcher:
		if t.Port != DispatcherPort {
			return refuse(CodeDispatcherRoute, c, t)
		}
		return Decision{Action: ToDispatcher, Class: c, Target: t}
	default:
		if !e.AllowsHost(t) {
			return refuse(CodeEgressNotAllowed, c, t)
		}
		return Decision{Action: Forward, Class: c, Target: t}
	}
}
