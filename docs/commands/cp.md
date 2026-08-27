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

## Path Syntax

Exactly one side must use `SANDBOX:PATH`.

- Host to sandbox: `./file.txt SANDBOX:/tmp/file.txt`
- Sandbox to host: `SANDBOX:/tmp/file.txt ./file.txt`

`crabbox cp` does not support sandbox-to-sandbox copies.

For `provider=docker-sandbox`, Crabbox rewrites `SANDBOX:PATH` to
`<sandbox-name>:PATH` and then calls `sbx cp`. For an SSH lease, `SANDBOX:PATH`
is a path on the resolved remote host.

## Flags

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
path installs a versioned Crabbox runner on the native Linux or macOS lease.
Official CLIs embed all supported runner binaries; the lease needs neither Go
nor internet access. Development builds compile their embedded runner source
with Go 1.26 or later on the operator. Installation needs `sh`, `mktemp`, and
`sha256sum` or `shasum`, verifies the exact binary digest, and preserves separate
versions in the user's private cache. The same Go implementation creates and
validates archives at both ends. Download entries must remain under one
source root and may contain only regular files and directories; Crabbox rejects
links (including hard links), special files, duplicate or escaping paths, archives over 1,000,000
entries, individual files over 64 GiB, or streams over 256 GiB. Tar header
checksums and the gzip stream checksum must validate before publication.

Archive uploads reject host-side symlinks by default and follow them with `-L`,
so no preserved link graph can escape the transferred tree.
Remote paths may be absolute, relative to the SSH user's working directory, or
under `~/`. The selected destination itself cannot be a symlink; existing parent
directory aliases are supported. Unlike workspace sync, `cp` operates on the
explicitly requested filesystem path and is not restricted to the lease workdir.
The archive transport rejects paths that cannot be represented in UTF-8 metadata
rather than silently changing the requested filename.
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

Current clients read both earlier local and remote transaction formats. New
records use the existing versioned local format; an older remote-shell client
refuses an interrupted new transaction rather than guessing its owner. Use a
current client to recover it. Both the complete archive and final protocol frame
must validate before publication; file contents cannot impersonate protocol
markers.

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
