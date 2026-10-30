# cp

`crabbox cp` copies files or directories between the host and a Crabbox-owned
lease. It resolves the lease id or slug first. Providers with a native copy
backend keep using that backend; SSH-backed leases otherwise use the resolved
SSH transport with rsync, a validated archive stream, or the native Windows
regular-file scp/SFTP path.

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
--recover <choice>      adopt legacy archive recovery data at one exact destination
```

## Transport Selection

Provider-native copy remains the first choice when the backend implements it.
Otherwise, Crabbox accepts any provider that resolves a managed SSH lease and
uses the same `SSHTarget` used by `run`, sync, and interactive SSH. Native
Windows targets (`--target windows` in normal mode) copy one regular file over
SFTP with the operator's native OpenSSH client; see
[Native Windows regular files](#native-windows-regular-files). For other
targets, Crabbox prefers rsync 3.4.3 or newer. On a POSIX operator host, if the
local client is missing or older, it uses a Go-owned tar+gzip archive stream
over SSH instead; it never invokes the rejected rsync client, scp, or another
copy command. Delegated providers with neither transport fail clearly.

The resolved SSH user, key/certificate paths, host-key policy, and ProxyCommand
are rendered into a mode-`0600` temporary OpenSSH config. The Crabbox-launched
transfer argv contains that config path and a fixed non-secret alias;
secret SSH usernames are not placed in argv or environment variables.
Config-backed SSH routes materialize the effective authentication files and agent,
`HostName`, `ProxyJump`, or `ProxyCommand`; interactive session directives are
not inherited. OpenSSH executes the provider-resolved ProxyCommand under that
provider's existing transport contract. Crabbox keeps the private config for
the full transfer and removes it after the child exits.

### Native Windows regular files

The Windows guest path copies **one regular file** per command, in either
direction. It does not copy directory trees, links, devices, or wildcard
matches, and it rejects `-L`. Give a drive-qualified absolute guest filename
(no `.`/`..`, alternate data streams, reserved device names, or trailing dots
or spaces) and an explicit local filename. The guest destination directory must
already exist, and no directory on its path may be a link or junction. Paths
containing spaces remain one argument.

```powershell
crabbox cp --provider hyperv --id windows-test .\input.zip 'SANDBOX:C:\crabbox\input file.zip'
crabbox cp --provider hyperv --id windows-test 'SANDBOX:C:\crabbox\results.zip' .\results.zip
```

Requirements:

- A Windows operator host. Other operator hosts fail with exit 2.
- The native Win32 OpenSSH `ssh.exe` first on `PATH`, OpenSSH 9.0 or newer,
  with a `scp.exe` of the same version beside it. OpenSSH 9 `scp` uses the SFTP
  protocol. WSL or MSYS clients and mismatched pairs fail with exit 2.
- A resolved lease key and strict host-key checking.
- The guest's OpenSSH SFTP subsystem. Crabbox's Windows guest bootstraps
  configure one when it is missing.

This path never falls back to legacy `scp -O`, disables host-key checking, or
loads a direct lease's personal SSH configuration. Other SCP callers are
unchanged.

An upload first snapshots the source into a private local staging directory
under the operator's temporary directory. It then uploads into an owned guest
staging directory, `.crabbox-cp-<random>` beside the destination, and the guest
publishes the file with an atomic replace only after the staged copy matches
the snapshot's size and SHA-256. A download reads the guest file's size and
SHA-256 first, streams into a private staging directory beside the local
destination, aborts if the transfer grows past the reported size, and verifies
the result before an atomic write-through replace. Neither direction buffers
the file in memory. An existing destination file stays unchanged when
transfer, verification, or publication fails. Guest staging is removed only
after its ownership marker matches. A cleanup failure after a successful
publication reports that the verified file was published and names the
retained staging directory.

The copy holds the lease claim for its whole duration, including staging
cleanup, using the same shared claim fence as `crabbox exec`. It starts only if
the lease is still this repository's claim on the resolved resource. Otherwise
it stops before any guest or local file effect: exit 4, or exit 2 with
`claim changed; retry` when the claim changed while the fence was being taken.
While it runs, claim changes such as another repository's `--reclaim`, `stop`,
or an endpoint refresh wait for it to finish, within their own time limits.

| Limit | Value |
| --- | --- |
| Whole command, including lease resolution, client checks, and transfer | 9 minutes |
| Owned guest staging cleanup | 30 seconds, outside the command budget |
| SSH connect | 10 seconds, one attempt |
| File size | 64 GiB |
| Free local space | the file size plus 1 GiB, in the operator's temporary directory for uploads or the destination directory for downloads |

The guest needs room for a second copy of the file beside the destination
while it is staged.

`scripts\test-windows-native.ps1` is an opt-in lane for Windows operator hosts.
It downloads a pinned, SHA-256-verified portable Win32-OpenSSH build into a new
directory and runs the real `scp.exe`/`ssh.exe` against its `sftp-server`
through a local protocol probe. It checks SFTP INIT/VERSION packets, overwrite,
a missing source, a denied publication, cancellation and deadline, corrupted
protocol output, and a 2 GiB-plus-17-byte round trip. It starts no SSH service,
performs no SSH authentication, changes no global settings, and fails if any
named case is skipped. The large round trip takes several minutes, so the lane
is not part of default CI.

### POSIX and WSL transports

The rsync path requires rsync on both sides. The local client must be rsync
3.4.3 or newer; Crabbox rejects older clients for data transfer because known
sender/receiver vulnerabilities cross the lease trust boundary. The archive
path installs a temporary filesystem runner on the native Linux or macOS
lease. The same Go implementation creates and validates archives on both sides,
using the resolved SSH transport. Download entries must remain under one
source root and may contain only regular files and directories; Crabbox rejects
links (including hard links), special files, duplicate or escaping paths, archives over 1,000,000
entries, individual files over 64 GiB, or streams over 256 GiB. Tar header
checksums and the gzip stream checksum must validate before publication.

Archive uploads reject host-side symlinks by default and follow them with `-L`,
so no preserved link graph can escape the transferred tree.
Uploads extract into a private remote staging directory. Downloads extract into
a private directory beside the destination. Both replace the selected target
only after the complete archive validates. An interruption before publication
keeps the prior contents at the target or in its durable backup. A private
filesystem journal records publication and cleanup, so the next copy can recover
an interrupted operation. Historical `.crabbox-cp-transaction` and
`.crabbox-cp-backup` sidecars are not automatically adopted or deleted; a copy
that encounters them stops and preserves them for explicit recovery. This differs
from rsync's incremental merge: archive fallback replaces an existing selected
file or directory subtree on success instead of retaining unrelated entries in
that subtree.

Downloads preserve regular files, directories, and timestamps, but do not
materialize lease-provided symlinks or special files and do not apply remote
ownership or group metadata to the host. New local files and directories use
normalized non-executable modes (`0644` and `0755`, subject to the host umask)
instead of lease-provided permission bits. On these transports, Windows
operator hosts and WSL2 targets require safe rsync 3.4.3 or newer.

On a Windows client, targets that depend on the native Windows OpenSSH config
use native rsync rather than WSL rsync so the configured route remains intact.

## Interrupted archive recovery

New archive copies record their publication decision in a private persistent
journal under the user's configuration directory, outside the transferred tree.
Before that decision, the old destination remains unchanged. After the decision,
the next copy resumes publication and cleanup using the recorded destination and
file identities. Do not remove this journal while a transaction is unresolved.

Older releases left `.crabbox-cp-transaction` and `.crabbox-cp-backup` sidecars
that do not identify an intended recovery outcome reliably. Copying stops when
it encounters those files and prints the exact destination and recovery commands.
Inspect the destination and backup, then explicitly choose one outcome:

```sh
crabbox cp --recover keep-destination -- ./exact-destination
crabbox cp --recover restore-backup -- ./exact-destination

crabbox cp --recover keep-destination --id blue-box -- SANDBOX:/tmp/exact-destination
crabbox cp --recover restore-backup --id blue-box -- SANDBOX:/tmp/exact-destination
```

`keep-destination` leaves the current destination as it is, including absent,
and retains any backup. `restore-backup` requires a backup and restores it while
retaining the previous destination. Both retain the old marker and all unselected
data in a collision-safe private sibling directory whose path is printed. No
legacy data is deleted implicitly. Remove retained data only after inspecting it
and confirming that it is no longer needed.

Recovery takes one exact destination, not a source/destination pair, and does not
accept `-L`. Local recovery accepts no lease/provider flags. Remote recovery
requires a claimed POSIX SSH lease; provider-native copy backends and native
Windows archive recovery are not supported. If transport fails after recovery,
the outcome is ambiguous: inspect the destination and retained data before
choosing any further action.

## See also

- [`ports`](ports.md) - publish, list, or unpublish provider-native ports.
- [`tunnel`](tunnel.md) - expose a remote loopback port locally over SSH.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
