package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
	"github.com/compliance-framework/factory/internal/apiclient"
)

// The credential classes a record's Injected names.
const (
	InjectedGitHubAPI  = "github-api"       // the task token as Bearer on api.github.com
	InjectedDispatcher = "dispatcher-token" // Config.DispatcherToken on the dispatcher route
	injectedGitPrefix  = "git:"             // git:<owner>/<repo>: that repo's token as Basic
)

// gitUsername is the user name a GitHub App installation token is presented with.
const gitUsername = "x-access-token"

// errDispatcherAddr refuses a dispatcher dial to any address but Config.Dispatcher's.
var errDispatcherAddr = errors.New("gateway: the dispatcher transport dials only the dispatcher")

// routeIntercepted applies the class's request rule inside an intercepted tunnel (github.com
// by repo, api.github.com's reads) and returns the Authorization to set after the carriers are
// stripped ("" for none); it answers a refusal itself and returns ok false then. The model hosts
// are a later PR's: they get no credential yet.
func (g *Gateway) routeIntercepted(w http.ResponseWriter, r *http.Request, d policy.Decision, rec *Record) (auth string, ok bool) {
	switch d.Class {
	case policy.GitHub:
		var token string
		gd := policy.DecideGitHub(r.Method, r.URL, func(repo string) policy.Access {
			var a policy.Access
			a, token = g.repoAccess(repo)
			return a
		})
		if gd.Refusal != "" {
			rec.Status, rec.Refusal = writeRefusal(w, gd.Refusal, "")
			return "", false
		}
		if gd.Credentialed() && token != "" {
			rec.Injected = injectedGitPrefix + gd.Repo
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(gitUsername+":"+token)), true
		}
		return "", true
	case policy.GitHubAPI:
		graphql, code := policy.DecideGitHubAPI(r.Method, r.URL)
		if code == "" && graphql {
			code = g.checkGraphQL(r)
		}
		if code != "" {
			rec.Status, rec.Refusal = writeRefusal(w, code, "")
			return "", false
		}
		if tok := g.taskToken(); tok != "" {
			rec.Injected = InjectedGitHubAPI
			return "Bearer " + tok, true
		}
		return "", true
	default:
		return "", true
	}
}

// checkGraphQL reads a POST /graphql body (at most policy.MaxGraphQLBody) and checks that it
// only reads, then puts the bytes back for forwarding with Content-Type exactly application/json
// and no Content-Encoding, so the upstream parses the bytes that were checked. Anything it can't
// show is a read (not exactly one Content-Type, another content type than JSON, an encoding other
// than identity, a body too long or cut short, a mutation) is api-write.
func (g *Gateway) checkGraphQL(r *http.Request) policy.Code {
	cts := r.Header.Values("Content-Type")
	if len(cts) != 1 || !identityEncoding(r.Header.Values("Content-Encoding")) {
		return policy.CodeAPIWrite
	}
	mt, _, err := mime.ParseMediaType(cts[0])
	if err != nil || mt != "application/json" || r.ContentLength > policy.MaxGraphQLBody {
		return policy.CodeAPIWrite
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, policy.MaxGraphQLBody+1))
	if err != nil || len(body) > policy.MaxGraphQLBody || policy.CheckGraphQLRequest(body) != nil {
		return policy.CodeAPIWrite
	}
	_ = r.Body.Close()
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	// The forwarded request's headers are cloned from r's.
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Content-Encoding")
	return ""
}

// identityEncoding reports whether a request's Content-Encoding values name no coding but
// identity (none at all included).
func identityEncoding(values []string) bool {
	for _, v := range values {
		for _, c := range strings.Split(v, ",") {
			if !strings.EqualFold(strings.TrimSpace(c), "identity") {
				return false
			}
		}
	}
	return true
}

