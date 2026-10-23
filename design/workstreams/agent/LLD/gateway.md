# LLD — The workspace gateway (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: P1 phase 4 · Packages: `internal/agent/gateway`, `internal/agent/gateway/{policy,ca,chatgpt}`, `internal/agent/host` (the `gatewayca` step)
> Design: [P1 §3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [§5](../../../proposals/P1-agent-workspaces.md#5-credentials-injected-by-the-gateway-never-in-the-workspace), Decisions 4, 6, 7 and 9 · Phase 0: [Q4](../../../proposals/P1-phase0-report.md#q4-does-claude-code-work-behind-a-tls-intercepting-gateway-with-a-placeholder-token), [Q5](../../../proposals/P1-phase0-report.md#q5-can-the-gateway-carry-codexs-subscription-auth), [egress](../../../proposals/P1-phase0-report.md#the-gateway-and-egress-on-the-mac) · The VM side: [workspace-run.md](workspace-run.md#phase-4s-gateway)

The gateway is the workspace's only TCP route out and the only holder of its credentials. (The VM's DNS still goes to the Mac's resolver, outside the gateway and its log: [Risks](#risks-and-open-questions), item 9.) It's an explicit forward proxy (absolute-form HTTP and CONNECT) that TLS-intercepts only the credentialed hosts, adds their credentials and applies the per-host rules below. Every other host gets a plain tunnel, after an egress check. It replaces Phase 0's mitmproxy addon (`gw.py`) and implements the policy the test-only `fakegateway` (#109) models. One gateway runs per workspace (Decision 7), as a library inside the phase 5 supervisor, which starts it on the workspace's gateway address and closes it with the workspace.

## Refinements of P1

- **The dispatcher route is narrower than the run token.** The run token (#101) authorises the task's prompt, context, output schema, effects and transcript upload. Through the gateway the VM gets **only `POST` effects** ([the dispatcher](#the-dispatcher)): the bundle already holds the rest, and the supervisor uploads the transcript after it has re-redacted it and added the `factory.egress` lines, so a VM upload could replace that audit.
- **Clients are pinned to their workspace.** On the Mac, pf lets every Softnet bridge reach the gateway port on *any* of the Mac's addresses, so VM A could reach workspace B's gateway on B's bridge. Each gateway therefore accepts connections only from its own VM's **exact** addresses (`Config.Clients`, a list of addresses, never prefixes: on tart the VM's leased address, which the driver reads after `Start`; on kata the workspace's CNI address). Pinning the address and not the /29 refuses the Mac's own address on that bridge and every other address in the subnet, so a local process on the Mac can't pose as the VM. The pin is the gateway's only client authentication: there is no proxy password, since the VM holds no secret to present, and `Proxy-Authorization` is dropped. It holds only while **a VM can't send from another workspace's address**, a precondition on the driver:
  - **tart:** Softnet filters each VM's packets to its own MAC and leased IP. Phase 5's manual check confirms it: a VM that adds another address (another workspace's, or another in its /29) and sends from it can't reach any gateway.
  - **kata (phase 9):** per-workspace source-address isolation is a requirement of the Linux track's driver: one CNI network per workspace, or anti-spoofing rules that bind each VM to its address. Phase 9's LLD must show it ([workspace-host.md](workspace-host.md#later-phases)).

  `New` refuses an empty `Clients`, and `Serve` refuses a `Clients` address that is one of the agent host's own interface addresses.
- **The CA is name-constrained** to the intercepted hosts, with every IP address excluded and URI and email names permitted only under `invalid`, so a leaked CA key can mint certificates only for those hosts, not for any site or IP address the VM visits ([the CA](#the-ca)).
- **Tunnels check the TLS SNI.** Every tunnel peeks the client's ClientHello and refuses an SNI that differs from the CONNECT host ([tunnels](#tunnels)). Deny-by-default is still best effort against egress by host name, not a boundary against a hostile VM: the credentials are the boundary.
- **`auth.openai.com` is refused at CONNECT** and never intercepted: the gateway owns ChatGPT refresh, and nothing the VM sends there is forwarded.
- **github.com web paths stay usable.** The fake fails closed for every path of a repo that isn't rw. The gateway keeps that for git's paths, and forwards other `GET`/`HEAD` requests (release downloads, archives) anonymously, since `setup` scripts install tools from GitHub releases.

## Departures from Phase 0's gw.py

`gw.py` is the only gateway shown to work with real claude and codex. Where this LLD behaves differently on purpose, phase 8's first run checks the egress log for the item, so a regression there reads as the departure and not as a gateway bug or a codex change:

| # | Phase 0 | This gateway | Phase 8 checks, in the egress log |
|---|---|---|---|
| D1 | `gw.py` intercepted `api.openai.com` and `auth.openai.com`, with the ChatGPT token injected on `api.openai.com` | `api.openai.com` is an ordinary host (a tunnel, egress-checked, no credential); `auth.openai.com` is refused (`auth-host`) | no codex call fails on `api.openai.com` (`mode: tunnel` records only), and none reaches `auth.openai.com` |
| D2 | `gw.py` set `chatgpt-account-id` on every chatgpt.com request | the same (no departure): `ChatGPT-Account-Id` is an identifier, not a credential, and is set on every chatgpt.com request while a login exists | as gw.py: codex's `backend-api` calls and the WebSocket upgrade succeed |
| D3 | `gw.py` forwarded `Cookie` untouched everywhere | on the model hosts only the infrastructure cookies pass ([cookies](#carriers-and-cookies)); stripped on github.com, api.github.com and the dispatcher | no chatgpt.com request gets a 403 (a Cloudflare challenge), and codex's WebSocket doesn't reconnect in a loop |
| D4 | `gw.py` swapped the Anthropic header the VM sent | the credential follows the gateway's `AnthropicAuth` whenever either carrier is present | `injected: anthropic-*` on the same calls as Phase 0's Q4 list, and none on `event_logging` or `mcp-registry` |
| D5 | the fakegateway (#109) refuses every path of a repo that isn't rw, web paths included | github.com web `GET`/`HEAD` forwarded anonymously | a `setup` release download works (with its redirect hosts allowed under deny-by-default, [egress](#egress)) |

## Packages

| Package | What | Rules |
|---|---|---|
| `internal/agent/gateway/policy` | the decisions: host classes, the github.com, api.github.com and dispatcher rules, the egress check, the cookie filter, the refusal codes and bodies | **pure**, like `internal/rules`: no network, no clock, no I/O. Table-tested per row of [the host table](#the-host-table) |
| `internal/agent/gateway/ca` | the agent host's CA: `Generate`, `Load`, leaf minting and the leaf cache | crypto and files only |
| `internal/agent/gateway/chatgpt` | `Login`, the `Store` interface, `Refresher` (proactive and on-401 refresh, single-flight, write-back) | one `Refresher` per supervisor, shared by its gateways |
| `internal/agent/gateway` | the proxy: listener, CONNECT, tunnels and their ClientHello peek, interception, upstream transports (the dispatcher's included), the egress dialer, the egress log, `Close` | uses the three above |
| `internal/agent/host` | the `gatewayca` step and `host init --rotate-ca` | as the other steps ([workspace-host.md](workspace-host.md#steps-tart-driver)) |

No new dependency: TLS, `httputil.ReverseProxy` (which already passes a `101 Switching Protocols` through by hijacking both sides) and `net/netip` are the standard library's. No `factory-agent` subcommand is added: a manual entry would need a subscription store and a lease, both phase 5's, and would be one more credential path to review. Tests drive the library in process.

### Sharing with the fakegateway

**The fake stays independent and imports nothing from `gateway`.** It's the oracle that phase 3's tests (`pipeline`, `effector`, `vmrun`) were written against and reviewed hard; if both used one policy package, a bug there would be a bug in both, and no test would see it. What they share is **data**: PR 10 moves the fake's route table (`TestRoutes`) into `internal/agent/internal/gatewaycases` (test-only), and both packages run every case. A case gives the request, the status, whether an upstream is reached and the credential class it gets. Refusal bodies differ (`fakegateway:` and `factory gateway:`), so the cases compare the refusal *code*: the `X-Factory-Gateway-Refusal` header, which PR 10 also adds to the fake's refusals (from its own constants). Where the gateway deliberately differs, the case says `gatewayOnly` or `fakeOnly` and why: github.com web paths, ports other than 443, the deny list, the client pin, the ClientHello peek, the cookie filter, the canonical-path check only where a path is matched.

## API

```go
package gateway

type Config struct {
    TaskID, RunID string           // required; TaskID binds the dispatcher route
    Clients       []netip.Addr     // the VM's exact addresses (IPv4-mapped unmapped); any other peer is closed. Required
    CA            *ca.CA           // required
    Git           *runner.Credentials // the job's; nil: none. Read on every request, so heartbeats' Set/SetReads apply at once
    DispatcherToken func() string  // read on every request; required. PoC: the worker's agent token; the lease's runToken (#101) when it lands
    Dispatcher    *url.URL         // the supervisor's dispatcher base URL (http or https), required; its own transport
    Anthropic     *AnthropicAuth   // nil: none. Only for a claude task
    ChatGPT       ChatGPTSource    // nil: none. Only for a codex task (*chatgpt.Refresher)
    Egress        policy.Egress    // the task's mode and allow list, extra denied prefixes, open addresses
    Sink          Sink             // the egress log; required
    Clock         clock.Clock      // records' times and durations (the CA has its own)
    Logger        *slog.Logger
    Resolver      Resolver         // default net.DefaultResolver; tests map names to test servers
    UpstreamRoots *x509.CertPool   // default: the system roots; tests: their httptest CAs. Also the dispatcher's
    Limits        Limits           // zero: the defaults below
}
type AnthropicAuth struct{ OAuthToken, APIKey string } // exactly one; String, Format, GoString, LogValue, MarshalJSON and MarshalText redact
type ChatGPTSource interface {
    Current(ctx context.Context) (chatgpt.Token, error)                  // refreshed ahead of expiry
    Unauthorized(ctx context.Context, used chatgpt.Token) (chatgpt.Token, error) // after an upstream 401
}
type Egress struct { // in package policy
    Mode       string           // "default" | "deny-by-default" (#100's environment.egress.mode)
    AllowHosts []string         // #100's environment.egress.allow: host, *.domain, either with :port
    Deny       []netip.Prefix   // added to DefaultDeny and the agent host's own prefixes
    OpenAddrs  []netip.AddrPort // exact exceptions to the deny list (not #100's allow)
}
type Sink interface{ Egress(Record) } // called synchronously, never after Close returns; must not block
type Resolver interface{ LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) }

func New(cfg Config) (*Gateway, error)              // validates; starts nothing
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error // until ctx ends or Close; nil then
func (g *Gateway) Close() error                     // idempotent; see [Close](#close)
```

- **The supervisor (phase 5)**, per lease: loads the CA (`ca.Load`, so a rotated CA applies to the next workspace without a restart), builds the `Config` from the job, `net.Listen("tcp", <gateway address>:<workspace.gatewayPort>)` after the driver's `Start`, runs `Serve` in a goroutine, and closes the gateway after `Destroy`. It gives a gateway only its task's model credential: a claude task's gateway has no ChatGPT login, and a codex task's has no Claude token.
- **Lease refresh needs no new call.** The worker's heartbeat already swaps refreshed git tokens into `job.Git` (`Set`, `SetReads`), and the gateway reads it on every request, as `gitcred` does. `DispatcherToken` is read on every request too, so a re-registered agent token applies at once; the run token (#101), when it replaces it, lives exactly as long as the lease.
- **The git host is `github.com`** (`policy.GitHost`), not a knob: the CA's name constraints list it ([the CA](#the-ca)), so another host (GitHub Enterprise) would need a new CA and a contract for it. Credentials whose `host` isn't `github.com` are never used.
- `New` refuses: an empty `TaskID`, `RunID`, `Clients` or `Sink`; a nil `DispatcherToken`; an invalid `Clients` address (unspecified, with a zone); an `AllowHosts` entry `v1alpha1.ValidateHost` refuses; a `Dispatcher` that isn't an absolute http(s) URL without user info, query or fragment; an `AnthropicAuth` with neither or both fields. Errors are `*ConfigError{Field}` and never carry a value.

## The host table

Host names are compared lower-cased and without a trailing dot, exactly (no suffixes): `downloads.claude.ai` is an ordinary host. A CONNECT names `host:port`, and a port is required. The intercepted hosts take CONNECT on port 443 only; any other port is refused (`port`). Plain HTTP to an intercepted host is refused (`plain-http`). A request that isn't in proxy form, and an absolute-form request with any scheme but `http` (a `GET https://…` sent without CONNECT), get 400 `not-proxy`: the gateway never fetches `https` on a client's behalf outside an intercepted tunnel.

| Host | Methods and paths | Credential | Refused |
|---|---|---|---|
| `api.anthropic.com`, `claude.ai`, `console.anthropic.com`, `platform.claude.com` | `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS` (exactly as sent). A request with a carrier takes only its host's path list (`policy.DecideModel`; the canonical path, case-sensitive, any query), the calls Phase 0's Q4 recorded: on `api.anthropic.com`, `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`, `/v1/models/<id>`, `/api/claude_code/settings`, `/api/claude_code/policy_limits`, `/api/claude_cli/bootstrap` and `/api/eval/sdk-<key>` (one segment); on the other three hosts, none. Not `/v1/messages/batches`, `/v1/files`, or `/api/oauth/` (which mints API keys). A request without a carrier isn't path-checked | the request carried `Authorization` or `X-Api-Key` (present, whatever its value): every carrier is stripped, then `Authorization: Bearer <OAuthToken>` or `X-Api-Key: <APIKey>`, per the gateway's `AnthropicAuth`, not per the carrier the VM used. No carrier: stripped and forwarded without one (Phase 0: event logging and the MCP registry are called that way). `Cookie`: [filtered](#carriers-and-cookies) | any other method (`TRACE` and `TRACK` echo the request, the injected credential included, in any case): 403 `api-write`; with a carrier, a path that isn't canonical: 400 `bad-path`, and one off the list (the owner's data, such as a file's content or a batch's results, or a minted key): 403 `model-path`. All before the login is read |
| `chatgpt.com` | the same methods, the WebSocket upgrade included. Every request, with `Authorization` or without (the account id goes on each), takes only the path list Phase 0's Q5 recorded: anything under `/backend-api/codex/` (responses and its WebSocket, models), `/backend-api/ps/plugins/list`, `/backend-api/ps/mcp`, `/backend-api/wham/accounts/check` and `/backend-api/wham/settings/user`. Not the owner's conversations, memories, files or cloud tasks (`/backend-api/wham/tasks`) | `ChatGPT-Account-Id: <account>` set from `ChatGPT.Current` on every request while a login exists, as gw.py did (an identifier, not a credential). With `Authorization` present: carriers stripped and `Authorization: Bearer <access>` set from `ChatGPT.Current`. A 401 is forwarded and recorded: the PoC has no refresh, the owner re-imports the login. Without it: stripped, no credential. `Cookie`: [filtered](#carriers-and-cookies) | as the Anthropic hosts, the path checks on every request |
| `auth.openai.com` | none | — | every CONNECT and plain request: 403 `auth-host` |
| `github.com` | see [github.com](#githubcom) | `Basic x-access-token:<token>` by the repo in the path | receive-pack for a repo that isn't rw, malformed git requests, non-`GET`/`HEAD` web requests |
| `api.github.com` | `GET`, `HEAD`, any path but `/graphql` (in any spelling: it can't be checked there); `POST /graphql` (exactly, no query) when the body is a read ([GraphQL](#graphql-reads)) | `Authorization: Bearer <task token>` (the lease's `gitCredential`); none when it's empty | every other request: 403 `api-write` |
| `dispatcher.factory.internal` (`runbundle.DefaultDispatcherURL`) | absolute-form `http` only, on port 80 (another port is `dispatcher-route`); see [the dispatcher](#the-dispatcher) | `Authorization: Bearer <DispatcherToken()>` | every other route: 403 `dispatcher-route`; CONNECT: 403 `plain-only` |
| everything else | CONNECT: a tunnel; absolute-form `http`: forwarded | none; only `Proxy-Authorization` is dropped | the [egress check](#egress) |

### Carriers and cookies

**Carriers** are `Authorization`, `Proxy-Authorization`, `X-Api-Key` and URL user info. On every injected route all of them are stripped before the credential is set, so the VM's placeholder is replaced and never compared (workspace-run.md: "always replace, never match placeholders"). For the audit, each record says what the VM sent in its carriers (`client`: `none`, `placeholder` when every carrier is `harness.Placeholder` in the forms the fake's `classify` accepts, else `other`). An `other` is logged as a warning, without its value: a credential reached the VM.

**`Cookie` isn't a carrier on the model hosts.** codex keeps a process-wide cookie jar for chatgpt.com, shared by its HTTP and WebSocket transports, that holds only Cloudflare's infrastructure cookies and the `__oailb` load-balancer cookie (`codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs` at rust-v0.163.0). They come from chatgpt.com's own `Set-Cookie`, which the gateway passes back, so after the first response every codex request carries them, and Phase 0's `gw.py` forwarded them. So on chatgpt.com and the Anthropic hosts (Cloudflare fronts them too) the gateway keeps only the cookies named in `policy.InfraCookies`, codex's allow list: `__cf_bm`, `__cflb`, `__cfruid`, `__cfseq`, `__cfwaitingroom`, `__oailb`, `_cfuvid`, `cf_clearance`, `cf_ob_info`, `cf_use_ob` and any `cf_chl_*`. It drops every other cookie (a session cookie the VM shouldn't have), and removes the header when none is left. Cookies don't count for `client`. On github.com, api.github.com and the dispatcher, `Cookie` is stripped. Tunnels and plain forwarding carry no credential and leave it alone.

**Inside an intercepted tunnel** each request must be in origin form: a `CONNECT`, an absolute-form request and `OPTIONS *` get 400 `not-proxy`, never forwarded. Its `Host` is normalised like the CONNECT's host (lower-cased, a trailing dot and a `:443` port removed) and must then be that host; another port, or another name, is 400 `host-mismatch`. Its upstream is always that host on 443, never a name taken from the request. The TLS handshake's SNI must be empty or that host (else the handshake fails). The canonical-path check (`RawPath == ""` and `path.Clean(Path) == Path`, else 400 `bad-path`) runs only where a path is matched (github.com, the dispatcher, and the model hosts on a request their path list gates): api.github.com takes encoded branch names (`feature%2Fx`), and its rule is the method alone.

### Refusals and errors

Every answer the gateway makes itself is `text/plain; charset=utf-8`, with `X-Factory-Gateway-Refusal: <code>` and the body below plus a newline. A refused CONNECT gets the status line, the header and the body, and the connection is closed. The bodies are `policy` constants and never name a credential, an address or a VM-chosen value; the egress class is one of a fixed set.

| Code | Status | Body (`factory gateway: …`) |
|---|---|---|
| `not-proxy` | 400 | `not a proxy request` |
| `bad-path` | 400 | `refused: the path is not canonical` |
| `host-mismatch` | 400 | `refused: the Host header does not match the CONNECT` |
| `bad-target` | 400 | `refused: the CONNECT target is not host:port` |
| `port` | 403 | `refused: this host is served on port 443 only` |
| `plain-http` | 403 | `refused: plain HTTP to an intercepted host` |
| `plain-only` | 403 | `refused: the dispatcher is served over plain HTTP through the proxy` |
| `auth-host` | 403 | `refused: auth.openai.com is not reachable from workspaces (the gateway refreshes the login)` |
| `push-refused` | 403 | `push refused: the repository is not writable for this task` |
| `web-write` | 403 | `refused: github.com takes only GET and HEAD outside git` |
| `api-write` | 403 | `refused: api.github.com is read-only for workspaces` |
| `dispatcher-route` | 403 | `refused: the dispatcher route is not open to workspaces` |
| `model-path` | 403 | `refused: this path on a model host is not open to workspaces` (a model host's request that would get the login, on a path off the host's list) |
| `egress-not-allowed` | 403 | `refused: the task's egress policy does not allow this host` |
| `egress-denied` | 403 | `refused: the destination is a denied address (<class>)`, class one of `unspecified`, `loopback`, `private`, `cgnat`, `link-local`, `ula`, `multicast`, `reserved`, `documentation`, `agent-host`, `extra` |
| `resolve` | 502 | `the destination's name does not resolve` |
| `upstream` | 502 | `the upstream is unreachable` (dial or TLS failure; the error is logged at debug, never sent: it can hold addresses) |
| `timeout` | 504 | `the upstream timed out` |
| `closing` | 503 | `the gateway is closing` |
| `no-login` | 503 | `no model login for this host on this agent host` (chatgpt.com when `ChatGPT.Current` has no token at all) |
| `sni-mismatch` | — | none: the tunnel's ClientHello names another host; the connection is closed after the `200` ([tunnels](#tunnels)) |
| `not-tls` | — | none: under deny-by-default, a tunnel to port 443 that doesn't start with a ClientHello; closed after the `200` |
| `peer` | — | none: a peer outside `Clients` is closed at accept |

### github.com

A path is `/<owner>/<repo>[.git]/<rest>`, the repo lower-cased without `.git`. **A git request** is one whose repo segment ends in `.git` (any case), or whose `rest`, lower-cased, is `info/refs`, `git-upload-pack` or `git-receive-pack`. Every other request is **a web request**.

The repo's access comes from `Config.Git` at the time of the request, only from credentials whose `host` is `github.com`: the task credential's `repo` is **rw** when its `permissions.contents` is `write`, else **read**; each read credential's `repo` and `readRepos` are **read**, with that credential's token. The task token wins for its repo, as in `gitcred`. Any other repo is **unknown**.

| Request | rw | read | unknown |
|---|---|---|---|
| `GET info/refs` with exactly `service=git-upload-pack` as the raw query | task token | its read token | anonymous |
| `POST git-upload-pack`, no query | task token | its read token | anonymous |
| `GET info/refs` with exactly `service=git-receive-pack` | task token | 403 `push-refused` | 403 `push-refused` |
| `POST git-receive-pack`, no query | task token | 403 `push-refused` | 403 `push-refused` |
| any other git request (a duplicate or upper-case parameter, `HEAD`, `objects/…`, `GET git-upload-pack`) | task token | 403 `push-refused` | 403 `push-refused` |
| web, `GET`/`HEAD` | anonymous | anonymous | anonymous |
| web, any other method | 403 `web-write` | 403 `web-write` | 403 `web-write` |

These are the fake's rows (rw repos pass any git request, read and unknown repos only the two exact fetch forms), plus the web rows. Refusals happen before any upstream is dialled. Pushing to a read repo is refused at `info/refs`, before GitHub sees it (Phase 0's check). A web request never carries a credential: GitHub's web paths don't need one for public content, and a release asset of a private repo goes through api.github.com.

### GraphQL reads

**Decided (2026-10-22, owner):** lisa and gh read through GraphQL, so `POST /graphql` on api.github.com passes when it only reads; GitHub writes from the VM stay on the `factory` CLI (the Effector). The request must have exactly one Content-Type, `application/json`, and no Content-Encoding but `identity`; the body at most 1 MiB, valid UTF-8, one JSON object (no batch array, no trailing data, no key twice under any case) with only `query` (a non-empty string), `variables` and `operationName`. The document is lexed (comments, strings and block strings skipped; a lexical error refuses it), and outside every bracket each definition must start with `{`, `query` or `fragment`, and no name may be `mutation` or `subscription` in any case (so an operation named `mutation` is refused too). Anything else is 403 `api-write`, before the upstream is dialled; the checked bytes are what's forwarded, with `Content-Type: application/json` and no Content-Encoding.

### The dispatcher

The only route is `POST /api/v1/tasks/<TaskID>/effects`: the path is compared segment by segment, after the canonical-path check, with `<TaskID>` exactly `Config.TaskID`; any query is allowed, as for the fake. It's fixed in `policy`, with no `Config` knob, so a supervisor can't widen it by mistake. The upstream is `Config.Dispatcher`: its scheme and host, its path joined in front of the request's, and `Host` set to it. That upstream is the supervisor's own configuration (on the Mac, `127.0.0.1:8800`; on the Linux supervisor an https URL), so it skips the egress check, which exists to stop VM-chosen destinations.

**The dispatcher has its own transport.** The egress dialer's `Control` refuses loopback, so it can't carry `127.0.0.1:8800`. The dispatcher's `http.Transport` dials with a plain `net.Dialer` (10 s, no `Control`) and only `Config.Dispatcher`'s host and port (the scheme's default when it has none): its `DialContext` refuses any other address, so a request can't redirect it. For an https dispatcher it verifies with `UpstreamRoots` (the system roots by default; the tests' httptest CA). HTTP/1.1, no proxy (`Proxy: nil`). This is the only dial in the gateway that bypasses the egress dialer; the `chatgpt` refresher's client is its own ([refresh](#chatgpt-refresh)) and isn't the gateway's.

| Route (`/api/v1`) | Run token (#101) | Through the gateway | Why |
|---|---|---|---|
| `POST /tasks/{id}/effects` | yes | **yes** | the Effector in the VM |
| `GET /tasks/{id}/prompt`, `/output-schema`, `/context/{n}` | yes | no | in the bundle (`PrepareBundle` on the host) |
| `PUT /runs/{id}/transcript` | yes | no | the supervisor uploads after re-redacting and adding `factory.egress` lines; a VM upload could replace them |
| anything else | no | no | — |

## Egress

Every destination the VM names (a CONNECT that isn't intercepted, an absolute-form `http` request) and every upstream dial for an intercepted host goes through `policy.Egress` and one dialer:

1. **The host check.** Under `Mode: deny-by-default` (#100's `environment.egress`), the host must match an `AllowHosts` entry, else 403 `egress-not-allowed`. Entries follow #100's rules, which are workspace.hosts' (`v1alpha1.ValidateHost`): a lower-case name of two or more labels, an optional single leading `*.`, an optional port, never an IP literal or `localhost`. The host is compared lower-cased and without a trailing dot. An exact entry matches that name only; `*.example.com` matches any name under `example.com`, at any depth (`a.example.com`, `a.b.example.com`), but not `example.com` itself. An entry with a port matches only that port; one without matches every port. An IP-literal destination never matches, so under deny-by-default it is always refused. The intercepted hosts are the gateway's own routes, so they are never subject to the mode: the run's git host, the dispatcher and its model always work. `default` skips this step.

   GitHub release downloads need their redirect hosts in the list: `github.com/<owner>/<repo>/releases/download/…` (an intercepted web path) answers with a redirect to `objects.githubusercontent.com` or `release-assets.githubusercontent.com`, and archives redirect to `codeload.github.com`, which are ordinary hosts here. A TaskSpec that installs tools from releases under deny-by-default lists `*.githubusercontent.com` and `codeload.github.com`.
2. **Resolve, then check every address.** An IP literal is used as it is; a name is resolved once (`Resolver.LookupNetIP(ctx, "ip", host)`, 5 s). If **any** address is denied, the request is refused (403 `egress-denied`, with the denied class in the body, never the address): a name that mixes public and private answers is refused, not filtered.
3. **Dial the checked addresses**, in order, as literal `ip:port` strings, so nothing is resolved again (no DNS rebinding). The dialer's `Control` checks the address again before the connect, in case a later change dials anything else. Upstream TLS still verifies against the host name.

**Denied** (`policy.DefaultDeny`), with IPv4-mapped IPv6 addresses (`::ffff:0:0/96`) unmapped first, and an address with a zone always denied. Each range has the class the `egress-denied` body names; PR 2's table test has a row per range:

| Class | IPv4 | IPv6 |
|---|---|---|
| `unspecified` | `0.0.0.0/8` | `::/128` |
| `loopback` | `127.0.0.0/8` | `::1/128` |
| `private` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | — |
| `cgnat` | `100.64.0.0/10` (Tailscale too) | — |
| `link-local` | `169.254.0.0/16` (metadata) | `fe80::/10` |
| `ula` | — | `fc00::/7` |
| `multicast` | `224.0.0.0/4` | `ff00::/8` |
| `documentation` | `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` | `2001:db8::/32` |
| `reserved` | `192.0.0.0/24`, `192.88.99.0/24` (the 6to4 relay anycast), `198.18.0.0/15` (benchmarking), `240.0.0.0/4` (with `255.255.255.255`) | `::/96` (deprecated IPv4-compatible, `::a.b.c.d`), `100::/64` (discard), `2001::/32` (Teredo, which embeds an obfuscated IPv4 address), `64:ff9b:1::/48` (local-use NAT64, RFC 8215), `fec0::/10` (deprecated site-local) |

The more specific range wins (`::1` is `loopback`, not `reserved`). `64:ff9b::/96` (well-known NAT64) and `2002::/16` (6to4) aren't denied as such: they take the embedded IPv4 address's class when that address is denied. Also denied:

- **the agent host's own addresses and their on-link prefixes**, which the `gateway` package reads from `net.InterfaceAddrs` (re-read when older than 30 s) and passes to `policy`, which stays pure (a new VM bridge appears with each workspace). This covers a public IPv6 address on the host, and the LAN's public IPv6 neighbours;
- `Egress.Deny`: extra prefixes from the supervisor (other workspaces' subnets, if a driver ever uses public ones).

**Exceptions** are exact `netip.AddrPort`s in `Egress.OpenAddrs` (named apart from #100's `allow`, which is `AllowHosts`): the Linux registry mirror (phase 9) and the tests' upstreams. Nothing else opens a denied address. Other workspaces are covered by the private ranges (Softnet's /29s and the kata CNI subnets are private), and by `Clients` on the way in.

### Tunnels

A CONNECT to an ordinary host passes the host check, resolves, is checked and dialled, and gets `200 Connection Established`. Before any client byte goes upstream, the gateway **peeks the ClientHello**:

- It reads the client's first bytes, at most 16 KiB and 10 s, and parses them as a TLS ClientHello with `crypto/tls` (a `tls.Server` over a connection that replays the bytes read and refuses writes, whose `GetConfigForClient` captures `ServerName` and aborts), so no new dependency.
- **A ClientHello with an SNI:** the SNI, lower-cased and without a trailing dot, must be the CONNECT host, else the connection is closed with a record (`refusal: sni-mismatch`). An IP-literal CONNECT may have no SNI; one with a name is a mismatch.
- **A ClientHello without an SNI:** tunnelled.
- **Not TLS** (the first byte isn't a handshake record, or nothing arrives in 10 s): tunnelled, except under deny-by-default to port 443, which is closed (`refusal: not-tls`).
- **Too long or malformed** (a handshake record whose ClientHello doesn't fit in 16 KiB, or doesn't parse): closed (`sni-mismatch`).
- Then the bytes already read are written upstream and both directions are copied. Server bytes flow to the client from the dial on, so a protocol where the server speaks first isn't held up by the peek.

**What it can't check** (the residual risk, [Risks](#risks-and-open-questions) item 10): domain fronting through the HTTP `Host` header inside the TLS session, and an Encrypted ClientHello whose outer SNI is the allowed host. Both need interception, which only the credentialed hosts get. Deny-by-default is best effort against egress by host name, not a boundary against a hostile VM: the credentials are the boundary, and they never enter the VM.

## TLS interception

After `200 Connection Established` for an intercepted host, the gateway runs a TLS server on the hijacked connection (TLS 1.2 or later, ALPN `http/1.1` only; 10 s handshake) with a leaf for the CONNECT's host, and hands the connection to an inner `http.Server`, as the fake does. Each request goes through `httputil.ReverseProxy` (`Rewrite`, never `Director`; no `X-Forwarded-*` headers) to a per-gateway upstream `http.Transport` over HTTP/1.1 (`ForceAttemptHTTP2: false`, so an upgrade request is never sent over h2), dialling through the egress dialer and verifying with `UpstreamRoots`.

**WebSocket** (codex's `GET /backend-api/codex/responses`): the upgrade request gets its credential like any other; when the upstream answers 101, `ReverseProxy` hijacks both sides and copies bytes. Frames are not inspected or rewritten, and no WebSocket library is needed. The upgraded pair is tracked like a tunnel ([Close](#close)).

## The CA

- **Source:** the agent host's state dir, `gateway-ca/` (0700): `ca.pem` (0644) and `ca.key` (0600, PKCS #8, ECDSA P-256). #103's `host init` doesn't create them. **Phase 4 adds the `gatewayca` step** (PR 6), driver-agnostic, after `statedir`: it's `missing` without `gateway-ca/`, `wrong` for a bad mode or owner, a key that doesn't match the certificate, or a certificate that isn't a CA, and `outdated` within 30 days of `NotAfter` or when the name constraints differ from this binary's (a different host list, or a CA without the IP exclusions, made before them). Its `Apply` is **the rotation** below, for all three states (for `missing` there is no old pair to move). `host init` applies every step that isn't ok, and phase 5's `supervise` runs init's steps at start, so an `outdated` CA is rotated by either, unattended; that's safe because a running gateway keeps the CA it loaded and each new workspace's bundle carries the new public half. The kata host init (phase 9) reuses the step.
- **The certificate:** CN `factory-agent gateway CA`, valid for 1 year from `Generate`, `IsCA`, `MaxPathLenZero`, key usage cert-sign only, and **critical name constraints**: permitted dNSName subtrees for exactly the intercepted hosts (the Anthropic set, `chatgpt.com`, `github.com`, `api.github.com`); **excluded iPAddress subtrees for all of IPv4 and IPv6** (`ExcludedIPRanges: 0.0.0.0/0, ::/0`), since RFC 5280 constrains each name type on its own and a dNSName-only CA would still sign a leaf for any IP address; and permitted URI and email subtrees of only `invalid` (RFC 6761: no real name is under it), so a URI or email SAN fails too. Go's, OpenSSL's and Node's verifiers enforce them. A dNSName constraint also permits the name's subdomains (`api.github.com` under `github.com`); the gateway mints leaves only for the exact hosts, with dNSName SANs only. A new intercepted host means rotating the CA.
- **`ca.Load(dir)`:** opens the directory once (`os.OpenRoot`) and reads both files through it, so a rotation can't hand it the certificate of one pair and the key of the other. When the directory is missing (the instant between the rotation's two renames) it retries once after 100 ms. It refuses a key file that isn't a regular file of the user's with mode 0600, a certificate that isn't a CA, a key that doesn't match it, and an expired or not-yet-valid certificate (on the injected clock). The public half is what the supervisor puts in the bundle (`BundleExtras.CA`).
- **Leaves:** P-256, one per host, minted on first use, valid from 1 h ago for 7 days (capped at the CA's `NotAfter`), and re-minted when less than 1 h is left. `ca` takes a clock for this; the gateway's tests give it the real one, because git and Go's TLS verify leaves on the real clock (as the fake's CA notes), and only `ca`'s own tests use a fake one. The cache is an LRU of 64 entries per `CA`, behind a mutex; the intercepted host list bounds it anyway. Leaves exist only in memory.
- **The rotation** (the step's `Apply`, and `host init --rotate-ca`, which forces it on an ok pair): under `<statedir>/host.lock`, as every apply, it removes a leftover `gateway-ca.new/` and `gateway-ca.old/`, generates a new pair into `gateway-ca.new/` (`ca.Generate`), then renames `gateway-ca/` to `gateway-ca.old/` (when it exists) and `gateway-ca.new/` to `gateway-ca/`. A crash between the renames leaves `gateway-ca/` missing, which the next `init` or `supervise` sees as `missing` and rotates again. Then it removes `gateway-ca.old/`: running gateways keep the CA they loaded, in memory, and each new workspace loads the new one. An ok pair (valid, more than 30 days left, this binary's constraints) is replaced only by `--rotate-ca`.

## ChatGPT refresh

**The algorithm (`chatgpt.Refresher`):**

- **`Login`:** `{AccessToken, RefreshToken, IDToken, AccountID, Issuer, ClientID string; LastRefresh time.Time}`. Phase 5's `factory-agent login codex` writes it from `~/.codex/auth.json`, `Issuer` and `ClientID` from `tokens.oauth_provenance` when the file has it (codex `main` records the OAuth authority a refresh token was issued by; the pinned rust-v0.163.0 doesn't), else empty. The import refuses an `Issuer` that isn't an absolute https URL without user info, query or fragment, or only one of the two. `String`, `Format` and `LogValue` redact.
- **`Store`:** `Load(ctx) (Login, error)` and `Save(ctx, Login) error`. Phase 5 implements it over a 0600 file in the supervisor's state dir, written atomically. After the import, that copy is the authoritative login (P1 §5).
- **One `Refresher` per supervisor,** shared by its gateways. Refresh tokens rotate on use, so two gateways refreshing on their own would spend each other's token.
- **`Current`** returns the cached login's access token and account. It refreshes first when the access token's `exp` claim (its JWT payload, decoded and not verified) is less than 5 minutes away, or, without a readable `exp`, when `LastRefresh` is more than 8 days old (codex's own rules).
- **`Unauthorized(used)`** follows an upstream 401. When the cached access token is no longer `used`, another caller has already refreshed it, so it returns the new one. Otherwise it refreshes.
- **Single flight:** one refresh runs at a time. Concurrent callers wait for its result, each on its own context.
- **Before the HTTP call,** the refresh reloads the `Store`. When the stored refresh token differs from the cached one (the owner re-imported), it takes the stored login and checks it again.
- **The request:** `POST <endpoint>` with `Content-Type: application/json` and the body `{"client_id": <client id>, "grant_type": "refresh_token", "refresh_token": <the store's>}`, through a client of its own (30 s, system roots, no proxy). The endpoint and client id are the login's provenance when it has one (`<Issuer>/oauth/token`, `ClientID`), as codex `main` refreshes, else the constants `TokenURL` and `ClientID`. It **refuses to send** an empty refresh token or `harness.Placeholder`.
- **The response:** 200 with a JSON object of at most 64 KiB, where `access_token`, `id_token` and `refresh_token` are all optional, as codex reads them: a missing one keeps the old value (a 200 with a new `refresh_token` and no `access_token` still saves the new refresh token and keeps the old access token), and `LastRefresh` becomes now. A 200 whose body isn't such an object is a failure of this refresh and **isn't retried**, since the refresh token may already be spent; it's logged, and the next refresh (the next 401, or `Current` within the window) tries again. An `id_token` whose `chatgpt_account_id` differs from `AccountID` is logged as an error, and `AccountID` doesn't change: the bundles carry it.
- **Write-back:** `Save` runs with `context.WithoutCancel` and 10 s, before the new token is used. If it fails, the new tokens are used from memory (the old refresh token is already spent), the error is logged, and `Save` is retried on every later `Current` until it succeeds.
- **Permanent failures** (codex's `classify_refresh_token_failure`): a 401; a 400 whose error code is `invalid_grant`; any status whose error code is `refresh_token_expired`, `refresh_token_reused` or `refresh_token_invalidated`. The error code is the body's `error` when it's a string, else `error.code`, compared case-insensitively. The refresher stops trying until the stored login changes, and `Err()` reports it, so phase 5 can stop leasing codex tasks and tell the owner to log in again and re-import.
- **Transient failures** (network, 5xx, a non-200 whose body doesn't parse, any other error code): up to 3 attempts in all (1 s, then 2 s between them, within 30 s). After that the caller gets the error, and the request goes out with the current token.

**In the gateway,** a chatgpt.com request with an `Authorization` is sent through a retrying `RoundTripper`. Its body is buffered when it's at most 1 MiB (an upgrade has none). On a 401 it reads that response's body (at most 64 KiB), calls `Unauthorized`, and resends once with the new token. When it can't (a request body over 1 MiB, a 401 body over 64 KiB, or a failed refresh), the client gets the original 401 unchanged (a long body is streamed on after the part already read). `Current` returns the cached token along with a refresh error; the gateway logs the error and sends that token, so a failed refresh shows up as the upstream's 401. Only when there is no token at all (no login loaded, `Load` failed) does chatgpt.com get 503 `no-login`. **The VM never refreshes:** its `auth.json` has placeholders and a fresh `last_refresh`, and a refresh it tries anyway is refused at CONNECT to `auth.openai.com` (`auth-host`), so its placeholder refresh token never leaves the gateway.

## The egress log

Every request, refusals included, is one `Record`. The supervisor (phase 5) writes each one as a `factory.egress` transcript line, outside the VM's reach, before it uploads the transcript:

```json
{"type":"factory.egress","at":"2026-10-22T10:00:00.123Z","mode":"intercept","method":"POST","host":"api.anthropic.com","port":443,"path":"/v1/messages","status":200,"injected":"anthropic-oauth","client":"placeholder","refusal":"","up":1834,"down":20511,"ms":5120}
```

- **`mode`** says where the request was handled: `connect` (an intercepted host's CONNECT itself), `intercept` (a request inside one), `tunnel` (any other CONNECT: one line per tunnel, written when it closes, with its bytes), `forward` (plain HTTP), `dispatcher` or `accept` (a peer outside `Clients`). A non-empty `refusal` marks a refusal, whatever the mode.
- **`injected`:** `""`, `anthropic-oauth`, `anthropic-key`, `chatgpt`, `git:<owner>/<repo>`, `github-api` or `dispatcher-token`. **`refusal`:** one of the codes in the tables above.
- **`path`:** the URL path only, never the query, cut at 512 bytes on a UTF-8 boundary; empty for CONNECT and tunnels.
- **What is never logged:** header values, bodies, queries, or the resolved addresses.
- **Upgrades** are logged with status 101 when they end. `at` is the request's start, on the injected clock; `ms` is its duration.
- **A cap of 50,000 records per gateway;** after that, one `{"type":"factory.egress-truncated","dropped":N}` at `Close`. An `npm install` makes thousands of requests, and the transcript's cap is 20 MB.

**slog:** refusals and `client: other` are logged at info, with the refusal code and the credential class but not the host or path, which the VM chose. Every request is logged at debug. ChatGPT refreshes are logged at info, with the new expiry; their failures at error, with the status and an error code from the known list, never the body. No level ever logs a token, a header value or a body.

## Limits and timeouts

| What | Default (`Limits`) |
|---|---|
| request headers (both servers) | 10 s to read (`ReadHeaderTimeout`), 64 KiB (`MaxHeaderBytes`); idle keep-alive 90 s |
| connections per gateway | 512 at once; more are closed at accept |
| a peer outside `Clients` | closed at accept, with one record (`mode: accept`, `refusal: peer`) |
| DNS / dial / TLS handshake (each side) | 5 s / 10 s / 10 s |
| upstream response headers | 5 minutes (git packs, model calls) |
| a tunnel's ClientHello peek | 16 KiB, 10 s |
| tunnels and upgraded connections | no idle or total timeout: TCP keepalive (30 s), and `Close` ends them with the workspace |
| ChatGPT buffered body for a retry | 1 MiB |

## Close

`Close` (lesson from #109's finding 6: a hijacked connection is no longer the server's) and `Serve`'s context both:

1. mark the gateway closed and close the listener, so no new connection is accepted;
2. close **every tracked connection**. Each accepted connection is wrapped, and tracked from accept until it closes, so a hijacked one (CONNECT, mid-handshake, intercepted, upgraded) is covered. Tunnels' upstream sides are tracked too. A `ReverseProxy` upgrade ends when its client side closes and then closes its backend; an in-flight request is cancelled through its context;
3. close the inner server and wait for every handler and copy goroutine (one `WaitGroup`); the tunnels and upgrades this ends write their records then, so no `Sink.Egress` happens after `Close` returns;
4. close the upstream transports' idle connections, write the truncation record, and return `nil` (later calls also return `nil`).

The supervisor calls it after the VM is destroyed, then adds the records to the transcript.

## Tests

Never the real Anthropic, OpenAI or GitHub: every upstream is an `httptest` TLS server (git: `git http-backend` behind one, as in harnesstest), reached through `Config.Resolver` (the real names mapped to `127.0.0.1`), `Config.UpstreamRoots` and an `Egress.OpenAddrs` exception for the server's address. The fake token endpoint counts its calls and checks the body. CI runs every test with `-race`. No test runs claude, codex, tart or sudo.

| PR | Tests |
|---|---|
| 2 policy | the deny table: a row per range with its class, IPv4-mapped addresses, NAT64 and 6to4 embedding, the more specific range winning, zones, the host's own prefixes and `Deny` as inputs, `OpenAddrs`; the host check: an exact entry, a wildcard (a name under it, two labels under it, the apex refused), with and without a port, an IP literal under deny-by-default, `default`; a row per refusal code (status, header, body), so every code in [the table](#refusals-and-errors) has one; the `InfraCookies` filter |
| 3 proxy core | the dialer never dials a hostname (a resolver that answers differently the second time); a name with mixed answers; the host's interface addresses, re-read; tunnels end to end; plain HTTP forwarding; `not-proxy` for an origin-form request and for an absolute-form `https://` one; `bad-target`; `Clients`: the exact address accepted, an IPv4-mapped peer (`::ffff:a.b.c.d`) matched to its IPv4 entry, another address of the /29 refused, `Serve` refusing a host address in `Clients`; the limits; `Close` with a pending CONNECT, a tunnel in mid-copy and a handler in flight (as the fake's `TestClose*`), and no record after it; the record shape (a golden, `-update`) and the cap |
| 4 ca | `Generate`'s constraints: a leaf for an unlisted host fails Go's verification, and so does a leaf with an IP SAN, one with a URI SAN and one with an email SAN; `Load`'s refusals (mode, owner, mismatch, expiry on a fake clock); `Load` across a rotation (the directory renamed between the two reads, and missing once); leaf expiry and re-mint; the LRU bound |
| 5 interception | SNI mismatch at the handshake; `Host` normalisation (`:443`, upper case and a trailing dot accepted; `:8443` and another name `host-mismatch`); `CONNECT`, an absolute-form request and `OPTIONS *` inside a tunnel get `not-proxy`; `port` (a CONNECT to an intercepted host on another port); a WebSocket upgrade passed through (a 101 echo server) and closed by `Close`; the ClientHello peek: a matching SNI tunnelled, another SNI closed (`sni-mismatch` record), no SNI, an IP-literal CONNECT with a name, non-TLS bytes in `default` and under deny-by-default to 443 (`not-tls`) and to another port, a ClientHello split across reads, one over 16 KiB, the timeout (a short `Limits` value), and a server that speaks first |
| 6 host step | `gatewayca` `Check`/`Apply` with the host test fakes: missing, wrong mode, mismatch, outdated by date, by host list and by missing IP exclusions, each applied through `Init` (the rotation, `.old` removed after the swap); an ok pair left alone by `init` and rotated by `--rotate-ca`; a leftover `.new`; a crash between the renames (missing, rotated by the next `Init`); `init --dry-run` lists it (the golden) |
| 7 GitHub, dispatcher | a table per row of [github.com](#githubcom) and [the dispatcher](#the-dispatcher): the status, the refusal code, whether the upstream was reached, the `Authorization` it got; carriers and `Cookie` stripped; `plain-http` to github.com; `plain-only` (a CONNECT to the dispatcher); the dispatcher on another port (`dispatcher-route`); the dispatcher transport reaching a `127.0.0.1` httptest dispatcher (which the egress dialer refuses) and an https one through `UpstreamRoots`, and refusing to dial any other address; a heartbeat's `Set` applied to the next request; `git clone` and a refused `git push` through the gateway |
| 8 model injection | the Anthropic presence rule in both modes; chatgpt.com's injection and account header, only with `Authorization`; the cookie filter: a chatgpt.com request with `__oailb`, `_cfuvid` and a `session` cookie reaches upstream with the first two only and records `client: placeholder`; `plain-http` to a model host; `auth.openai.com` refused (CONNECT and plain); the retrying `RoundTripper` over a fake `ChatGPTSource`: a 401 retried once, a body over 1 MiB not retried, a 401 whose body is over 64 KiB passed through unchanged, a failed refresh passing the 401 on; `no-login` (503) when the source has no token |
| 9 refresher | on a fake clock: proactive by `exp` and, without one, by `LastRefresh`; on 401 when `used` is already stale; single flight (N concurrent callers, one call); a re-imported store; write-back and a failed `Save` retried; the endpoint and client id from the provenance, else the constants; the import refusing a bad `Issuer`; a 200 without `access_token` keeping the old one and saving the new refresh token; a 200 with a bad body not retried; permanent failures (401, 400 `invalid_grant`, `REFRESH_TOKEN_REUSED` upper-case in `error.code`) and transient ones (5xx, network) with their backoff; the placeholder never sent |
| 10 conformance | `gatewaycases` run by both the fake and the gateway; `vmrun`'s in-process end-to-end test also through the real gateway, with the no-credential walk ([workspace-run.md](workspace-run.md#tests), layer 4) |

Every table asserts that no record and no log line contains a token (the redactor's patterns and the test's literal tokens), as the fake's `assertNoTokens` does.

## Delivery

A gh stack on phase 3's. The sizes count `_test.go` files, as ccf-review's size check does, and leave out testdata and generated files. PRs 2, 4, 6 and 9 are each one package (or one host step) and come with its table tests; the proxy and its interception are split so each stays under 1000 lines. A PR that still ends over 1000 takes the `size-exception` label, which the owner consented to for P1 phase 4, with the reason in its body, rather than a re-cut mid-phase.

1. docs(agent): this LLD (~370).
2. feat(agent): gateway policy: host classes, the egress check (modes, exact and wildcard allow hosts, the deny table, open addresses), the refusal codes and bodies, `InfraCookies`; pure (~800: ~300 code, ~500 tests).
3. feat(agent): gateway proxy core: the listener and the exact-address `Clients`, CONNECT tunnels, plain forwarding, the egress dialer, the egress log, limits, `Close` (~900: ~450, ~450).
4. feat(agent): the gateway CA: `ca.Generate` with its constraints, `ca.Load`, leaf minting and the cache (~550: ~250, ~300).
5. feat(agent): gateway TLS interception: the intercept loop, `Host` normalisation, the WebSocket passthrough, and the tunnels' ClientHello peek (~850: ~350, ~500).
6. feat(agent): the `gatewayca` host step, the rotation and `host init --rotate-ca` (~500: ~200, ~300).
7. feat(agent): gateway GitHub and dispatcher routes: `policy`'s git and dispatcher rules, Basic by repo, api.github.com `GET` only, the run token, the dispatcher transport (~950: ~400, ~550).
8. feat(agent): gateway model injection: the Anthropic and chatgpt.com routes, the cookie filter, `auth.openai.com`, the retrying `RoundTripper` over `ChatGPTSource` (~700: ~300, ~400).
9. feat(agent): the `chatgpt` refresher: `Login` with its provenance, `Store`, `Refresher` with single flight and write-back (~900: ~400, ~500).
10. test(agent): `gatewaycases` shared with the fakegateway, and `vmrun` end to end through the real gateway (~600).

PRs 2 to 5 and 7 to 10 build on darwin and linux; PR 6 only adds a step to the tart list (the kata list is phase 9's). None needs #100 or #101 merged: the egress mode and the run token come in as `Config` values, which phase 5 fills from the contract.

## UNVERIFIED (the one list)

Each item is a constant, a `Config` field or a check, so a fix doesn't change the design.

**What the codex source settles.** The refresh was read from the pinned openai/codex `rust-v0.163.0` (`codex-rs/login/src/auth/manager.rs`, `codex-rs/login/src/oauth/client.rs`, `oauth/error.rs`, `token_data.rs`) and from `main` at `0eb98b4617` (2026-10-22), and never run; Phase 0 made no call to `auth.openai.com`. The pinned source gives the constants (`REFRESH_TOKEN_URL`, `CLIENT_ID`), the three-field JSON body (no `refresh_reason`, which `main` adds), the three optional response fields, the error code taken from `error` or `error.code` and compared case-insensitively, which failures are permanent, the 5-minute window, the 8 days and the `last_refresh` fallback for a token without a readable `exp`. It has no `oauth_provenance`: that is `main`'s. codex's own env overrides (`CODEX_REFRESH_TOKEN_URL_OVERRIDE`, `CODEX_APP_SERVER_LOGIN_CLIENT_ID`) don't apply to the gateway.

**Still unverified:**

1. `auth.openai.com` answers the request as codex's code expects: 200 with the fields above, and the statuses and error codes the permanent rule lists. Nothing has called it.
2. Refresh tokens are single-use (implied by `refresh_token_reused`).
3. The access token is a JWT with `exp`.
4. chatgpt.com answers an expired access token with 401, not 403.
5. The `oauth_provenance` path (`<Issuer>/oauth/token` with its `client_id`) is read from `main` only; no owner's `auth.json` with it has been seen.
6. Every client in the VM accepts the CA's name constraints, the IP exclusions and the `invalid` URI and email subtrees included ([Risks](#risks-and-open-questions), item 4).
7. Softnet drops a VM's packets from an address it wasn't leased (the client pin's precondition; phase 5's manual check).
8. Departures D1 to D5 from `gw.py` ([the table](#departures-from-phase-0s-gwpy)).
9. The model hosts' path allow-lists cover every call claude-code and codex make; phase 8's first run checks the egress log for `model-path` refusals. Phase 0 names codex's analytics call without its path: it is allowed only if it is under `/backend-api/codex/`. Other `wham` and `ps` paths (codex's usage, for one) are refused until a run shows a harness needs them; `/api/oauth/` stays refused whatever a run shows.

Phase 8's first codex run through the real gateway checks 3, 4, 6 and 8; phase 5's manual check covers 7. A forced refresh against the real endpoint checks 1, 2 and, for an owner whose `auth.json` has provenance, 5; it's run by the owner, never by an agent.

## Risks and open questions

1. **gh's GraphQL reads.** `gh pr view` and the `gh-pr-review` extension `POST /graphql`. **Decided (2026-10-22, revised):** queries pass ([GraphQL reads](#graphql-reads)); mutations and subscriptions are refused, and GitHub writes stay on the factory CLI.
2. **The ChatGPT refresh is UNVERIFIED** ([the list](#unverified-the-one-list)), and if the owner keeps using the same login in their own codex, one side ends up holding a spent refresh token (P1 §5: log in again and re-import).
3. **Name constraints tie the CA to the host list:** adding a host makes the `gatewayca` check `outdated`, and the next `init` or `supervise` rotates the CA.
4. **Name-constrained roots** must be accepted by every client in the VM: claude's runtime (BoringSSL), codex (rustls), git (GnuTLS on Ubuntu), Node and Python (OpenSSL). All of them implement the extension, but none was tried with it in Phase 0, whose CA was mitmproxy's unconstrained one. PR 4's tests run Go's verifier and PR 10's end-to-end test adds git's (the real `git` binary through the gateway); phase 8's first run checks claude and codex. **Decided (2026-10-22):** the name-constrained CA stays, with this fallback: if a client refuses only the URI or email subtrees, `Generate` drops those first; if it refuses the constraints, `Generate` drops them all (constants) and the CA rotates, and the key's 0600 file is then the only protection, as in P1.
5. **The Anthropic presence rule** follows Phase 0's claude-code 2.1.x. A claude that authenticates a call without any carrier would get no credential: forwarded without, and visible as a 401 in the log (departure D4).
6. **#100's comment** says deny-by-default allows "only Allow, plus the git host and the dispatcher". The gateway also keeps the model hosts and api.github.com, its own routes, and deny-by-default is best effort against a hostile VM ([tunnels](#tunnels)). The contract's comment should say so (a one-line contract PR): the owner's call.
7. **IPv6:** the denied ranges cover the special-purpose blocks and the host's own prefixes, but not every public range a LAN might route. The network layer (Softnet, iptables) is still the first line.
8. **Tunnels have no idle timeout.** A VM can hold up to 512 connections open for its lifetime; the workspace's wall clock bounds them. **Decided (2026-10-22):** no idle timeout on tunnels or WebSockets.
9. **The VM's DNS goes around the gateway.** workspace-host's pf anchor passes UDP 53 from the bridges to the Mac, so the VM resolves through the Mac's resolver: a channel (DNS exfiltration) outside the gateway and its egress log, which deny-by-default doesn't see either. With an explicit proxy the VM needs no DNS for proxied traffic, since the gateway resolves. **Phase 5 decides:** its manual check blocks the VM's DNS (pf drops port 53 from the bridges) and keeps it blocked if apt, npm and docker through the proxy still work; otherwise it accepts the channel and says so here and in workspace-host.md. Blocking it changes the `pf` step's anchor, so `host init` re-applies it.
10. **What the tunnel peek can't see:** domain fronting through the HTTP `Host` inside TLS, and an Encrypted ClientHello whose outer SNI is the allowed host ([tunnels](#tunnels)). Closing them needs interception, which only the credentialed hosts get; the credentials, never in the VM, are the boundary.
