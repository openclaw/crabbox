# SSH Keys

Read when:

- changing local key storage or key generation;
- debugging SSH authentication or host-key trust;
- changing how provider key pairs are imported or cleaned up.

Crabbox generates a fresh SSH client authentication key per lease by default.
This keeps a long-lived personal key out of every runner and gives the provider
layer a predictable, per-lease resource name it can import and later delete.

For provisioned servers, brokered Hetzner cleanup records confirmation before
deleting an owned per-lease key. If key cleanup fails, the lease retains both the server evidence
and cleanup debt, so a retry can finish key cleanup without repeating server
deletion. A definitive failed create that left only a newly created owned key
can retry its exact retained key ID under the cleanup claim without inventing
a server-absence receipt. Shared keys remain retained. See
[Hetzner cleanup confirmation](lifecycle-cleanup.md#brokered-hetzner-cleanup-confirmation).

## Per-lease key generation

When a lease is created, the CLI runs `ssh-keygen` to produce a key it stores
locally. The key type is `ed25519` for most leases, and `rsa` (4096-bit) only
for AWS and Azure Windows targets, where the platform requires RSA. Generation
is idempotent: if a key already exists for the lease ID, it is reused as-is.
Fixed-ID AWS acquisition holds the normal durable claim lock while creating or
reusing this key, so concurrent replays cannot race two different keypairs into
one EC2 idempotency identity.

Without an explicit `XDG_STATE_HOME`, local key storage keeps its existing OS
user-config location:

```text
macOS:   ~/Library/Application Support/crabbox/testboxes/<lease>/id_ed25519
Linux:   ~/.config/crabbox/testboxes/<lease>/id_ed25519
```

Set `XDG_STATE_HOME` to a writable absolute directory to place generated keys at
`$XDG_STATE_HOME/crabbox/testboxes/<lease>/id_ed25519`. Claims and local history
use their separate namespaces under the same explicit state root. This is local
operator input only: repository YAML and `CRABBOX_CONFIG` cannot redirect keys.
`CRABBOX_CONFIG` selects the YAML file, not generated credential storage.

The matching `<lease>/id_ed25519.pub` and lease host-trust files sit beside the
private key. Directories remain private (`0700`) and private keys `0600` on
POSIX; Windows uses private ACLs. An existing explicit root must be owned by the
current user and not writable by unrelated users. A POSIX `0755` base is allowed;
Crabbox does not chmod that caller-owned directory. Windows permits harmless
read/traverse access and well-known SYSTEM/Builtin Administrators authority at
the base, but generated descendants use protected current-user-only ACLs.
Unsupported Windows grants are rejected conservatively rather than interpreted
as safe. The root must be absolute, without unsafe path components. Existing
generated keys are admitted under the same selected-root policy before reuse,
not silently repaired.

Keep the same root for acquire, inspect, SSH, reuse, heartbeat, stop and cleanup.
Switching roots never migrates keys or searches the old root; return to the
original root to manage its leases. This also covers canonical per-lease key
copies managed by Crabbox after authenticated provider import. User-supplied
keys, external provider auth files and host-level trust retain their existing
paths and ownership. Caches keep their separate locations,
and short SSH control sockets retain their temporary namespace.

For sandboxed automation, combine a writable explicit state root with
`CRABBOX_CONFIG` pointing at an approved configuration file. The default
user-config `crabbox` directory need not be writable for generated lease keys;
allow the runtime's separately documented cache and temporary paths as needed.

Prefer a state root outside source checkouts and shared host directories.
Crabbox's filtered repository sync excludes the exact
`$XDG_STATE_HOME/crabbox` namespace, even when tracked files or repository rules
would otherwise include it; sibling source files remain eligible. A checkout
inside that managed namespace cannot be synced. See [Sync](sync.md) for native
sync and mount limitations. This is not a general secret scanner and does not
remove files already committed upstream or shared with an existing runner.

## Provisioned host identity

For supported coordinator-backed Linux leases, the coordinator also generates
a separate Ed25519 server host-key pair and injects it before the machine's
first boot. It stores only the public half on the lease record; the private half
is sent only in the provider bootstrap payload. `crabbox inspect --json`
exposes the public identity as `sshHostKey` in exact `algorithm base64` form for
automation that pins the server identity before connecting.

This pre-boot path is available for Hetzner, GCP, and non-private AWS Linux
leases, and for Azure Linux leases not created from a snapshot. The field is
omitted for private AWS workspaces, Windows, macOS, Daytona, Azure snapshot,
registered, and direct-provider leases, where Crabbox cannot authoritatively
inject a host key before boot.

### Trust model

To pin the exact SSH host identity before the first connection, including
coordinator terminal and native-VNC connections, the coordinator generates the
host key pair and delivers it to the instance through provider launch data: AWS
and Hetzner user-data, Azure `customData`, or GCP metadata. This avoids a
trust-on-first-use gap for those connections.

The launch data contains the host private key. Principals with provider-side
read access to that data, or root access on the instance, can read the private
key and impersonate that host. On providers that expose launch data through an
in-guest metadata service, any guest process able to query that service can also
read the key without root access. Treat provider launch-data readers, guest
workloads with metadata access, and root on the instance as trusted
infrastructure. Tighten provider-side IAM permissions, such as
`ec2:DescribeInstanceAttribute` and `compute.instances.get`, restrict in-guest
metadata access where the provider supports it, and do not log launch-data
request bodies. This Low/P3 residual risk is accepted to preserve pre-connection
host-identity pinning.

## Host-key trust and connection reuse

A per-lease `known_hosts` file lives next to the key
(`<lease>/known_hosts`). When a coordinator response contains `sshHostKey`, the
CLI validates the OpenSSH public key and atomically writes exactly that key
under a stable lease alias before any readiness probe or other SSH transport.
Those connections use `StrictHostKeyChecking=yes`; an invalid key or unsafe
local trust path fails closed without attempting SSH. A refreshed authoritative
key replaces the prior isolated pin rather than appending stale trust.

Readiness stops promptly when OpenSSH rejects a host key, including during the
separate WSL SFTP probe. Waiting for guest startup cannot repair that rejection:
verify the lease identity and its SSH host trust before reconnecting. Detection
works across split writes and large diagnostics without retaining their text;
it does not replace keys or relax host-key checking. Other startup failures keep
their existing retry policy, and caller cancellation remains authoritative.

Targets without authoritative host-key metadata preserve the existing behavior.
Their SSH connections use:

- `StrictHostKeyChecking=accept-new` — trust a host's key on first contact, then
  pin it;
- `UserKnownHostsFile` pointed at the per-lease `known_hosts`;
- `IdentitiesOnly=yes` with `-i <key>` so only the lease key is offered;
- `ForwardAgent=no`, `ForwardX11=no`, and `ForwardX11Trusted=no` so broad local
  OpenSSH configuration cannot delegate ambient agent or X11 authority to a
  lease.

Because host keys are scoped to the lease's own file, a reused provider IP from
a previous lease never poisons the user's global `~/.ssh/known_hosts`, and two
leases sharing an address do not cross host-key state.

Provider-owned isolated trust flows, including Machine0 host rotation and Lume
bootstrap attestation, keep their own aliases and lifecycle rules.

On macOS and Linux, connection multiplexing is enabled
(`ControlMaster=auto`, `ControlPersist=10m`). Canonical per-lease credentials use
a private, short socket directory under `/tmp`, with separate sockets for each
endpoint and authentication/host-key identity. Reused IPs do not share connections
between leases. Windows OpenSSH and secret-authenticated targets disable multiplexing
(`ControlMaster=no`).

Route sharing retains the installed OpenSSH client's `%C` semantics: clients
before 9.6 do not distinguish changes to the configured ProxyJump hostname.
Even newer clients do not fingerprint the entire jump chain or SSH configuration.

If a multiplexed OpenSSH session cannot pass its file descriptors because the
local control socket is full, Crabbox retries that same session once. A second
exact `mux_client_request_session: send fds failed` transport failure switches
that invocation to `ControlMaster=no`. This recovery preserves the original
lease, SSH identity, host-key checks, proxy, port, and staged stdin. Only the
complete two-line local OpenSSH file-descriptor failure record authorizes this
retry; matching remote stderr, server-supplied log messages, and logs containing
unrelated records do not. Local diagnostics are captured through a private
temporary FIFO, without a disk log. After each attempt, Crabbox forwards up to
64 KiB to stderr and marks any truncation. The retry detector retains at most
512 bytes; overflow, incomplete capture, or output errors disable this recovery.
Capture ends when the foreground attempt exits, even if a persistent master remains.

Native Windows framed stdin transfers use asynchronous reads on Win32 OpenSSH's
pipe handle. They read the declared byte count before running a staged script;
short input or a read failure aborts the transfer. Empty frames do not initialize
stdin, and the reader preserves following bytes without closing the process-owned
handle. SSH identity, host-key checks, multiplexing, and ambiguous-disconnect
retry rules are unchanged.

## What the broker sees

In brokered mode the CLI sends only the public key to the coordinator; the
private key never leaves the local machine. The Worker imports or reuses that
public key in the target provider under a stable per-lease name derived from the
lease ID (`crabbox-<lease>`, with `_` rewritten to `-`):

- Hetzner uploads it as an SSH key, reusing an existing key with matching
  contents instead of creating a duplicate;
- AWS imports it as an EC2 key pair;
- Azure and GCP inject it through their respective instance metadata / key
  paths.

When the coordinator assigns a different final lease ID than the provisional one
the CLI started with, the CLI renames the local key directory to the final ID so
later `status`, `ssh`, `run --id`, and `stop` commands keep finding the key.

## Cleanup

Provider delete paths remove the per-lease cloud key or key pair when the
machine is deleted (for example AWS `DeleteKeyPair`, Hetzner SSH-key delete, and
the equivalent on other adapters). After a brokered provider deletion is
confirmed, the CLI removes that lease's local connection directory, including
its private/public key, certificate, and `known_hosts`. First it requests exit
through each lease-owned OpenSSH control socket and waits for those exact master
processes to exit. A local cleanup failure returns an error explicitly preserving
the confirmed remote deletion; the claim remains for a local-only Stop retry. It
preserves the directory when cleanup is queued, failed, canceled, retained, or
ownership cannot be confirmed, and never removes a configured shared key path.
Several direct provider backends likewise remove their generated lease directory
after confirmed destructive cleanup.

Configured shared keys outside the canonical lease directory retain their shared
connection scope; Stop does not close another lease's connections. Old clients'
flat `/tmp/crabbox-ssh-*` sockets are not adopted or swept: after their last
session, those masters retain their existing ten-minute idle expiry. The new
lease namespace does not reuse them.

## Bringing your own key

Setting `CRABBOX_SSH_KEY` (or the `ssh.key` config value) points the CLI at an
existing private key instead of a generated per-lease one. `doctor` validates
that key — checking the private path and its `.pub` sibling — only when
`CRABBOX_SSH_KEY` is set; otherwise it reports the default per-lease mode as
healthy.

## Related docs

- [Security](../security.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Identifiers](identifiers.md)
- [ssh command](../commands/ssh.md)
- [doctor command](../commands/doctor.md)