// repoAccess is repo's access (owner/name, lower-cased) and its token, from the job's current
// credentials, read now so a heartbeat's refresh applies at once: the task credential's repo is
// rw when it writes contents, else read, with the task token, which wins for its repo; each
// read credential's repo and readRepos are read, with its token. Only github.com credentials
// count.
func (g *Gateway) repoAccess(repo string) (policy.Access, string) {
	if g.cfg.Git == nil {
		return policy.AccessUnknown, ""
	}
	if c := g.cfg.Git.Get(); gitHubCred(c) && sameRepo(c.Repo, repo) {
		if c.Permissions["contents"] == "write" {
			return policy.AccessRW, c.Token
		}
		return policy.AccessRead, c.Token
	}
	for _, c := range g.cfg.Git.Reads() {
		if !gitHubCred(c) {
			continue
		}
		for _, r := range append([]string{c.Repo}, c.ReadRepos...) {
			if sameRepo(r, repo) {
				return policy.AccessRead, c.Token
			}
		}
	}
	return policy.AccessUnknown, ""
}

// taskToken is the task credential's token when it is github.com's; "" otherwise.
func (g *Gateway) taskToken() string {
	if g.cfg.Git == nil {
		return ""
	}
	if c := g.cfg.Git.Get(); gitHubCred(c) {
		return c.Token
	}
	return ""
}

func gitHubCred(c apiclient.GitCredential) bool {
	return policy.NormalizeHost(c.Host) == policy.GitHost
}

// sameRepo compares a credential's owner/name with a path's lower-cased repo.
func sameRepo(credRepo, repo string) bool {
	return credRepo != "" && strings.TrimSuffix(strings.ToLower(credRepo), ".git") == repo
}

// toDispatcher serves the dispatcher's only route: POST /api/v1/tasks/<TaskID>/effects
// (dispatcher-route for anything else, bad-path for a path that isn't canonical), sent to
// Config.Dispatcher over its own transport with every carrier and Cookie stripped and
// Config.DispatcherToken as Bearer.
func (g *Gateway) toDispatcher(w http.ResponseWriter, r *http.Request, rec *Record) {
	if code := policy.DecideDispatcher(r.Method, r.URL, g.cfg.TaskID); code != "" {
		rec.Status, rec.Refusal = writeRefusal(w, code, "")
		return
	}
	base := g.cfg.Dispatcher
	auth := ""
	if tok := g.cfg.DispatcherToken(); tok != "" {
		auth, rec.Injected = "Bearer "+tok, InjectedDispatcher
	}
	g.proxy(w, r, rec, g.dispatcher, func(out *http.Request) {
		out.URL = &url.URL{Scheme: base.Scheme, Host: base.Host,
			Path: strings.TrimSuffix(base.Path, "/") + r.URL.Path, RawQuery: r.URL.RawQuery}
		out.Host = base.Host
		stripCarriers(out, policy.Dispatcher)
		if auth != "" {
			out.Header.Set("Authorization", auth)
		}
	})
}

// newDispatcherTransport is the dispatcher's own transport (gateway.md, the dispatcher): a
// plain dialer without the egress check, since the dispatcher is the supervisor's own
// configuration (127.0.0.1:8800 on the Mac), that dials only Config.Dispatcher's host and port,
// so a request can't redirect it. HTTP/1.1, no proxy, UpstreamRoots for https.
func (g *Gateway) newDispatcherTransport() *http.Transport {
	base := g.cfg.Dispatcher
	port := base.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[base.Scheme]
	}
	want := net.JoinHostPort(base.Hostname(), port)
	plain := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	return &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: false, DisableCompression: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !strings.EqualFold(addr, want) {
				return nil, errDispatcherAddr
			}
			c, err := plain.DialContext(ctx, network, want)
			if err != nil {
				return nil, err
			}
			if tc := g.track(c); tc != nil {
				return tc, nil
			}
			return nil, &egressError{code: policy.CodeClosing}
		},
		TLSClientConfig:     &tls.Config{RootCAs: g.cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: tlsHandshakeTimeout, ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout: idleTimeout, MaxIdleConnsPerHost: 4,
	}
}
