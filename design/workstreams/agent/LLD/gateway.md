# LLD — The workspace gateway (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: P1 phase 4 · Packages: `internal/agent/gateway`, `internal/agent/gateway/{policy,ca,chatgpt}`, `internal/agent/host` (the `gatewayca` step)
> Design: [P1 §3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [§5](../../../proposals/P1-agent-workspaces.md#5-credentials-injected-by-the-gateway-never-in-the-workspace), Decisions 4, 6, 7 and 9 · Phase 0: [Q4](../../../proposals/P1-phase0-report.md#q4-does-claude-code-work-behind-a-tls-intercepting-gateway-with-a-placeholder-token), [Q5](../../../proposals/P1-phase0-report.md#q5-can-the-gateway-carry-codexs-subscription-auth), [egress](../../../proposals/P1-phase0-report.md#the-gateway-and-egress-on-the-mac) · The VM side: [workspace-run.md](workspace-run.md#phase-4s-gateway)

The gateway is the workspace's only route out and the only holder of its credentials. It's an explicit forward proxy (absolute-form HTTP and CONNECT) that TLS-intercepts only the credentialed hosts, adds their credentials and applies the per-host rules below. Every other host gets a plain tunnel, after an egress check. It replaces Phase 0's mitmproxy addon (`gw.py`) and implements the policy the test-only `fakegateway` (#109) models. One gateway runs per workspace (Decision 7), as a library inside the phase 5 supervisor, which starts it on the workspace's gateway address and closes it with the workspace.

## Refinements of P1

- **The dispatcher route is narrower than the run token.** The run token (#101) authorises the task's prompt, context, output schema, effects and transcript upload. Through the gateway the VM gets **only `POST` effects** ([the dispatcher](#the-dispatcher)): the bundle already holds the rest, and the supervisor uploads the transcript after it has re-redacted it and added the `factory.egress` lines, so a VM upload could replace that audit.
- **Clients are pinned to their workspace.** On the Mac, pf lets every Softnet bridge reach the gateway port on *any* of the Mac's addresses, so VM A could reach workspace B's gateway on B's bridge. Each gateway therefore accepts connections only from its own VM's address (`Config.Clients`).
- **The CA is name-constrained** to the intercepted hosts, so a leaked CA key can mint certificates only for them, not for any site the VM visits ([the CA](#the-ca)).
- **`auth.openai.com` is refused at CONNECT** and never intercepted: the gateway owns ChatGPT refresh, and nothing the VM sends there is forwarded.
- **github.com web paths stay usable.** The fake fails closed for every path of a repo that isn't rw. The gateway keeps that for git's paths, and forwards other `GET`/`HEAD` requests (release downloads, archives) anonymously, since `setup` scripts install tools from GitHub releases.

## Packages

| Package | What | Rules |
|---|---|---|
| `internal/agent/gateway/policy` | the decisions: host classes, the github.com, api.github.com and dispatcher rules, the egress check, the refusal codes and bodies | **pure**, like `internal/rules`: no network, no clock, no I/O. Table-tested per row of [the host table](#the-host-table) |
| `internal/agent/gateway/ca` | the agent host's CA: `Generate`, `Load`, leaf minting and the leaf cache | crypto and files only |
| `internal/agent/gateway/chatgpt` | `Login`, the `Store` interface, `Refresher` (proactive and on-401 refresh, single-flight, write-back) | one `Refresher` per supervisor, shared by its gateways |
| `internal/agent/gateway` | the proxy: listener, CONNECT, tunnels, interception, upstream transports, the egress dialer, the egress log, `Close` | uses the three above |
| `internal/agent/host` | the `gatewayca` step and `host init --rotate-ca` | as the other steps ([workspace-host.md](workspace-host.md#steps-tart-driver)) |

No new dependency: TLS, `httputil.ReverseProxy` (which already passes a `101 Switching Protocols` through by hijacking both sides) and `net/netip` are the standard library's. No `factory-agent` subcommand is added: a manual entry would need a subscription store and a lease, both phase 5's, and would be one more credential path to review. Tests drive the library in process.

### Sharing with the fakegateway

**The fake stays independent and imports nothing from `gateway`.** It's the oracle that phase 3's tests (`pipeline`, `effector`, `vmrun`) were written against and reviewed hard; if both used one policy package, a bug there would be a bug in both, and no test would see it. What they share is **data**: PR 7 moves the fake's route table (`TestRoutes`) into `internal/agent/internal/gatewaycases` (test-only), and both packages run every case. A case gives the request, the status, whether an upstream is reached and the credential class it gets. Refusal bodies differ (`fakegateway:` and `factory gateway:`), so the cases compare the refusal *code* (`X-Factory-Gateway-Refusal`). Where the gateway deliberately differs, the case says `gatewayOnly` or `fakeOnly` and why: github.com web paths, ports other than 443, the deny list, the client pin, the canonical-path check only where a path is matched.

## API

```go
package gateway

type Config struct {
    TaskID, RunID string           // required; TaskID binds the dispatcher route
    Clients       []netip.Prefix   // the VM's addresses; any other peer is closed. Required
    CA            *ca.CA           // required
    Git           *runner.Credentials // the job's: read on every request, so heartbeats' Set/SetReads apply at once
    GitHost       string           // default "github.com"; the lease's gitCredential.host
    RunToken      string           // the lease's runToken (#101); required
    Dispatcher    *url.URL         // the supervisor's dispatcher base URL (http or https), required
    Anthropic     *AnthropicAuth   // nil: none. Only for a claude task
    ChatGPT       ChatGPTSource    // nil: none. Only for a codex task (*chatgpt.Refresher)
    Egress        policy.Egress    // the task's mode and allow list, extra denied prefixes, exceptions
    Sink          Sink             // the egress log; required
    Clock         clock.Clock      // records' times, leaf validity
    Logger        *slog.Logger
    Resolver      Resolver         // default net.DefaultResolver; tests map names to test servers
    UpstreamRoots *x509.CertPool   // default: the system roots; tests: their httptest CAs
    Limits        Limits           // zero: the defaults below
}
type AnthropicAuth struct{ OAuthToken, APIKey string } // exactly one; String, Format and LogValue redact
type ChatGPTSource interface {
    Current(ctx context.Context) (chatgpt.Token, error)                  // refreshed ahead of expiry
    Unauthorized(ctx context.Context, used chatgpt.Token) (chatgpt.Token, error) // after an upstream 401
}
type Sink interface{ Egress(Record) } // called synchronously, never after Close returns; must not block
type Resolver interface{ LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) }

func New(cfg Config) (*Gateway, error)              // validates; starts nothing
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error // until ctx ends or Close; nil then
func (g *Gateway) Close() error                     // idempotent; see [Close](#close)
```

- **The supervisor (phase 5)**, per lease: loads the CA (`ca.Load`, so a rotated CA applies to the next workspace without a restart), builds the `Config` from the job, `net.Listen("tcp", <gateway address>:<workspace.gatewayPort>)` after the driver's `Start`, runs `Serve` in a goroutine, and closes the gateway after `Destroy`. It gives a gateway only its task's model credential: a claude task's gateway has no ChatGPT login, and a codex task's has no Claude token.
- **Lease refresh needs no new call.** The worker's heartbeat already swaps refreshed git tokens into `job.Git` (`Set`, `SetReads`), and the gateway reads it on every request, as `gitcred` does. The run token isn't refreshed: it lives exactly as long as the lease (#101).
- `New` refuses: an empty `TaskID`, `RunID`, `RunToken`, `Clients` or `Sink`; a `Dispatcher` that isn't an absolute http(s) URL without user info, query or fragment; an `AnthropicAuth` with neither or both fields; `RunToken` equal to `harness.Placeholder`. Errors are `*ConfigError{Field}` and never carry a value.

## The host table

Host names are compared lower-cased and without a trailing dot, exactly (no suffixes): `downloads.claude.ai` is an ordinary host. A CONNECT names `host:port`, and a port is required. The intercepted hosts take CONNECT on port 443 only; any other port is refused (`port`). Plain HTTP to an intercepted host is refused (`plain-http`), and so is a request that isn't in proxy form (`not-proxy`, 400).

| Host | Methods and paths | Credential | Refused |
|---|---|---|---|
| `api.anthropic.com`, `claude.ai`, `console.anthropic.com`, `platform.claude.com` | all | the request carried `Authorization` or `X-Api-Key` (present, whatever its value): every carrier is stripped, then `Authorization: Bearer <OAuthToken>` or `X-Api-Key: <APIKey>`, per the gateway's `AnthropicAuth`, not per the carrier the VM used. No carrier: stripped and forwarded without one (Phase 0: event logging and the MCP registry are called that way) | — |
| `chatgpt.com` | all, the WebSocket upgrade included | with `Authorization` present: carriers stripped, `Authorization: Bearer <access>` and `ChatGPT-Account-Id: <account>` set from `ChatGPT.Current`. A 401 is retried once ([refresh](#chatgpt-refresh)). Without it: stripped, no credential | — |
| `auth.openai.com` | none | — | every CONNECT and plain request: 403 `auth-host` |
| the git host (`github.com`) | see [github.com](#githubcom) | `Basic x-access-token:<token>` by the repo in the path | receive-pack for a repo that isn't rw, malformed git requests, non-`GET`/`HEAD` web requests |
| `api.github.com` | `GET`, `HEAD`, any path | `Authorization: Bearer <task token>` (the lease's `gitCredential`); none when it's empty | every other method: 403 `api-write` |
| `dispatcher.factory.internal` (`runbundle.DefaultDispatcherURL`) | absolute-form `http` only, port 80; see [the dispatcher](#the-dispatcher) | `Authorization: Bearer <RunToken>` | every other route: 403 `dispatcher-route`; CONNECT: 403 `plain-only` |
| everything else | CONNECT: a tunnel; absolute-form `http`: forwarded | none; only `Proxy-Authorization` is dropped | the [egress check](#egress) |

**Carriers** are `Authorization`, `Proxy-Authorization`, `X-Api-Key`, `Cookie` and URL user info. On every injected route all of them are stripped before the credential is set, so the VM's placeholder is replaced and never compared (workspace-run.md: "always replace, never match placeholders"). For the audit, each record says what the VM sent (`client`: `none`, `placeholder` when every carrier is `harness.Placeholder` in the forms the fake's `classify` accepts, else `other`). An `other` is logged as a warning, without its value: a credential reached the VM.

**Inside an intercepted tunnel** each request's `Host` must be the CONNECT's host (else 400 `host-mismatch`), and its upstream is always that host on 443, never a name taken from the request. The TLS handshake's SNI must be empty or that host (else the handshake fails). The canonical-path check (`RawPath == ""` and `path.Clean(Path) == Path`, else 400 `bad-path`) runs only where a path is matched (the git host and the dispatcher): api.github.com takes encoded branch names (`feature%2Fx`), and its rule is the method alone.

### github.com

A path is `/<owner>/<repo>[.git]/<rest>`, the repo lower-cased without `.git`. **A git request** is one whose repo segment ends in `.git` (any case), or whose `rest`, lower-cased, is `info/refs`, `git-upload-pack` or `git-receive-pack`. Every other request is **a web request**.

The repo's access comes from `Config.Git` at the time of the request, only from credentials whose `host` is the git host: the task credential's `repo` is **rw** when its `permissions.contents` is `write`, else **read**; each read credential's `repo` and `readRepos` are **read**, with that credential's token. The task token wins for its repo, as in `gitcred`. Any other repo is **unknown**.

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

### The dispatcher

The only route is `POST /api/v1/tasks/<TaskID>/effects`: the path is compared segment by segment, after the canonical-path check, with `<TaskID>` exactly `Config.TaskID`; any query is allowed, as for the fake. It's fixed in `policy`, with no `Config` knob, so a supervisor can't widen it by mistake. The upstream is `Config.Dispatcher`: its scheme and host, its path joined in front of the request's, and `Host` set to it. That upstream is the supervisor's own configuration (on the Mac, `127.0.0.1:8800`), so it skips the egress check, which exists to stop VM-chosen destinations.

| Route (`/api/v1`) | Run token (#101) | Through the gateway | Why |
|---|---|---|---|
| `POST /tasks/{id}/effects` | yes | **yes** | the Effector in the VM |
| `GET /tasks/{id}/prompt`, `/output-schema`, `/context/{n}` | yes | no | in the bundle (`PrepareBundle` on the host) |
| `PUT /runs/{id}/transcript` | yes | no | the supervisor uploads after re-redacting and adding `factory.egress` lines; a VM upload could replace them |
| anything else | no | no | — |

## Egress

Every destination the VM names (a CONNECT that isn't intercepted, an absolute-form `http` request) and every upstream dial for an intercepted host goes through `policy.Egress` and one dialer:

1. **The host check.** Under `Mode: deny-by-default` (#100's `environment.egress`), the host must be in `AllowHosts` (exact, lower-cased, without a trailing dot; with the port when the entry has one), else 403 `egress-not-allowed`. The intercepted hosts are the gateway's own routes, so they are never subject to the mode: the run's git host, the dispatcher and its model always work. `default` skips this step.
2. **Resolve, then check every address.** An IP literal is used as it is; a name is resolved once (`Resolver.LookupNetIP(ctx, "ip", host)`, 5 s). If **any** address is denied, the request is refused (403 `egress-denied`, with the denied class in the body, never the address): a name that mixes public and private answers is refused, not filtered.
3. **Dial the checked addresses**, in order, as literal `ip:port` strings, so nothing is resolved again (no DNS rebinding). The dialer's `Control` checks the address again before the connect, in case a later change dials anything else. Upstream TLS still verifies against the host name.

**Denied** (`policy.DefaultDeny`), with IPv4-mapped IPv6 addresses unmapped first, and an address with a zone always denied:

- IPv4: `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10` (CGNAT, Tailscale), `127.0.0.0/8`, `169.254.0.0/16` (link-local, metadata), `172.16.0.0/12`, `192.0.0.0/24`, `192.168.0.0/16`, `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4` (with `255.255.255.255`);
- IPv6: `::/128`, `::1/128`, `100::/64`, `2001:db8::/32`, `fc00::/7` (ULA), `fe80::/10`, `ff00::/8`; and `64:ff9b::/96` (NAT64) or `2002::/16` (6to4) when the embedded IPv4 address is denied;
- **the agent host's own addresses and their on-link prefixes**, from `net.InterfaceAddrs`, re-read when older than 30 s (a new VM bridge appears with each workspace). This covers a public IPv6 address on the host, and the LAN's public IPv6 neighbours;
- `Egress.Deny`: extra prefixes from the supervisor (other workspaces' subnets, if a driver ever uses public ones).

**Exceptions** are exact `netip.AddrPort`s in `Egress.Allow`: the Linux registry mirror (phase 9) and the tests' upstreams. Nothing else opens a denied address. Other workspaces are covered by the private ranges (Softnet's /29s and the kata CNI subnets are private), and by `Clients` on the way in.

## TLS interception

After `200 Connection Established` for an intercepted host, the gateway runs a TLS server on the hijacked connection (TLS 1.2 or later, ALPN `http/1.1` only; 10 s handshake) with a leaf for the CONNECT's host, and hands the connection to an inner `http.Server`, as the fake does. Each request goes through `httputil.ReverseProxy` (`Rewrite`, never `Director`; no `X-Forwarded-*` headers) to a per-gateway upstream `http.Transport` over HTTP/1.1 (`ForceAttemptHTTP2: false`, so an upgrade request is never sent over h2), dialling through the egress dialer and verifying with `UpstreamRoots`.

**WebSocket** (codex's `GET /backend-api/codex/responses`): the upgrade request gets its credential like any other; when the upstream answers 101, `ReverseProxy` hijacks both sides and copies bytes. Frames are not inspected or rewritten, and no WebSocket library is needed. The upgraded pair is tracked like a tunnel ([Close](#close)).

## The CA

- **Source:** the agent host's state dir, `gateway-ca/` (0700): `ca.pem` (0644) and `ca.key` (0600, PKCS #8, ECDSA P-256). #103's `host init` doesn't create them. **Phase 4 adds the `gatewayca` step** (PR 4), driver-agnostic, after `statedir`: it's `missing` without the pair, `wrong` for a bad mode or owner, a key that doesn't match the certificate, or a certificate that isn't a CA, and `outdated` within 30 days of `NotAfter` or when the name constraints differ from this binary's host list. Its `Apply` is `ca.Generate`. The kata host init (phase 9) reuses the step.
- **The certificate:** CN `factory-agent gateway CA`, valid for 1 year from `Generate`, `IsCA`, `MaxPathLenZero`, key usage cert-sign only, and **critical name constraints** permitting exactly the intercepted hosts (the Anthropic set, `chatgpt.com`, `github.com`, `api.github.com`). Go's, OpenSSL's and Node's verifiers enforce them. A new intercepted host, or another git host, means rotating the CA.
- **`ca.Load(dir)`:** refuses a key file that isn't a regular file of the user's with mode 0600, a certificate that isn't a CA, a key that doesn't match it, and an expired or not-yet-valid certificate (on the injected clock). The public half is what the supervisor puts in the bundle (`BundleExtras.CA`).
- **Leaves:** P-256, one per host, minted on first use, valid from 1 h ago for 7 days (capped at the CA's `NotAfter`), and re-minted when less than 1 h is left. The cache is an LRU of 64 entries per `CA`, behind a mutex; the intercepted host list bounds it anyway. Leaves exist only in memory.
- **`host init --rotate-ca`** generates a new pair into `gateway-ca.new/`, then renames the old directory to `gateway-ca.old/` and the new one into place. Running gateways keep the CA they loaded; each new workspace loads the new one. `gateway-ca.old/` is removed by the next `init`. Without `--rotate-ca`, `init` never replaces a valid pair.

## ChatGPT refresh

**The algorithm (`chatgpt.Refresher`):**

- **`Login`:** `{AccessToken, RefreshToken, IDToken, AccountID string; LastRefresh time.Time}`. Phase 5's `factory-agent login codex` writes it from `~/.codex/auth.json`; `String`, `Format` and `LogValue` redact.
- **`Store`:** `Load(ctx) (Login, error)` and `Save(ctx, Login) error`. Phase 5 implements it over a 0600 file in the supervisor's state dir, written atomically. After the import, that copy is the authoritative login (P1 §5).
- **One `Refresher` per supervisor,** shared by its gateways. Refresh tokens rotate on use, so two gateways refreshing on their own would spend each other's token.
- **`Current`** returns the cached login's access token and account. It refreshes first when the access token's `exp` claim (its JWT payload, decoded and not verified) is less than 5 minutes away, or, without a readable `exp`, when `LastRefresh` is more than 8 days old (codex's own rules).
- **`Unauthorized(used)`** follows an upstream 401. When the cached access token is no longer `used`, another caller has already refreshed it, so it returns the new one. Otherwise it refreshes.
- **Single flight:** one refresh runs at a time. Concurrent callers wait for its result, each on its own context.
- **Before the HTTP call,** the refresh reloads the `Store`. When the stored refresh token differs from the cached one (the owner re-imported), it takes the stored login and checks it again.
- **The request:** `POST <TokenURL>` with `Content-Type: application/json` and the body `{"client_id": <ClientID>, "grant_type": "refresh_token", "refresh_token": <the store's>}`, through a client of its own (30 s, system roots, no proxy). It **refuses to send** an empty refresh token or `harness.Placeholder`.
- **The response:** 200 with JSON of at most 64 KiB, where `access_token` is required and `id_token` and `refresh_token` are optional (a missing one keeps the old value). An `id_token` whose `chatgpt_account_id` differs from `AccountID` is logged as an error, and `AccountID` doesn't change: the bundles carry it.
- **Write-back:** `Save` runs with `context.WithoutCancel` and 10 s, before the new token is used. If it fails, the new tokens are used from memory (the old refresh token is already spent), the error is logged, and `Save` is retried on every later `Current` until it succeeds.
- **Permanent failures:** a 401, or a 400 whose `error` is `invalid_grant`, `refresh_token_expired`, `refresh_token_reused` or `refresh_token_invalidated`. The refresher stops trying until the stored login changes, and `Err()` reports it, so phase 5 can stop leasing codex tasks and tell the owner to log in again and re-import.
- **Transient failures** (network, 5xx, a bad body): up to 3 attempts in all (1 s, then 2 s between them, within 30 s). After that the caller gets the error, and the request goes out with the current token.

**In the gateway,** a chatgpt.com request with an `Authorization` is sent through a retrying `RoundTripper`. Its body is buffered when it's at most 1 MiB (an upgrade has none). On a 401 it reads that response's body (at most 64 KiB), calls `Unauthorized`, and resends once with the new token. When it can't (a body over 1 MiB, or a failed refresh), the client gets the original 401 unchanged. **The VM never refreshes:** its `auth.json` has placeholders and a fresh `last_refresh`, and a refresh it tries anyway is refused at CONNECT to `auth.openai.com` (`auth-host`), so its placeholder refresh token never leaves the gateway.

**UNVERIFIED** (the one list; each is a constant or a `Config` field, so a fix doesn't change the design): read from openai/codex `main` at `0eb98b4617` (2026-10-22: `codex-rs/login/src/auth/manager.rs`, `codex-rs/login/src/oauth/client.rs`), not from the pinned `rust-v0.163.0`, and never run; Phase 0 made no call to `auth.openai.com`.

1. `TokenURL` `https://auth.openai.com/oauth/token`, `ClientID` `app_EMoamEEZ73f0CkXaXp7hrann`, the JSON body above (codex `main` also sends an optional `refresh_reason`, left out), and the response's three fields.
2. The error codes (codex's `classify_refresh_token_failure`), and that refresh tokens are single-use (implied by `refresh_token_reused`).
3. The access token is a JWT with `exp`; codex's window of 5 minutes and its 8 days.
4. The pinned codex doesn't refresh with a non-JWT placeholder access token and a fresh `last_refresh` (codex `main` falls back to `last_refresh` when the token has no readable `exp`).
5. chatgpt.com answers an expired access token with 401, not 403.

The first codex run through the real gateway (phase 8's smoke run) checks 3 to 5. A forced refresh against the real endpoint checks 1 and 2; it's run by the owner, never by an agent.

## The egress log

Every request, refusals included, is one `Record`. The supervisor (phase 5) writes each one as a `factory.egress` transcript line, outside the VM's reach, before it uploads the transcript:

```json
{"type":"factory.egress","at":"2026-10-22T10:00:00.123Z","mode":"intercept","method":"POST","host":"api.anthropic.com","port":443,"path":"/v1/messages","status":200,"injected":"anthropic-oauth","client":"placeholder","refusal":"","up":1834,"down":20511,"ms":5120}
```

- **`mode`:** `connect` (an intercepted host's CONNECT itself), `intercept` (a request inside one), `tunnel` (one line per tunnel, written when it closes, with its bytes), `forward` (plain HTTP), `dispatcher` or `refused`.
- **`injected`:** `""`, `anthropic-oauth`, `anthropic-key`, `chatgpt`, `git:<owner>/<repo>`, `github-api` or `run-token`. **`refusal`:** one of the codes in the tables above.
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
| a peer outside `Clients` | closed at accept, with one `refused` record (`client`) |
| DNS / dial / TLS handshake (each side) | 5 s / 10 s / 10 s |
| upstream response headers | 5 minutes (git packs, model calls) |
| tunnels and upgraded connections | no idle or total timeout: TCP keepalive (30 s), and `Close` ends them with the workspace |
| ChatGPT buffered body for a retry | 1 MiB |

## Close

`Close` (lesson from #109's finding 6: a hijacked connection is no longer the server's) and `Serve`'s context both:

1. mark the gateway closed and close the listener, so no new connection is accepted;
2. close **every tracked connection**. Each accepted connection is wrapped, and tracked from accept until it closes, so a hijacked one (CONNECT, mid-handshake, intercepted, upgraded) is covered. Tunnels' upstream sides are tracked too. A `ReverseProxy` upgrade ends when its client side closes and then closes its backend; an in-flight request is cancelled through its context;
3. close the inner server and wait for every handler and copy goroutine (one `WaitGroup`), so no `Sink.Egress` happens after `Close` returns;
4. close the upstream transports' idle connections, write the truncation record, and return `nil` (later calls also return `nil`).

The supervisor calls it after the VM is destroyed, then adds the records to the transcript.

## Tests

Never the real Anthropic, OpenAI or GitHub: every upstream is an `httptest` TLS server (git: `git http-backend` behind one, as in harnesstest), reached through `Config.Resolver` (the real names mapped to `127.0.0.1`), `Config.UpstreamRoots` and an `Egress.Allow` exception for the server's address. The fake token endpoint counts its calls and checks the body. CI runs every test with `-race`. No test runs claude, codex, tart or sudo.

| PR | Tests |
|---|---|
| 2 core | the deny list: a table of addresses (each range, IPv4-mapped, NAT64 and 6to4 embedding, zones), a name with mixed answers, the host's interface addresses, `Allow`; the dialer never dials a hostname (a resolver that answers differently the second time); `deny-by-default`; tunnels end to end; plain HTTP forwarding; `Clients`; the limits; `Close` with a pending CONNECT, a tunnel in mid-copy and a handler in flight (as the fake's `TestClose*`), and no record after it; the record shape (a golden, `-update`) and the cap |
| 3 interception | `ca`: `Generate`'s constraints (a leaf for an unlisted host fails Go's verification), `Load`'s refusals (mode, owner, mismatch, expiry on a fake clock), leaf expiry and re-mint, the LRU bound; SNI and `Host` mismatches; port 443 only; a WebSocket upgrade passed through (a 101 echo server) and closed by `Close` |
| 4 host step | `gatewayca` `Check`/`Apply` with the host test fakes: missing, wrong mode, mismatch, outdated by date and by constraints; `--rotate-ca`'s renames; `init --dry-run` lists it (the golden) |
| 5 GitHub, dispatcher | a table per row of [github.com](#githubcom) and [the dispatcher](#the-dispatcher): the status, the refusal code, whether the upstream was reached, the `Authorization` it got; carriers stripped; a heartbeat's `Set` applied to the next request; `git clone` and a refused `git push` through the gateway |
| 6 models | the Anthropic presence rule in both modes; chatgpt.com's injection and account header; `auth.openai.com` refused; the refresher on a fake clock: proactive, on 401 with one retry, single flight (N concurrent callers, one call), a re-imported store, write-back and a failed `Save` retried, permanent and transient failures, the placeholder never sent, a body over 1 MiB not retried |
| 7 conformance | `gatewaycases` run by both the fake and the gateway; `vmrun`'s in-process end-to-end test also through the real gateway, with the no-credential walk ([workspace-run.md](workspace-run.md#tests), layer 4) |

Every table asserts that no record and no log line contains a token (the redactor's patterns and the test's literal tokens), as the fake's `assertNoTokens` does.

## Delivery

A gh stack on phase 3's (line counts without testdata):

1. docs(agent): this LLD (~300).
2. feat(agent): gateway core: `policy` (egress, refusals), the proxy, CONNECT tunnels, plain forwarding, the egress dialer, `Clients`, the egress log, limits, `Close` (~750).
3. feat(agent): gateway TLS interception: the `ca` package, leaf minting and cache, the intercept loop, the WebSocket passthrough (~550).
4. feat(agent): the `gatewayca` host step and `host init --rotate-ca` (~300).
5. feat(agent): gateway GitHub and dispatcher routes: `policy`'s git and dispatcher rules, Basic by repo, api.github.com `GET` only, the run token (~600).
6. feat(agent): gateway model routes: Anthropic and ChatGPT injection, `auth.openai.com`, the `chatgpt` refresher with write-back (~700).
7. test(agent): `gatewaycases` shared with the fakegateway, and `vmrun` end to end through the real gateway (~400).

PRs 2, 3 and 5 to 7 build on darwin and linux; PR 4 only adds a step to the tart list (the kata list is phase 9's). None needs #100 or #101 merged: the egress mode and the run token come in as `Config` values, which phase 5 fills from the contract.

## Risks and open questions

1. **gh's GraphQL reads are refused.** `gh pr view` and the `gh-pr-review` extension `POST /graphql`, which "GET only" refuses. Telling a query from a mutation means parsing the VM's GraphQL, which the PoC doesn't do. The skills' reads may need REST, or the Effector. Check at phase 8's first run.
2. **The ChatGPT refresh is UNVERIFIED** ([the list](#chatgpt-refresh)), and if the owner keeps using the same login in their own codex, one side ends up holding a spent refresh token (P1 §5: log in again and re-import).
3. **Name constraints tie the CA to the host list:** adding a host means `--rotate-ca`, which the `gatewayca` check flags as `outdated`.
4. **The Anthropic presence rule** follows Phase 0's claude-code 2.1.x. A claude that authenticates a call without any carrier would get no credential: forwarded without, and visible as a 401 in the log.
5. **#100's comment** says deny-by-default allows "only Allow, plus the git host and the dispatcher". The gateway also keeps the model hosts and api.github.com, its own routes. The contract's comment should say so (a one-line contract PR).
6. **IPv6:** the denied ranges cover the special-purpose blocks and the host's own prefixes, but not every public range a LAN might route. The network layer (Softnet, iptables) is still the first line.
7. **Tunnels have no idle timeout.** A VM can hold up to 512 connections open for its lifetime; the workspace's wall clock bounds them.
