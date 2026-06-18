# adapter

See [Runtime adapter stack](../features/runtime-adapter-stack.md) for the
end-to-end topology, trust boundaries, startup order, and failure signals for
`adapter serve`, `adapter ingress`, and `adapter connect`.

`crabbox adapter serve` exposes a small authenticated HTTP service that
creates and stops Crabbox workspaces. It is intended for a trusted fleet UI or
automation service that should use normal Crabbox provider configuration rather
than embed provider-specific lifecycle logic.

The adapter host must run Linux or macOS. `adapter serve` exits with a
clear unsupported-platform error on every other host OS. Windows workspaces
remain supported as guests through providers that support them.

```sh
install -d -m 700 "$HOME/.local/run/crabbox"
crabbox adapter serve \
  --listen 127.0.0.1:8787 \
  --unix-socket "$HOME/.local/run/crabbox/adapter.sock" \
  --token-file ~/.config/crabbox/adapter.token \
  --state-file ~/.local/state/crabbox/adapter/state.json \
  --config ~/.config/crabbox/adapter.yaml \
  --provider external \
  --id mac-lab \
  --profile public-desktop \
  --forbid-class-override \
  --forbid-server-type-override \
  --max-concurrent 2 \
  --allow-desktop \
  --allow-browser \
  --attach-url-template 'wss://terminal.example.test/workspaces/{workspaceId}'
```

The listen address defaults to `127.0.0.1:8787`. Put the service behind an
authenticated HTTPS proxy before exposing it beyond loopback. The token file is
required, must be a regular non-symlink file with mode `0600`, and contains one
bearer token. The adapter opens it once with no-follow semantics, validates
that handle, and bounds the read to 8 KiB. Tokens are never accepted on argv.
The state directory must be owned by the current user and not writable by group
or others. The exclusive lock is opened relative to that verified directory
with no-follow semantics, then its exact descriptor is checked for ownership,
type, and private mode before the kernel lock is acquired.

`--unix-socket` adds a second listener for `adapter connect`; it does not
replace `--listen`. Its existing parent directory must be owned by the current
user and not writable by group or others. The adapter refuses to replace a
foreign or non-socket path, removes only a stale current-user-owned socket, and
installs the live socket with mode `0600`. The outbound connector accepts only
this Unix transport and verifies the server peer UID before sending the local
bearer token.

