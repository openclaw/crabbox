# cp

`crabbox cp` copies files or directories between the host and a Crabbox-owned
lease. It resolves the lease id or slug first. Providers with a native copy
backend keep using that backend; SSH-backed leases otherwise transfer through
the resolved SSH transport with rsync.

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
uses rsync over the same `SSHTarget` used by `run`, sync, and interactive SSH.
Delegated providers with neither transport fail clearly.

The resolved SSH user, key/certificate paths, host-key policy, and ProxyCommand
are rendered into a mode-`0600` temporary OpenSSH config. The Crabbox-launched
rsync/ssh argv contains only that config path and a fixed non-secret alias;
secret SSH usernames are not placed in argv or environment variables.
Config-backed SSH routes materialize only the effective `HostName`,
`ProxyJump`, or `ProxyCommand`; interactive session directives are not
inherited. OpenSSH executes the provider-resolved ProxyCommand under that
provider's existing transport contract. Crabbox removes the config when the
transfer exits.

SSH fallback requires rsync on both sides. The local client must be rsync 3.4.3
or newer; Crabbox rejects older clients before connecting because known
sender/receiver vulnerabilities cross the lease trust boundary. Native Windows
SSH targets use the archive sync path and currently require a provider-native
copy backend; WSL2 targets use the SSH rsync fallback after Crabbox verifies
that the remote rsync supports secluded arguments.

On a Windows client, targets that depend on the native Windows OpenSSH config
use native rsync rather than WSL rsync so the configured route remains intact.

## See also

- [`ports`](ports.md) - publish, list, or unpublish provider-native ports.
- [`tunnel`](tunnel.md) - expose a remote loopback port locally over SSH.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
