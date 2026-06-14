# ports

`crabbox ports` bridges provider-native port publishing for an existing Crabbox
lease. It resolves a Crabbox-owned lease id or slug first, then asks the active
provider to list, publish, or unpublish ports.

```sh
crabbox ports --provider docker-sandbox --id blue-box
crabbox ports --provider docker-sandbox --id blue-box --publish 3000
crabbox ports --provider docker-sandbox --id blue-box --unpublish 41000:3000
crabbox ports --provider docker-sandbox --id blue-box --json
crabbox ports --provider codesandbox --id web-box --publish 3000 --json
```

## Lease Resolution

`--id` is required. It accepts a Crabbox lease id or active slug. Providers only
act on Crabbox-owned leases; raw user-created sandboxes are rejected.

For `provider=docker-sandbox`, Crabbox resolves the local `dsbx_...` claim and
then calls `sbx ports <sandbox-name> ...`.

For `provider=codesandbox`, Crabbox resolves the local `csbx_...` claim,
verifies the remote CodeSandbox ownership tag, then asks the CodeSandbox SDK
bridge for open ports or provider-owned host URLs.

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

For CodeSandbox, the spec is only the sandbox port number:

```text
SANDBOX_PORT
```

`--publish 3000` waits for port `3000` in the sandbox and prints the
CodeSandbox SDK host URL. With `--json`, CodeSandbox returns objects with
`port`, `host`, and `url` fields. `--unpublish` is not supported for
CodeSandbox because the SDK observes ports owned by running processes rather
than exposing a close-port operation; stop the process inside the sandbox.

## Provider Support

This command is provider-opt-in. Providers without a native port-publishing or
URL bridge fail clearly instead of guessing. Supported providers:

- `docker-sandbox` — native `sbx ports` list, publish, and unpublish.
- `codesandbox` — SDK open-port listing and host URL publishing.

## See also

- [`cp`](cp.md) - copy files between host and a delegated sandbox.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
- [CodeSandbox provider](../providers/codesandbox.md) - provider-specific
  host URL and preview token notes.
