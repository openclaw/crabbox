# cp

`crabbox cp` copies files or directories between the host and a Crabbox-owned
lease. It resolves the lease id or slug first. Providers with a native copy
backend keep using that backend; SSH-backed leases otherwise transfer through
the resolved SSH transport with rsync or a validated archive stream.

```sh
crabbox cp --provider docker-sandbox --id blue-box ./coverage.xml SANDBOX:/tmp/coverage.xml
crabbox cp --provider docker-sandbox --id blue-box SANDBOX:/tmp/output.log ./output.log
crabbox cp --provider docker-sandbox --id blue-box -L ./src SANDBOX:/tmp/src
crabbox cp --provider ssh --id buildbox ./instructions SANDBOX:/tmp/instructions
crabbox cp --id blue-box SANDBOX:/tmp/results ./results
```

## Lease Resolution

`--id` is required. It accepts a Crabbox lease id or active slug. Providers only
act on Crabbox-owned leases; raw user-created sandboxes are rejected.

For SSH-backed copies, claim validation remains mandatory. The subsequent
best-effort lease refresh has a 20-second budget; a refresh failure warns and
copying continues without guaranteeing renewed expiry. Cancellation by the
caller stops the command instead. This budget does not limit the file transfer.

## Path Syntax

Exactly one side must use `SANDBOX:PATH`.

- Host to sandbox: `./file.txt SANDBOX:/tmp/file.txt`
- Sandbox to host: `SANDBOX:/tmp/file.txt ./file.txt`

`crabbox cp` does not support sandbox-to-sandbox copies.

For `provider=docker-sandbox`, Crabbox rewrites `SANDBOX:PATH` to
`<sandbox-name>:PATH` and then calls `sbx cp`. For an SSH lease, `SANDBOX:PATH`
is a path on the resolved remote host.

## Flags

`crabbox cp --help` (or `-h`) shows the invocation, path rule, both copy
directions, and copy-specific flags before the complete provider flag reference.

```text
--id <lease-id-or-slug>
--provider <name>
-L                      follow symbolic links when copying from host to sandbox
```

## Transport Selection

Provider-native copy remains the first choice when the backend implements it.
Otherwise, Crabbox accepts any provider that resolves a managed SSH lease and
uses the same `SSHTarget` used by `run`, sync, and interactive SSH. Crabbox
prefers rsync 3.4.3 or newer. On a POSIX operator host, if the local client is missing or older, it uses a
Go-owned tar+gzip archive stream over SSH instead; it never invokes the rejected
rsync client, scp, or another copy command. Delegated providers with neither
transport fail clearly.

The resolved SSH user, key/certificate paths, host-key policy, and ProxyCommand
are rendered into a mode-`0600` temporary OpenSSH config. The Crabbox-launched
rsync/ssh argv contains only that config path and a fixed non-secret alias;
secret SSH usernames are not placed in argv or environment variables.
Config-backed SSH routes materialize the effective authentication files and agent,
`HostName`, `ProxyJump`, or `ProxyCommand`; interactive session directives are
not inherited. OpenSSH executes the provider-resolved ProxyCommand under that
provider's existing transport contract. Crabbox keeps the private config for
the full transfer and removes it after the child exits.

The rsync path requires rsync on both sides. The local client must be rsync
3.4.3 or newer; Crabbox rejects older clients for data transfer because known
sender/receiver vulnerabilities cross the lease trust boundary. The archive
path instead requires standard POSIX tools including `bash`, `tar`, `gzip`,
`find`, `mktemp`, `awk`, `ps`, `ln`, and `sync` on the native Linux or macOS POSIX
lease. Local Go code creates uploads and validates downloads. Download entries must remain under one
source root and may contain only regular files and directories; Crabbox rejects
links (including hard links), special files, duplicate or escaping paths, archives over 1,000,000
entries, individual files over 64 GiB, or streams over 256 GiB. Tar header
checksums and the gzip stream checksum must validate before publication.

Archive uploads reject host-side symlinks by default and follow them with `-L`,
so no preserved link graph can escape the transferred tree.
Uploads extract into a private remote staging directory. Downloads extract into
a private directory beside the destination. Both replace the selected target
only after the complete archive validates. An interruption before publication
keeps the prior contents at the target or in its durable backup, which the next
copy restores if publication did not finish. An interruption after publication
keeps the new target and defers backup cleanup to the next copy. Replacement
uses a private durable `<target>.crabbox-cp-transaction` lock record and
`<target>.crabbox-cp-backup` sidecars; after a process or host crash, the next
copy to that target automatically restores an interrupted backup or finishes
cleanup of a published replacement. A backup without its marker is never
deleted automatically and produces an actionable error. This differs
from rsync's incremental merge: archive fallback replaces an existing selected
file or directory subtree on success instead of retaining unrelated entries in
that subtree.

Downloads preserve regular files, directories, and timestamps, but do not
materialize lease-provided symlinks or special files and do not apply remote
ownership or group metadata to the host. New local files and directories use
normalized non-executable modes (`0644` and `0755`, subject to the host umask)
instead of lease-provided permission bits. Native Windows
SSH targets use the workspace archive sync path and currently require a
provider-native copy backend; Windows operator hosts and WSL2 targets require
safe rsync 3.4.3 or newer.

On a Windows client, targets that depend on the native Windows OpenSSH config
use native rsync rather than WSL rsync so the configured route remains intact.

## See also

- [`ports`](ports.md) - publish, list, or unpublish provider-native ports.
- [`tunnel`](tunnel.md) - expose a remote loopback port locally over SSH.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