Set `--id` to the same stable DNS-style ID used by
[`crabbox adapter connect`](#outbound-connection) when the adapter is connected
outbound to a coordinator. Child lifecycle commands then register both that
adapter ID and the exact workspace ID with the coordinator. The portal can send
Delete through the live relay. After the adapter proves stable provider absence,
it sends an owner-scoped completion for that exact pending adapter/workspace
registration generation before completing matching local-state cleanup. Each
local claim persists a fresh registration ID; the coordinator retains it across
refreshes and rejects stale completion retries after a later generation becomes
active. If the coordinator record expires while the provider workspace remains
live, the CLI rotates the persisted ID only after that explicit stale-generation
rejection and retries registration. The old acknowledged ID remains durable
beside the pending replacement until the replacement succeeds; confirmed-absence
cleanup can safely try both after a crash or lost response. A workspace that
never persisted a registration ID uses the legacy metadata-release cleanup path
only after the coordinator confirms that its exact adapter/workspace binding
also has no registration generation. Missing or malformed generation-aware
claim state fails closed.
Registration must return that exact adapter/workspace/registration
binding before the workspace can become ready; a failed or mismatched
registration instead enters exact-identity provider cleanup. Without `--id`,
registered leases retain the metadata-only removal behavior.

Validate a copied state file before an upgrade without locking, rewriting,
reconciling, or running provider commands:

```sh
crabbox adapter state validate --state-file /path/to/copied-state.json
```

The command uses the production state decoder and record invariants, rejects
unknown fields, incompatible versions, corrupt records, symlinks, and broad
file permissions, and exits nonzero on failure.

## Authenticated ingress

`crabbox adapter ingress` exposes a loopback HTTP service through an exact
identity-and-secret assertion supplied by a trusted upstream proxy. It is a
small provider-neutral building block for a fleet UI or adapter deployment that
needs HTTP streaming and WebSocket upgrades without maintaining a custom
reverse proxy.

The command accepts one private JSON config file:

```json
{
  "listen": "0.0.0.0:8443",
  "upstream": "http://127.0.0.1:8787",
  "publicOrigin": "https://fleet.example.test",
  "identityHeader": "X-Proxy-User",
  "identity": "operator@example.test",
  "secretHeader": "X-Proxy-Secret",
  "secretFile": "/home/operator/.config/crabbox/proxy.secret",
  "denyPaths": ["/login/provider", "/auth/provider/callback"],
  "denyPrefixes": ["/api/admin/"],
  "stripHeaderPrefixes": ["x-crabbox-", "x-fleet-private-"]
}
```

```sh
crabbox adapter ingress --config /home/operator/.config/crabbox/ingress.json
```

Both the config and secret must be current-user-owned regular non-symlink files
with mode `0400` or `0600`. The config is strict JSON and rejects unknown keys.
The command currently requires Linux or macOS.
The listen host must be a literal IP address, the upstream must be one exact
loopback HTTP origin, and the public origin must be one exact non-loopback HTTPS
origin. Keep the listener behind the trusted proxy and terminate TLS there.

For ordinary HTTP requests, `Origin` may be absent but must exactly match
`publicOrigin` when present. WebSocket upgrades always require the exact origin.
The ingress rejects duplicate or incorrect assertion headers, removes browser
credentials, forwarding headers, hop-by-hop headers, and configured private
header prefixes, then installs the configured assertion and sanitized public
host/protocol forwarding headers before proxying. Header names containing
underscores are rejected to prevent downstream dash/underscore aliasing.
Denied routes return `404` before authentication. `/healthz` is forwarded
without assertions only for loopback peers.

## Outbound connection

`crabbox adapter connect` exposes a local `crabfleet/v1` runtime adapter to the
configured coordinator over one outbound authenticated WebSocket. Use it when
the adapter host is behind NAT or otherwise cannot accept inbound requests from
Crabfleet.

```sh
crabbox adapter connect \
  --id mac-lab \
  --local-socket "$HOME/.local/run/crabbox/adapter.sock" \
  --token-file ~/.config/crabbox/adapter.token
```

The command reads the coordinator URL and login from normal Crabbox config. It
does not accept a second coordinator URL or token on argv. Each connection gets
a fresh short-lived ticket from `POST /v1/adapters/{id}/ticket`, then opens
`/v1/adapters/{id}/agent`. After a disconnect it reloads config, obtains a new
ticket, and reconnects with bounded exponential backoff, so refreshed normal
login credentials are picked up without a second secret store. The local token
file is also reloaded before each reconnect and each forwarded request, so an
atomically rotated token is never pinned in the relay process. Coordinator
authentication may be a static login token or the normal shell-free
token-command configuration.

The first ticket for a previously unused adapter ID creates a ten-minute
provisional owner/org claim. A successful agent connection or registered lease
binding makes that claim durable. Another normally authenticated owner may
recover an expired provisional claim only when it has no connected agent,
pending relay request, unexpired ticket, live registered lease, or pending
workspace deletion. Existing adapter claims created before this contract are
treated as durable, so upgrades do not change current adapter ownership.

`--local-socket` is required. It must be an absolute, clean Unix-socket path in
an existing directory owned by the current user and not writable by group or
others. The socket must also be owned by the current user with mode `0600`.
Every connection verifies the server peer UID before sending the local bearer
token. TCP and loopback HTTP endpoints are intentionally not accepted. The
connector runs on Linux and macOS and fails at the command boundary on Windows.

`--token-file` is required. It uses the same no-follow, descriptor-verified,
regular-file, mode-`0600`, 8 KiB-bounded loading as `adapter serve`. The local
token never crosses the WebSocket. Remote authorization, cookies, and proxy
credentials are discarded; the relay supplies only that local bearer token to
the verified Unix-socket service.

The relay accepts only typed JSON requests:

```json
{
  "type": "request",
  "id": "request-1",
  "method": "DELETE",
  "path": "/v1/workspaces/fleet-a-is-101",
  "deadlineMs": 4102444800000
}
```

Allowed operations are exactly:

- `POST /v1/workspaces`
- `GET /v1/workspaces/{id}`
- `DELETE /v1/workspaces/{id}`
- `POST /v1/workspaces/{id}/connections/desktop`

There is no arbitrary URL, shell, argv, environment, file, or provider-command
surface. Request and response bodies are UTF-8 strings bounded to 64 KiB.
Only workspace creation accepts a non-empty request body. Ordinary local
requests time out after nine seconds, and the coordinator allows five more
seconds for response delivery. Every frame carries that absolute Unix
millisecond deadline; the connector rejects expired frames before local
dispatch and caps the local request context to the earlier of that deadline or
its own timeout. Desktop connection setup
gets the configured `--connection-timeout` plus 30 seconds of relay overhead,
and the connector negotiates that deadline plus five seconds of response grace
with the coordinator. The setup value may not exceed 24 hours. Set it to the
same value as `adapter serve --connection-timeout` when overriding the default.
Responses contain only the request ID, HTTP status,
optional content type, and exact bounded body:

```json
{
  "type": "response",
  "id": "request-1",
  "status": 202,
  "headers": {"content-type": "application/json; charset=utf-8"},
  "body": "{\"id\":\"fleet-a-is-101\",\"status\":\"stopping\"}\n"
}
```

The connector dispatches up to 64 ordinary requests concurrently and keeps a
separate bounded lane for deletes, so a slow desktop setup cannot block its
cancellation. The coordinator also applies per-adapter, per-owner, and global
in-flight limits; work already sent upstream remains counted after its caller
disconnects until the response or deadline. A durable generation-scoped
dispatch fence prevents confirmed absence from freeing and reusing a binding
while an earlier delete can still reach the adapter. WebSocket responses are
serialized, and disconnecting the relay cancels every in-flight local request.

Flags:

```text
--id <name>                    required coordinator adapter ID
--local-socket <path>          required current-user-owned Unix socket
--token-file <path>            required local adapter bearer-token file
--connection-timeout <duration> local desktop setup budget; default 2m
```

## HTTP API

`GET /healthz` is unauthenticated and returns `{"status":"ok"}`. Every `/v1`
route requires:

```text
Authorization: Bearer <token-file contents>
```

Create a workspace:

```http
POST /v1/workspaces
Content-Type: application/json

{
  "id": "demo-box",
  "repo": "example/app",
  "branch": "main",
  "runtime": "linux",
  "profile": "public-desktop",
  "ttlSeconds": 14400,
  "idleTimeoutSeconds": 1800,
  "capabilities": {
    "desktop": true,
    "browser": true,
    "code": false
  }
}
```

`id` is required and must be a lowercase DNS-style name of at most 63
characters. It is the stable adapter path identity; a provider resource ID
returned later is provenance only. POST returns `202` while provisioning. The
collision-safe provider slug is independently truncated to the CLI's 41-byte
requested-slug limit, so maximum-length workspace IDs remain valid. The
same normalized request and ID are idempotent and never start a second warmup;
a different immutable request using the same ID returns `409`
`workspace_id_conflict`. Crabfleet treats that exact error code as a terminal
local conflict without adopting or deleting the pre-existing workspace; other
`409` responses remain ambiguous and retryable.
Immediately after provider acquisition, the warmup child reports the raw lease,
slug, provider, and provider `cloudId` over a private loopback acknowledgment
gate. The adapter durably stores that complete identity before allowing the
child to continue with claims, network setup, or readiness output. Subsequent
inspection must match every field exactly; inspection can never first-adopt a
provider resource identity.
If the atomic rename succeeds but the state-directory flush does not, POST
retains that exact attempt ID and slug but returns retryable
`503 state_durability_pending` with `Retry-After: 1`; retry the identical POST.
It returns `202` only after the installed snapshot is durably synchronized.
Deployments may set `--required-ttl` and `--required-idle-timeout` to require
exact `ttlSeconds` and `idleTimeoutSeconds` values on every POST. Omission or a
different value is rejected before state persistence or provider execution.
`--forbid-class-override` and `--forbid-server-type-override` similarly reject
nonempty request `class` and `serverType` values after authentication and before
state persistence or provider execution. This lets deployment-owned config and
profiles remain the only machine-shape policy.

Optional metadata fields are `command`, `prompt`, `purpose`, `summary`, `owner`,
`createdBy`, `parentSessionId`, and `rootSessionId`. They are stored in the
private adapter state for idempotency and recovery but are never executed.
Only non-secret identity fields are forwarded to an external provider through
`CRABBOX_ADAPTER_*` child environment variables. Request bodies are capped
at 64 KiB.

Inspect or stop the workspace:

```http
GET    /v1/workspaces/demo-box
DELETE /v1/workspaces/demo-box
```

DELETE is asynchronous and idempotent. It invokes normal `crabbox stop`, not a
metadata-only coordinator release. Like POST, a stopping transition whose
directory flush is pending is retained but returns
`503 state_durability_pending`; retrying the same DELETE never rewrites the
cleanup identity and returns `202` only after durability is confirmed.
Workspace statuses are `provisioning`,
`ready`, `stopping`, `failed`, `expired`, and `stopped`. A GET of an active
workspace schedules a bounded provider reconciliation; a later poll reflects
provider expiry or another terminal provider state.
Before destructive release, the adapter passes the complete persisted
lease, attempt, slug, and provider-resource identity set to `stop`; every
nonempty value must match the release-only resolution. A mismatched adapter
response is rejected without invoking provider release. Provider release also
waits for a durable stopping transition. If that transition cannot be written,
the adapter immediately revokes its local desktop bridge and retains an
in-memory revocation retry while leaving provider cleanup behind the durability
barrier. That terminal intent remains distinct from ordinary bridge cleanup:
successful local revocation cannot clear it, reopen a desktop, or resume ready
inspection until the `stopping` transition itself is durable.

A workspace response is a single JSON object. `attachUrl` is present only when
the deployment configures an actual WSS terminal transport; an HTTPS page is
not a terminal connection. Capabilities use the `crabfleet/v1` adapter shape
and explicitly report unavailable features as `false`:

```json
{
  "id": "demo-box",
  "status": "ready",
  "leaseId": "cbx_abcdef123456",
  "provider": "external",
  "providerResourceId": "provider-resource-123",
  "host": "192.0.2.10",
  "attachUrl": "wss://terminal.example.test/workspaces/demo-box",
  "message": "workspace ready",
  "capabilities": {"terminal": true, "takeover": false, "vnc": true, "desktop": true, "logs": false, "artifacts": false},
  "expiresAt": "2026-06-13T00:00:00Z",
  "createdAt": "2026-06-12T00:00:00Z",
  "updatedAt": "2026-06-12T00:01:00Z"
}
```

Errors use `{"error":{"code":"...","message":"..."}}` with an appropriate
HTTP status.

## Desktop connections

For a ready workspace created with `capabilities.desktop=true`:

```http
POST /v1/workspaces/demo-box/connections/desktop
```

The adapter verifies that the local WebVNC bridge daemon is running on the
selected port, the VNC target is reachable, and the portal bridge is connected.
New adapter-owned daemons must report a supervisor PID before setup can
continue. A missing PID or any later verification failure triggers bounded
process-tree revocation rather than leaving a credential bridge behind.
Reusing an existing daemon additionally requires its durable identity and live
command to prove adapter ownership, provider-side-effect-free operation, and
the exact adapter state path, provider scope, and persisted resource
identities. Any mismatch is revoked and recreated before handoff.
The adapter keeps raw ownership material in-process and passes only a
domain-separated public owner ID to WebVNC subprocesses. Daemon status returns
only a boolean ownership match and redacts that ID from command diagnostics, so
neither the raw owner token nor its public derivative appears in status or log
output.
Every adapter-owned WebVNC start and status resolution receives and checks
the complete persisted lease, attempt, slug, resource, and provider-scope
identity before a credential bridge can become ready.
For direct-SSH WebVNC it verifies that the exact loopback listener is owned by
the same recorded WebVNC supervisor process tree immediately before the VNC
authentication probe, immediately after it, and after the final status check.
A different process prebinding or replacing the selected port is rejected
without receiving or yielding a VNC credential. It then obtains the current portal URL from
`crabbox webvnc status` and returns:

```json
{"url":"https://broker.example.test/portal/leases/cbx_.../vnc#password=..."}
```

The credential-bearing URL is returned only by this endpoint and is never
persisted or included in normal workspace responses. Returned URLs must use HTTPS;
plain HTTP is accepted only for literal loopback hosts. `--vnc-url-template` can
replace the returned URL only after live bridge verification succeeds. URL
templates support `{workspaceId}`, `{leaseId}`, and `{slug}`, follow the same
transport restriction, and may not contain user information, query strings, or
fragments. The adapter checks status, lease identity, and effective expiry
again after setup and revokes the local bridge if the lifecycle changed.

## Lifecycle and recovery

The adapter holds an exclusive process-lifetime lock beside the state file.
A second adapter using the same state path exits before loading, rewriting,
or reconciling shared state.
The adapter persists atomic mode-`0600` JSON after every transition, flushes
the file before rename, and flushes the complete existing parent chain on every
write attempt plus the state directory after rename. Retrying after a parent
flush failure therefore repeats the missing durability work even though the
directories now exist. If the final directory flush fails after installation,
the adapter keeps the installed state in memory and logs the indeterminate
durability instead of rolling back to stale state. It raises a durability
barrier and retries that flush before any provider lifecycle command may run.
State installation and lifecycle-command admission share one gate: a new
barrier cancels admitted acquisition, inspection, provider-stop, and desktop
setup commands, while local WebVNC revocation remains allowed so a bridge is
not kept alive by a failed durability retry. Ready-state bridge cleanup and its
retries run before the durability barrier; only subsequent provider inspection
waits for directory sync recovery. Startup rewrites an existing
loaded snapshot through the same atomic persistence path before reconciliation,
so a process restart cannot forget an indeterminate directory flush.
On restart, an acquisition without a durably acknowledged raw identity retries
the same idempotent fixed lease ID and unique `cbx-ctl-*` slug instead of
adopting identity from inspection. A started acquisition that fails before that
acknowledgment remains cleanup-pending. Stable provider absence must span the
`--create-timeout` window; a present or late attempt is retried with the same
fixed ID and slug to recover its raw identity, then cleaned up by exact
identity. The adapter inspects only provisioning and ready workspaces whose
full acquire identity is already durable, and resumes stops. Ready workspace
inspection runs at least once per `--ready-reconcile-interval` and sooner when
the recorded expiry is near.
If ready inspection returns a different lease, attempt, slug, or provider
resource identity, the adapter immediately leaves `ready`, clears host and
attach metadata, revokes the bridge, and cleans up only the persisted expected
identity; it never adopts or releases the mismatched response.
Requested `ttlSeconds` is also stored as an adapter-owned deadline, so
cleanup does not depend on the provider returning an expiry field.
An acquisition prepared before a crash is reconciled for one create-timeout
window before the adapter retries the same stable lease identity and unique
slug.
Fresh creates never adopt an existing same-named lease. A durably prepared
create is treated as potentially launched even if the post-spawn callback did
not persist, covering the process-start/callback race during cleanup. DELETE
and expiry transitions cancel an in-flight provider acquisition before cleanup
begins.
Lifecycle operations are bounded by `--max-concurrent`; each workspace has at
most one scheduled reconcile retry. Cleanup revokes local WebVNC independently
before provider shutdown, refreshes provider inventory after stop, and records
provider cleanup only after absence remains stable across reconciliations. An
absence proof compares every persisted lease ID, fixed attempt ID, slug-derived
name, and provider resource ID against every inventory row. Rows with empty or
incomplete applicable identity fields fail closed instead of proving absence.
Provider inventory output is bounded at 1 MiB; overflow is an explicit error,
never a truncated absence proof. Before issuing a destructive release the
adapter durably records that request, then retries only exact inventory
confirmation, so overflow or refresh failure cannot repeatedly issue release.
Confirmed-absence cleanup does not mark the provider stopped until matching
claim, routing, and slug-reservation deletions and their containing-directory
flushes succeed. If a flush fails after deletion, the next reconciliation
repeats that directory durability barrier before terminal acknowledgment.
A later positive inventory match may authorize one new release for a resource
that materialized after an earlier confirmed absence. A
prepared or started acquisition that was never observed remains in `stopping`
through its bounded create-timeout recovery window so a late provider resource
can still be found and stopped.
Either failed cleanup phase remains pending until it succeeds. Adapter
shutdown cancels active acquisition, inspection, bridge, and cleanup subprocess
trees and waits for active reconciliations to finish reaping them.

Lifecycle subprocesses wait behind a pipe handshake until their exact PID and
process-start identity are durably recorded. A watchdog kills their process
group if the adapter dies; Linux also uses a kernel parent-death signal.
Startup terminates any exactly matching recorded child before provider
reconciliation and never signals a recycled PID.

The adapter invokes the configured Crabbox binary with only fixed arguments
and terminates its complete subprocess tree on timeout, cancellation, or a
failed start-state persistence callback:
`warmup`, `inspect --json`, `list --json --refresh --all`, `stop`, and WebVNC
bridge/status commands. A successful structured provider list confirms absence;
numeric command exit codes alone never classify a workspace as missing.
Unrelated legacy or partial inventory rows are ignored, but any row matching a
persisted lease, slug/name, or resource identity must contain the complete
target identity set or absence confirmation fails closed.
Crash-safe creation also requires fixed idempotent lease IDs. The public
`external` provider exposes that contract only when the adapter configuration
explicitly sets `external.capabilities.idempotentLeaseId: true`; startup fails
closed otherwise. Declarative external adapters additionally require raw
`json-lease` output from both acquire and resolve, complete `json-lease-array`
inventory, plus a standalone
`{{cloudId}}` argument in every release command, so an adapter never provisions a resource it
cannot later re-attest and release exactly.
Provider identity discovery itself uses the bounded inspection context and the
same durable child registry, process-group watchdog, termination, and reap path;
a hung configured binary cannot block startup or shutdown indefinitely.
`--config`, `--provider`, and `--profile` are deployment policy. When
`--profile` is set, requests may omit it or provide that exact value; all other
profiles are rejected. Once accepted, the profile stored in each request is the
only profile used for retries, so changing a later adapter invocation cannot
reroute an existing workspace. The resolved provider route and opaque
configuration-scope identity are persisted before the first lifecycle side
effect and reused by every restart operation. Inspect, refreshed list, stop,
and WebVNC subprocesses reject a current provider configuration whose scope no
longer matches that record. For the `external` provider, inspect, refreshed
inventory, stop, and confirmed-absence cleanup also load the deterministic
per-lease routing file directly, even if the ordinary lease claim is missing.

Flags:

```text
--listen <host:port>             default 127.0.0.1:8787
--unix-socket <path>             optional private Unix socket for adapter connect
--token-file <path>              required bearer-token file
--state-file <path>              durable private JSON state
--config <path>                  fixed Crabbox child config
--provider <name>                fixed child provider
--id <name>                      coordinator adapter ID for registered workspaces
--profile <name>                 only accepted workspace profile
--max-concurrent <n>             default 2; range 1..64
--allow-desktop                  accept desktop capability
--allow-browser                  accept browser capability
--allow-code                     accept code capability
--attach-url-template <url>      optional WSS terminal connection URL
--vnc-url-template <url>         published URL after bridge verification
--create-timeout <duration>      default 60m
--inspect-timeout <duration>     default 2m
--stop-timeout <duration>        default 10m
--connection-timeout <duration>  default 2m
--ready-reconcile-interval <duration> default 1m
--required-ttl <duration>       require exact request ttlSeconds; disabled by default
--required-idle-timeout <duration> require exact request idleTimeoutSeconds; disabled by default
--forbid-class-override          reject nonempty request class values
--forbid-server-type-override    reject nonempty request serverType values
--crabbox-binary <path>          lifecycle executable
--work-dir <path>                lifecycle working directory
```

Every flag has a `CRABBOX_ADAPTER_*` environment equivalent. Flags override
environment values. Provider credentials remain in the provider's normal
credential store or child environment; never put them in HTTP requests.

`adapter state validate` flags:

```text
--state-file <path>              required state copy to validate read-only
```

## Public Linux desktop profile

[`scripts/install-linux-desktop.sh`](../../scripts/install-linux-desktop.sh) is
the reusable open-source Debian/Ubuntu guest bootstrap. It installs
XFCE/Xvfb/x11vnc plus noVNC and websockify from distribution packages, creates
the Crabbox desktop state files, binds VNC only to `127.0.0.1:5900`, and starts
hardened systemd services. The generated VNC password is owned by the desktop
user with mode `0600`; it is never readable through a shared primary group. It
also installs `/usr/local/bin/crabbox-start-desktop`, the reset helper used by
WebVNC to restart the matching XFCE and x11vnc units. The bootstrap installs
`sudo` and a mode-`0440` sudoers rule granting only the desktop user passwordless
execution of that root-owned helper; it does not grant general sudo access. It
installs no proprietary browser. External provider lifecycle configuration can
run this script while preparing a desktop-capable box.
