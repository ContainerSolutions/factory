package policy

import "net/http"

// Code is a refusal: why the gateway answered a request itself, or closed a connection, instead
// of forwarding it. It is the X-Factory-Gateway-Refusal header's value and an egress record's
// "refusal".
type Code string

// The refusal codes (gateway.md, "Refusals and errors").
const (
	CodeNotProxy         Code = "not-proxy"
	CodeBadPath          Code = "bad-path"
	CodeHostMismatch     Code = "host-mismatch"
	CodeBadTarget        Code = "bad-target"
	CodePort             Code = "port"
	CodePlainHTTP        Code = "plain-http"
	CodePlainOnly        Code = "plain-only"
	CodeAuthHost         Code = "auth-host"
	CodePushRefused      Code = "push-refused"
	CodeWebWrite         Code = "web-write"
	CodeAPIWrite         Code = "api-write"
	CodeDispatcherRoute  Code = "dispatcher-route"
	CodeModelPath        Code = "model-path"
	CodeEgressNotAllowed Code = "egress-not-allowed"
	CodeEgressDenied     Code = "egress-denied"
	CodeResolve          Code = "resolve"
	CodeUpstream         Code = "upstream"
	CodeTimeout          Code = "timeout"
	CodeClosing          Code = "closing"
	CodeNoLogin          Code = "no-login"
	// CodeSNIMismatch, CodeNotTLS and CodePeer close the connection without an answer.
	CodeSNIMismatch Code = "sni-mismatch"
	CodeNotTLS      Code = "not-tls"
	CodePeer        Code = "peer"
)

const (
	// RefusalHeader carries the code on every answer the gateway makes itself.
	RefusalHeader = "X-Factory-Gateway-Refusal"
	// ContentType is the type of every answer the gateway makes itself.
	ContentType = "text/plain; charset=utf-8"
	// BodyPrefix starts every refusal body.
	BodyPrefix = "factory gateway: "
)

type refusal struct {
	status int
	text   string // the body after BodyPrefix, without the newline; "" for egress-denied
}

var refusals = map[Code]refusal{
	CodeNotProxy:         {http.StatusBadRequest, "not a proxy request"},
	CodeBadPath:          {http.StatusBadRequest, "refused: the path is not canonical"},
	CodeHostMismatch:     {http.StatusBadRequest, "refused: the Host header does not match the CONNECT"},
	CodeBadTarget:        {http.StatusBadRequest, "refused: the CONNECT target is not host:port"},
	CodePort:             {http.StatusForbidden, "refused: this host is served on port 443 only"},
	CodePlainHTTP:        {http.StatusForbidden, "refused: plain HTTP to an intercepted host"},
	CodePlainOnly:        {http.StatusForbidden, "refused: the dispatcher is served over plain HTTP through the proxy"},
	CodeAuthHost:         {http.StatusForbidden, "refused: auth.openai.com is not reachable from workspaces (the gateway refreshes the login)"},
	CodePushRefused:      {http.StatusForbidden, "push refused: the repository is not writable for this task"},
	CodeWebWrite:         {http.StatusForbidden, "refused: github.com takes only GET and HEAD outside git"},
	CodeAPIWrite:         {http.StatusForbidden, "refused: api.github.com is read-only for workspaces"},
	CodeDispatcherRoute:  {http.StatusForbidden, "refused: the dispatcher route is not open to workspaces"},
	CodeModelPath:        {http.StatusForbidden, "refused: this path on a model host is not open to workspaces"},
	CodeEgressNotAllowed: {http.StatusForbidden, "refused: the task's egress policy does not allow this host"},
	CodeEgressDenied:     {http.StatusForbidden, ""},
	CodeResolve:          {http.StatusBadGateway, "the destination's name does not resolve"},
	CodeUpstream:         {http.StatusBadGateway, "the upstream is unreachable"},
	CodeTimeout:          {http.StatusGatewayTimeout, "the upstream timed out"},
	CodeClosing:          {http.StatusServiceUnavailable, "the gateway is closing"},
	CodeNoLogin:          {http.StatusServiceUnavailable, "no model login for this host on this agent host"},
}

// Codes returns every refusal code, in the LLD's table order.
func Codes() []Code {
	return []Code{
		CodeNotProxy, CodeBadPath, CodeHostMismatch, CodeBadTarget, CodePort, CodePlainHTTP,
		CodePlainOnly, CodeAuthHost, CodePushRefused, CodeWebWrite, CodeAPIWrite,
		CodeDispatcherRoute, CodeModelPath, CodeEgressNotAllowed, CodeEgressDenied, CodeResolve, CodeUpstream,
		CodeTimeout, CodeClosing, CodeNoLogin, CodeSNIMismatch, CodeNotTLS, CodePeer,
	}
}

// Status is the HTTP status the gateway answers c with; 0 for a code it answers by closing the
// connection (sni-mismatch, not-tls, peer) and for an unknown code.
func (c Code) Status() int { return refusals[c].status }

// Response is an answer the gateway makes itself. Nothing in it is chosen by the VM: the body
// never names a credential, an address or a host.
type Response struct {
	Status int
	Header http.Header // Content-Type and RefusalHeader
	Body   string      // BodyPrefix, the code's text and a newline
}

// Response builds the answer for c. class is used only by egress-denied, whose body names it;
// it must then be one of the DenyClasses. ok is false for a code without an answer (sni-mismatch,
// not-tls, peer), an unknown code, and egress-denied with a class outside the set.
func (c Code) Response(class DenyClass) (r Response, ok bool) {
	ref, ok := refusals[c]
	if !ok {
		return Response{}, false
	}
	text := ref.text
	if c == CodeEgressDenied {
		if !class.Valid() {
			return Response{}, false
		}
		text = "refused: the destination is a denied address (" + string(class) + ")"
	}
	return Response{
		Status: ref.status,
		Header: http.Header{"Content-Type": {ContentType}, RefusalHeader: {string(c)}},
		Body:   BodyPrefix + text + "\n",
	}, true
}
