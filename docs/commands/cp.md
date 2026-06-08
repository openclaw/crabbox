# cp

`crabbox cp` copies files or directories between the host and a delegated
sandbox that Crabbox owns. It resolves the lease id or slug first, then maps the
special `SANDBOX:PATH` side to the provider's native sandbox identifier.

```sh
crabbox cp --provider docker-sandbox --id blue-box ./coverage.xml SANDBOX:/tmp/coverage.xml
crabbox cp --provider docker-sandbox --id blue-box SANDBOX:/tmp/output.log ./output.log
crabbox cp --provider docker-sandbox --id blue-box -L ./src SANDBOX:/tmp/src
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
`<sandbox-name>:PATH` and then calls `sbx cp`.

## Flags

```text
--id <lease-id-or-slug>
--provider <name>
-L                      follow symbolic links when copying from host to sandbox
```

## Provider Support

This command is provider-opt-in. Providers without a native copy bridge fail
clearly instead of guessing. In this slice, `docker-sandbox` is the supported
provider.

## See also

- [`ports`](ports.md) - publish, list, or unpublish provider-native ports.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
