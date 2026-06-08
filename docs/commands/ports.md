# ports

`crabbox ports` bridges provider-native port publishing for an existing Crabbox
lease. It resolves a Crabbox-owned lease id or slug first, then asks the active
provider to list, publish, or unpublish ports.

```sh
crabbox ports --provider docker-sandbox --id blue-box
crabbox ports --provider docker-sandbox --id blue-box --publish 3000
crabbox ports --provider docker-sandbox --id blue-box --unpublish 41000:3000
crabbox ports --provider docker-sandbox --id blue-box --json
```

## Lease Resolution

`--id` is required. It accepts a Crabbox lease id or active slug. Providers only
act on Crabbox-owned leases; raw user-created sandboxes are rejected.

For `provider=docker-sandbox`, Crabbox resolves the local `dsbx_...` claim and
then calls `sbx ports <sandbox-name> ...`.

## Flags

```text
--id <lease-id-or-slug>
--provider <name>
--publish <spec>      publish a port mapping; repeatable
--unpublish <spec>    unpublish a port mapping; repeatable
--json                print provider-native listing output as JSON
```

`--publish` and `--unpublish` cannot be combined in the same call.

For Docker Sandbox, the spec format mirrors `sbx ports`:

```text
[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]
```

Examples:

- `3000` lets Docker Sandbox allocate an ephemeral host port for sandbox port
  `3000`.
- `41000:3000` binds host port `41000` to sandbox port `3000`.
- `127.0.0.1:41000:3000/tcp4` restricts the published port to IPv4 loopback.

## Provider Support

This command is provider-opt-in. Providers without a native port-publishing
bridge fail clearly instead of guessing. In this slice, `docker-sandbox` is the
supported provider.

## See also

- [`cp`](cp.md) - copy files between host and a delegated sandbox.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
