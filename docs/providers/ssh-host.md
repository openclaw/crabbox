# Managed SSH hosts

`ssh-host` leases existing operator-owned machines through the coordinator.
It is separate from direct [`ssh`](ssh.md): direct SSH does not enroll inventory,
allocate team capacity, or change its existing key semantics.

A host runs `crabbox host serve`. Its outbound HTTPS heartbeat receives an
exclusive assignment; an outbound WebSocket carries the existing SSH relay.
Borrowers need coordinator login, not the operator's SSH key, a public inbound
SSH port, or membership in an operator's private network. The lifecycle adapter
relay is **not** this transport. Both the host front door and lease endpoint
have pinned Ed25519 host keys; each borrower supplies its own public key.

## Operator enrollment

Use a dedicated test machine. Linux requires a root-owned agent, `bubblewrap`,
unprivileged user namespaces, `useradd`, `userdel`, `runuser`, `getent`, `pkill`,
`pgrep`, `groupdel`, Git, and rsync. Verify namespace support before enabling the host.
Linux workloads run as a lease-specific unprivileged account, with a read-only
host filesystem, writable private home, private PID/IPC namespaces, `/tmp`, and
`/var/tmp`. This is **not a VM or network isolation**: host-readable files and
network access remain available according to OS permissions. Do not grant the
lease account sudo, Docker access, or host secrets. macOS enrollment is not
supported by this runtime; direct macOS SSH remains supported.

Prepare the machine without an operator login, then enroll its public descriptor
from an administrator's workstation. The host receives a revocable credential
that can only heartbeat for that host and attach its assigned SSH relays.
It cannot list inventory, borrow leases, or perform admin operations.

On the Linux host:

```sh
sudo crabbox host prepare --coordinator https://broker.example.com \
  --name linux-builder --template tools-v1 --capabilities gpu,build > host.json
```

`prepare` persists a machine UUID and Ed25519 front-door key in protected local
state before writing the public descriptor to stdout. The descriptor contains
no credential or private key. Transfer only `host.json` to the admin workstation.
On that workstation, choose an existing private directory outside repositories:

```sh
crabbox login --url https://broker.example.com
crabbox host enroll --descriptor host.json \
  --credential-out /secure/operator/host.credential.json
```

Enrollment generates a random 256-bit secret locally and writes the protected
credential file **before** sending its SHA-256 hash to the coordinator. A lost
response is safe to retry with the same descriptor and output path. A changed
hash, key, or enrollment metadata conflicts; an existing output file is never
silently replaced with a new secret. Inventory starts disabled. Owner and
organization come from the administrator's authenticated identity.
Host enrollment, credential revocation/reissue/recovery, and inventory changes
prefer an explicitly configured coordinator admin token; without one, they use
the normal login, which must be an administrator. `host list` keeps the normal
login. In this scoped flow, the admin credential stays on the workstation;
`host serve` uses only its installed host credential.

Transfer the credential file through a private channel to the prepared host,
then install it through stdin:

```sh
sudo crabbox host install --credential-stdin < /secure/operator/host.credential.json
sudo crabbox host serve
```

Installation checks the UUID, public key, coordinator URL, credential generation,
and the coordinator's authenticated enrollment receipt. It refuses replacement
while local runtime or assignment custody remains. The credential file is
sensitive; remove transferred copies when installation is verified. Keep the
admin workstation's copy only while enrollment retries need it.

From the admin workstation, verify the host is online before enabling it:

```sh
crabbox host list
crabbox host enable --id <host-uuid>
```

Run the agent under an operator-managed service using the same protected state.
Scoped hosts use only the installed host credential and bound coordinator URL;
ambient user/admin login and token commands are not fallbacks. The default
credential lifetime is 90 days; `--credential-ttl` accepts up to `8760h` (365 days).
Expiry stops placement and relay access, and bounds the agent's local access
deadline. The UUID and key binding identifies the prepared state; copying that
entire private state can copy its authority, so it is not hardware attestation.

On Linux the default private state directory is `/var/lib/crabbox-host`. Do not
copy it between machines or delete it to recover capacity. One process lock
fences agents across state directories. Only outbound coordinator HTTPS/WebSocket
access is required. Enrollment alone is not a connectivity check.

