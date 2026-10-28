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

Use the existing coordinator login and organization authorization. Enrollment
and eligibility changes require coordinator admin authentication. Host state is
bound to the enrolling operator identity and coordinator URL. Keep credentials
in the existing protected login store or trusted process environment, never in
repository YAML. On Linux the default private state directory is
`/var/lib/crabbox-host`; do not copy it between machines or delete it to recover
capacity. One process lock fences agents across state directories on a machine.

```sh
crabbox login --url https://broker.example.com
sudo --preserve-env=CRABBOX_COORDINATOR,CRABBOX_COORDINATOR_TOKEN,CRABBOX_OWNER \
  crabbox host enroll --name linux-builder --template tools-v1 --capabilities gpu,build
crabbox host list
# Substitute the enrolled UUID; inventory starts disabled.
crabbox host enable --id <host-uuid>
sudo --preserve-env=CRABBOX_COORDINATOR,CRABBOX_COORDINATOR_TOKEN,CRABBOX_OWNER \
  crabbox host serve
```

Run the agent under an operator-managed service with the same protected state
and identity. Credentials must be available in that service's context; a
non-root user's login store is not automatically the root service's store.
Only outbound coordinator HTTPS/WebSocket access is required. Enrollment alone
is not a connectivity check: `host list` must report the agent online.
`--template` is an operator-attested toolchain/environment revision, not a
snapshot or containment attestation. Drain and remove/re-enroll a clean host to
change its immutable enrollment metadata. Capabilities are exact-match labels,
not automatically measured hardware claims.

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
crabbox host enroll --name windows-builder --template windows-tools-v1 --capabilities build
crabbox host serve --mxc-executable C:\Tools\mxc\wxc-exec.exe
```

The agent validates the executable hash and `isolationSessionAvailable` before
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
