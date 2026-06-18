# Runtime adapter stack

Crabbox can sit behind a fleet UI without teaching that UI how each provider
creates, inspects, or deletes a workspace. Three `crabbox adapter` processes
split the job into a provider lifecycle API, an authenticated browser ingress,
and an optional outbound coordinator connection.

Use this stack when:

- a browser-facing service needs to create and stop Crabbox workspaces;
- the provider host should remain on loopback or behind NAT;
- browser identity and TLS terminate at an existing trusted proxy; and
- provider credentials must stay on the adapter host.

It is not a general reverse proxy, identity provider, WAF, or remote shell.

## Components

| Component | Responsibility | Network exposure |
| --- | --- | --- |
| Fleet UI | User workflow and workspace metadata | Usually loopback behind ingress |
| `adapter ingress` | Authenticate proxy assertions, enforce origin and route policy, sanitize headers, proxy HTTP/WebSocket traffic | Listener reachable only by the trusted proxy |
| `adapter serve` | Authenticated, provider-neutral workspace lifecycle API | Loopback HTTP plus an optional private Unix socket |
| `adapter connect` | Relay a narrow typed lifecycle API to a coordinator | Outbound WebSocket plus the private Unix socket |
| Provider adapter | Create, inspect, connect to, and delete the actual workspace | Local child process or configured provider API |

The fleet UI and provider adapter are deployment choices. The three adapter
commands are reusable Crabbox building blocks.

## Topology

```text
browser
   |
   | HTTPS
   v
trusted identity-aware proxy
   |
   | exact identity + secret assertions
   v
adapter ingress
   |
   | sanitized HTTP / WebSocket on loopback
   v
fleet UI ---------------------> adapter serve ----------------> provider
                                      ^
                                      | private Unix socket
                                      |
coordinator <--- outbound WSS --- adapter connect
```

The browser path and coordinator path are independent. `adapter ingress`
protects the fleet UI. `adapter connect` lets a coordinator reach the lifecycle
API without opening an inbound route to `adapter serve`.

## Request paths

### Browser traffic

1. The trusted proxy authenticates the browser and terminates TLS.
2. The proxy sends exactly one configured identity header and one configured
   secret header to `adapter ingress`.
3. `adapter ingress` rejects denied routes before authentication, checks the
   assertions and request origin, removes unsafe forwarding and private
   headers, then installs the configured upstream assertions.
4. The request reaches the loopback fleet UI. Streaming responses and valid
   WebSocket upgrades remain streaming end to end.

The proxy is responsible for user authentication and TLS. `adapter ingress`
trusts only the configured assertion pair; it does not validate browser login
sessions itself.

### Workspace lifecycle

1. The fleet UI sends a bearer-authenticated request to `adapter serve`.
2. `adapter serve` validates the fixed deployment policy, persists lifecycle
   state, and invokes the configured provider.
3. The provider returns a workspace identity and connection metadata.
4. Later reads and deletes resolve the persisted workspace record rather than
   accepting arbitrary provider commands from the request.

The API exposes workspace operations, not shell commands, argv, environment,
files, or provider credentials. Keep the bearer token between the fleet UI and
`adapter serve` private.

### Outbound coordinator control

1. `adapter connect` authenticates to the coordinator using normal Crabbox
   configuration and obtains a short-lived relay ticket. A new adapter ID starts
   with a ten-minute provisional owner/org claim; agent connection or successful
   lease registration makes it durable. Only expired inactive provisional claims
   are recoverable, and existing adapter claims remain durable across upgrades.
2. It opens an outbound WebSocket and accepts only the documented typed
   workspace operations.
3. Each operation is forwarded through the verified, current-user-owned Unix
   socket to `adapter serve` with the local bearer token.
4. The local token, remote cookies, and proxy credentials never cross the
   relay.

The relay is optional. Omit it when only the local fleet UI needs lifecycle
access.

## Trust boundaries

| Boundary | Required proof | Keep private |
| --- | --- | --- |
| Browser to trusted proxy | Deployment-specific user authentication | Browser session |
| Trusted proxy to `adapter ingress` | Exact identity assertion, exact secret assertion, exact public origin | Proxy secret and assertion policy |
| Fleet UI to `adapter serve` | Bearer token from the private token file | Adapter bearer token |
| `adapter connect` to local adapter | Unix-socket owner, mode, peer UID, and bearer token | Socket and adapter bearer token |
| `adapter connect` to coordinator | Normal Crabbox login or token command, then short-lived relay ticket | Coordinator credential |
| `adapter serve` to provider | Deployment-owned provider configuration | Provider credentials and lifecycle state |

Do not reuse one secret across boundaries. In particular, the proxy secret,
adapter bearer token, and coordinator credential serve different principals
and rotation paths.

## Configuration and rotation

- Keep the `adapter serve` token, state, and Unix-socket parent in
  current-user-owned private paths.
- Keep the `adapter ingress` JSON config and secret as current-user-owned
  regular files with mode `0400` or `0600`.
- Use one literal listener IP, one exact loopback HTTP upstream, and one exact
  non-loopback HTTPS public origin for ingress.
- Bind the fleet UI and `adapter serve` to loopback. The trusted proxy should be
  the only peer that can reach the ingress listener.
- Restart `adapter ingress` after rotating its config or proxy secret; both are
  loaded when the process starts.
- `adapter connect` reloads normal Crabbox config and its local token file when
  reconnecting. It also reloads the local token before each forwarded request.

## Start and stop order

Recommended startup:

1. Start `adapter serve` and wait for its loopback health check.
2. Start the fleet UI and verify it can reach the lifecycle API.
3. Start `adapter ingress` behind the trusted proxy.
4. Optionally start `adapter connect` and verify coordinator registration.
5. Expose the trusted proxy route only after every required health check passes.

Shutdown in reverse. Remove or disable the public proxy route first, stop the
outbound connector, stop the fleet UI, then stop `adapter serve`. This prevents
new browser or coordinator work from arriving while lifecycle state is being
quiesced.

## Health and failure signals

| Signal | Meaning |
| --- | --- |
| `adapter serve /healthz` succeeds | Lifecycle process is serving; it does not prove every provider operation will succeed |
| Loopback `adapter ingress /healthz` succeeds | Ingress can reach the upstream health endpoint |
| `401` from ingress | Missing, duplicate, or incorrect proxy assertions |
| `403` from ingress | Request origin does not exactly match the configured public origin |
| `404` from ingress | Route is denied, or a non-loopback peer requested `/healthz` |
| `502` from ingress | The loopback upstream is unavailable or failed during proxying |
| Connector reconnect loop | Coordinator, local socket, local token, or coordinator authentication is unavailable |

Treat a healthy process as one layer of evidence, not proof of the full path.
For a deployment smoke test, verify an authenticated browser request, one
workspace lifecycle operation, and the outbound coordinator path when enabled.

## Related reference

- [`adapter` command](../commands/adapter.md): complete flags, config schema,
  API, relay protocol, and file-safety requirements.
- [Broker auth and routing](broker-auth-routing.md): coordinator and portal
  authentication models.
- [Security](../security.md): repository-wide trust and secret boundaries.