### Existing operator-authenticated hosts

Existing `host enroll --name ... --template ...` and `host serve` deployments
remain supported with their existing protected operator login. They do not gain
scoped credentials automatically. A scoped host never accepts operator auth for
heartbeat or relay attachment, including after expiry or revocation. Prepare a
new clean enrollment to adopt the scoped flow; do not delete state that retains
custody.

`--template` is an operator-attested toolchain/environment revision, not a
snapshot or containment attestation. Drain and remove/re-enroll a clean host to
change its immutable enrollment metadata. Capabilities are exact-match labels,
not automatically measured hardware claims.

## Revocation and renewal

`host revoke --id <host-uuid>` requires admin authorization. It immediately
invalidates the host credential, disables placement, marks the host offline,
and closes coordinator relays. A relay backend failure returns an incomplete
result (503): the credential is already invalid, and exact relay IDs remain
recorded so the same admin operation can retry closure. It preserves allocation, quarantine, and
cleanup custody. Revocation does not claim that workload cleanup succeeded.

For planned renewal, drain the host, end its leases, and wait for cleanup receipts
until inventory is available. Then disable it and stop the agent. Reissue requires
a disabled, drained host with no allocation or quarantine and the current
credential generation from `host list`:

```sh
crabbox host drain --id <host-uuid>
# Stop outstanding leases and wait for confirmed cleanup before continuing.
crabbox host disable --id <host-uuid>
crabbox host reissue --descriptor host.json --expected-generation 1 \
  --credential-out /secure/operator/host-next.credential.json
```

Install the new file on the stopped host with `host install --credential-stdin`,
restart `host serve`, verify online, then `host resume --id ...` and
`host enable --id ...`. Reissue invalidates the previous generation immediately;
retry a lost response with the same output file and expected generation.

If revocation or expiry happened with outstanding custody, retain the private
host state. On the admin workstation, `host revoke` returns the retained custody
triple: lease ID, allocation generation, and access generation. Drain and disable
the host, then stop that exact lease. A pending-cleanup result from `stop` means
the coordinator has requested cleanup but still retains custody.

```sh
crabbox host revoke --id <host-uuid>
crabbox host drain --id <host-uuid>
crabbox stop <lease-id>
crabbox host recover --descriptor host.json --expected-generation 1 \
  --lease-id <lease-id> --allocation-generation 1 --access-generation 0 \
  --credential-out /secure/operator/host-cleanup.credential.json
```

Use the exact generations from the revocation response. `recover` requires a
disabled, drained host and an already-stopped lease whose cleanup is pending.
Its credential lasts one hour by default (at most 24 hours), can only reconcile
that exact cleanup assignment, and cannot prepare workloads, borrow, or attach
relays. Issuing it does not settle custody or clear quarantine.

Stop the host agent, install this credential through stdin, and restart the agent
with its existing state. This is the only credential-install path that permits
retained local custody: the lease ID and both generations must match, and local
runtime, assignment, and report records remain unchanged. Native cleanup retries
produce the normal exact-generation receipt. Successful cleanup alone settles
custody; the cleanup credential can only replay that exact completed acknowledgement
until expiry or reissue, so a lost response or failed local save can be retried.
If that acknowledgement credential expires, repeat `recover` with the same lease
and allocation/access generations and the current credential generation. The
coordinator must still prove that exact completed cleanup, with no new allocation.
Failed or stale receipts keep custody
reserved. Then stop the agent and use normal `reissue` with the cleanup credential's
generation to install a normal host credential. Missing or mismatched local state
requires operator investigation; never delete state to manufacture a clean host.

## Borrowing

```sh
crabbox login --url https://broker.example.com
crabbox host list
crabbox run --provider ssh-host --host-capabilities build -- echo hello
crabbox warmup --provider ssh-host --ssh-host linux-builder --ttl 1h
crabbox inspect --id <lease-id> --json
crabbox run --id <lease-id> -- make test
crabbox stop <lease-id>
```

Normal run sync, commands, inspect, heartbeats, and stop use the existing managed
lease flow. Matching uses organization, target OS, architecture, optional host
name/UUID and required capabilities. Host allocation and the lease record commit
atomically. Same-ID/same-intent retries recover the same lease; changed intent or
identity conflicts. Inventory advertises only metadata, never operator credentials.

