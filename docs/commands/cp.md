# cp

`crabbox cp` copies data between the host and a Crabbox lease. It resolves the
lease id or slug first, then uses either the provider's native copy bridge or
the lease's resolved SSH transport.

```sh
crabbox cp --provider docker-sandbox --id blue-box ./coverage.xml SANDBOX:/tmp/coverage.xml
crabbox cp --provider docker-sandbox --id blue-box SANDBOX:/tmp/output.log ./output.log
crabbox cp --provider docker-sandbox --id blue-box -L ./src SANDBOX:/tmp/src
crabbox cp --provider ssh --id buildbox.local ./input.json SANDBOX:/tmp/input.json
crabbox cp --provider ssh --id buildbox.local SANDBOX:/tmp/output.json ./output.json
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
`<sandbox-name>:PATH` and then calls `sbx cp`. For an SSH-reachable provider,
Crabbox maps that path to the resolved SSH target and calls the local `scp`
client with the same key, certificate, proxy command, and host-key policy used
by `crabbox connect`.

## Flags

```text
--id <lease-id-or-slug>
--provider <name>
-L                      follow symbolic links when copying from host to sandbox
```

## Provider Support

This command works with providers that expose either a native copy bridge or an
SSH-reachable lease. The native bridge defines whether directory copies and
`-L` are supported; the SSH path copies regular files. Crabbox rejects
token-as-username SSH targets because passing that secret through `scp` is not
portable.

## See also

- [`ports`](ports.md) - publish, list, or unpublish provider-native ports.
- [`tunnel`](tunnel.md) - forward a remote loopback port through SSH.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
