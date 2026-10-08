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
Otherwise, Crabbox accepts an SSH lease backend and uses its resolved
`SSHTarget`. Native Windows files use the SFTP path below. Other SSH targets
retain rsync or validated POSIX archive behavior.

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

The Windows guest path supports **individual regular files**, not directory
trees, links, devices, wildcard expansion or `-L`. Specify a drive-qualified
absolute guest filename and an explicit local filename; create the guest
destination directory first. Paths containing spaces remain one argument.

```powershell
crabbox cp --provider hyperv --id windows-test .\input.zip 'SANDBOX:C:\crabbox\input file.zip'
crabbox cp --provider hyperv --id windows-test 'SANDBOX:C:\crabbox\evidence.zip' .\evidence.zip
```

Use a supported native Windows OpenSSH `scp.exe`/`ssh.exe` pair with default
SFTP support (OpenSSH 9.0 or newer), and a working guest SFTP subsystem. Missing
or unsupported tools/subsystems fail with a prerequisite error. This path
never falls back to legacy `scp -O`, disables host-key checking, or loads a
direct lease's personal SSH configuration. Existing non-copy legacy SCP
callers are not changed.

Copies use owned staging files, streamed size/SHA-256 verification, and publish
only after successful transfer. Existing final files are preserved on failure;
multi-GB payloads are not buffered in memory. Allow room for staging and report
cleanup errors rather than treating a partial export as success. The Windows
transfer budget is nine minutes, with bounded cleanup of owned staging.
The public claim/admission checks run before either transfer direction.

The prior final file is preserved when transfer, verification or publication
fails. A cleanup failure after a successful publication instead reports that
the verified file was published and identifies the retained owned staging
directory. Local publication uses a Windows atomic write-through replacement;
no POSIX directory-sync assumption or legacy SCP fallback is used.

The dedicated `scripts\test-windows-native.ps1` lane downloads a pinned,
hash-verified portable OpenSSH fixture into a new directory and checks actual
SFTP INIT/VERSION packets, cancellation and a 2 GiB-plus-17-byte round trip.
It starts no SSH service, performs no SSH authentication, and changes no global
settings. The CI lane requires every named case to run and pass without skips.

Windows source snapshots compare native full 128-bit file identities, avoiding
mixed directory/handle IDs on ReFS. To explicitly exercise an existing ReFS
volume in the identity regression lane, set `CRABBOX_TEST_FILESYSTEM_ROOT` to
an owned test parent and `CRABBOX_TEST_REQUIRE_REFS=1`. The latter fails if the
fixture is not really on ReFS. The tests retain replacement and symlink negatives;
ordinary Windows CI proves the native mechanism on its available filesystem
without pretending that NTFS is ReFS coverage.

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
file or directory subtree on success instead of retaining unrelated entries.

Downloads preserve regular files, directories, and timestamps, but do not
materialize lease-provided symlinks or special files and do not apply remote
ownership or group metadata to the host. New local files and directories use
normalized non-executable modes (`0644` and `0755`, subject to the host umask)
instead of lease-provided permission bits.

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