## Portable ready pools

Enable `CRABBOX_PORTABLE_POOLS_ENABLED=true` on an isolated coordinator first.
Managed hosts require the existing portable protocol (`--access`); legacy pools
and ordinary typed pools are rejected because they do not rotate borrower keys.

```sh
crabbox warmup --provider ssh-host --ttl 2h --idle-timeout 30m
crabbox pool identity builders --id <lease-id> --cache-compatibility tools-v1 > pool.json
crabbox pool register builders --id <lease-id> --identity-file pool.json \
  --compatibility-key build --access
crabbox run --provider ssh-host --pool builders --pool-access \
  --pool-identity-file pool.json --pool-compatibility-key build -- make test
```

Identity binds the organization, enrolled template revision, architecture, and
existing repository seed digest. Registration cleans the initial lease account.
Each borrow creates a fresh account/session and access generation. A ready return
requires process, account/session, and workspace teardown before another grant;
**borrower-written caches and hydration do not survive return**. Install shared
tooling in the operator-managed host template. No physical host reboot, deletion,
shutdown, or reset occurs. Portable hard deadlines and receipt handling are
unchanged; see [pool](../commands/pool.md).

## Failure and recovery

| Event | Behavior |
| --- | --- |
| Borrower disconnect | Lease/portable liveness and hard deadlines remain authoritative; no immediate reallocation. |
| Coordinator restart | Durable reservation survives; relay connections reconnect using newly authorized sessions. |
| Agent restart | Revoke access and settle the retained account/session; fail the interrupted lease before reuse. |
| Offline host | No new placement after heartbeat freshness expires; outstanding custody remains reserved. |
| Lease/access expiry | Agent locally fences access and cleans owned work even when the coordinator is unavailable. |
| Missing or stale report | Cannot release custody; current assignment is returned for reconciliation. |
| Failed cleanup | Quarantine and retain allocation; retry only owned cleanup. Never infer success from silence. |
| Drain/disable | Prevent new placement/grants; existing borrower keeps its bounded assignment. |
| Remove | Allowed only without outstanding custody/quarantine; deletes inventory, not the machine. |

```sh
crabbox host drain --id <host-uuid>
crabbox host resume --id <host-uuid>
crabbox host disable --id <host-uuid>
crabbox host remove --id <host-uuid>
```

Repair the concrete OS cleanup failure while preserving private agent state,
then restart the agent. Ownership mismatches or a lost MXC provision receipt
require operator investigation; do not bypass quarantine or delete state to
make a machine appear empty.

## Windows / MXC qualification

The Windows adapter requires the pinned MXC 0.8.0 executor and an agent running
in the logged-in **interactive desktop context**:

```powershell
cmd /c "crabbox host prepare --coordinator https://broker.example.com --name windows-builder --template windows-tools-v1 --capabilities build > host.json"
# Enroll host.json on the admin workstation, then transfer the private credential.
cmd /c "crabbox host install --credential-stdin < C:\Secure\host.credential.json"
crabbox host serve --mxc-executable C:\Tools\mxc\wxc-exec.exe
```

The `cmd /c` redirections preserve the JSON bytes on both Windows PowerShell and
PowerShell 7. The agent validates the executable hash and `isolationSessionAvailable` before
polling. Ordinary SSH network logon is not an MXC activation bridge; failed
activation never falls back to host execution. Commands and sync terminate at
an SSH endpoint started **inside** the MXC isolation session. Deprovision receipts settle the exact session/account; typed `stale_id`
receipts allow retries after a lost completion. Missing provision identity retains quarantine.
Windows supports command execution and non-PTY shells; interactive Windows PTY
allocation is not implemented. Linux `connect` supports a lease-account PTY,
terminal resizing, and ordinary shell exit status.

The recorded Windows ARM64 proof covers the complete relay/endpoint execution,
authorization, revocation, and cleanup path. Qualify the intended Windows build
and hardware before rollout; it does not establish GPU access or native amd64
execution. Windows network access is explicitly allowed;
no network-containment claim is made. Do not deploy based on cross-compilation
alone. See [validation](../features/managed-host-validation.md) for exact proof.
