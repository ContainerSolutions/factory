// Package gatewaycases is the request table that the workspace gateway and the test-only
// fakegateway both run (gateway.md, "Sharing with the fakegateway"). It is data only and
// imports neither proxy, so a bug in one proxy's policy shows up as a difference from the
// other. A case gives the request the VM sends, the status and refusal code it gets, and what
// the upstream receives; each package's test runs the cases against its own proxy, upstream and
// key. Bodies differ between the two (fakegateway: and factory gateway:), so cases compare the
// refusal code, the X-Factory-Gateway-Refusal header.
//
// It holds the model endpoint's route (OC-3) for now; the rest of the fake's route table moves
// here with the conformance PR (gateway.md, Delivery, PR 10).
package gatewaycases

import (
	"net/http"

	"github.com/ContainerSolutions/factory/internal/agent/harness"
)

// ModelHost is the model endpoint's name as the VM sees it.
const ModelHost = "model.factory.internal"

// Case is one request through a proxy.
type Case struct {
	Name string
	// Raw is the request the VM sends to the proxy, absolute-form or a CONNECT, with CRLF line
	// ends and its body.
	Raw string
	// NoEndpoint runs the case on a proxy without a model endpoint.
	NoEndpoint bool
	// Status is the status the VM gets, and Refusal the code it carries ("" when forwarded).
	Status  int
	Refusal string
	// Upstream is the request URI (path and query) the model server gets when the endpoint's URL
	// has no path; "" when the request must not reach it. A request that reaches it carries
	// exactly the proxy's key as Bearer and no other carrier, Cookie or placeholder.
	Upstream string
}

// placeholderCarriers are the carriers a VM's model client may send, each the placeholder.
const placeholderCarriers = "Authorization: Bearer " + harness.Placeholder + "\r\n" +
	"X-Api-Key: " + harness.Placeholder + "\r\n" +
	"Proxy-Authorization: Bearer " + harness.Placeholder + "\r\n" +
	"Cookie: session=" + harness.Placeholder + "\r\n"

// ModelEndpoint is the model endpoint's route (gateway.md, the model endpoint): plain HTTP on
// port 80, POST /v1/chat/completions and GET /v1/models[/<id>] only, the key always injected,
// and every check made before the endpoint is read.
func ModelEndpoint() []Case {
	const m = "http://" + ModelHost
	get := func(target string) string {
		return "GET " + target + " HTTP/1.1\r\nHost: " + ModelHost + "\r\nAuthorization: Bearer " + harness.Placeholder + "\r\n\r\n"
	}
	post := func(target, headers string) string {
		return "POST " + target + " HTTP/1.1\r\nHost: " + ModelHost + "\r\n" + headers + "Content-Length: 2\r\n\r\n{}"
	}
	const ok, refused = http.StatusOK, http.StatusForbidden
	return []Case{
		{Name: "chat completions, every carrier replaced", Raw: post(m+"/v1/chat/completions?stream=true", placeholderCarriers), Status: ok, Upstream: "/v1/chat/completions?stream=true"},
		{Name: "chat completions, user info replaced", Raw: post("http://x:"+harness.Placeholder+"@"+ModelHost+"/v1/chat/completions", ""), Status: ok, Upstream: "/v1/chat/completions"},
		{Name: "chat completions without a carrier", Raw: post(m+"/v1/chat/completions", ""), Status: ok, Upstream: "/v1/chat/completions"},
		{Name: "models", Raw: get(m + "/v1/models"), Status: ok, Upstream: "/v1/models"},
		{Name: "a model", Raw: get(m + "/v1/models/qwen3-coder-next"), Status: ok, Upstream: "/v1/models/qwen3-coder-next"},
		{Name: "on port 80, the host normalised", Raw: get("http://Model.Factory.Internal.:80/v1/models"), Status: ok, Upstream: "/v1/models"},
		{Name: "another path", Raw: post(m+"/v1/completions", placeholderCarriers), Status: refused, Refusal: "model-path"},
		{Name: "vLLM's metrics", Raw: get(m + "/metrics"), Status: refused, Refusal: "model-path"},
		{Name: "under a model", Raw: get(m + "/v1/models/org/model"), Status: refused, Refusal: "model-path"},
		{Name: "another method", Raw: get(m + "/v1/chat/completions"), Status: refused, Refusal: "model-path"},
		{Name: "another port", Raw: get(m + ":8000/v1/models"), Status: refused, Refusal: "model-path"},
		{Name: "a path that isn't canonical", Raw: get(m + "/v1/models/../../metrics"), Status: http.StatusBadRequest, Refusal: "bad-path"},
		{Name: "CONNECT", Raw: "CONNECT " + ModelHost + ":443 HTTP/1.1\r\nHost: " + ModelHost + ":443\r\n\r\n", Status: refused, Refusal: "plain-only"},
		{Name: "CONNECT on port 80", Raw: "CONNECT " + ModelHost + ":80 HTTP/1.1\r\nHost: " + ModelHost + ":80\r\n\r\n", Status: refused, Refusal: "plain-only"},
		{Name: "no endpoint configured", NoEndpoint: true, Raw: post(m+"/v1/chat/completions", placeholderCarriers), Status: http.StatusServiceUnavailable, Refusal: "no-login"},
		{Name: "no endpoint, the route checked first", NoEndpoint: true, Raw: get(m + "/metrics"), Status: refused, Refusal: "model-path"},
	}
}
