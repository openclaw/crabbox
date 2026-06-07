# External Provider

Use `provider: external` when lifecycle and connection discovery belong to an
internal, proprietary, or separately versioned tool. Crabbox launches one
configured executable per operation. There is no shell evaluation and no
provider-specific code in Crabbox.

The executable owns provisioning, inventory, resume, release, and any private
authentication. It returns an SSH target; Crabbox then owns dirty-tree sync,
rsync, commands, results, SSH sessions, and WebVNC tunnels.

## Configuration

```yaml
provider: external
target: linux
external:
  command: node
  args:
    - /absolute/path/provider.mjs
  config:
    backend: vm
    namespace: team-devboxes
    size: cpu32
  workRoot: /workspaces/crabbox
  routingFile: ""
```

`external.config` is arbitrary YAML passed as JSON to the executable. Keep
secrets in the executable's normal credential store or environment rather than
the Crabbox config file. Generated stop commands store resolved command,
arguments, and config in a private per-lease routing file and print only that
file's opaque path. Routing files use mode `0600` and are removed after
successful release.

Flags:

```text
--external-command
--external-arg
--external-config-json
--external-work-root
--external-routing-file
```

Environment:

```text
CRABBOX_EXTERNAL_COMMAND
CRABBOX_EXTERNAL_WORK_ROOT
CRABBOX_EXTERNAL_ROUTING_FILE
```

## Protocol

Protocol version 1 uses JSON over stdio:

- stdin: exactly one request object;
- stdout: exactly one response object;
- stderr: diagnostics for the user;
- exit nonzero: operation failure.

Request shape:

```json
{
  "protocolVersion": 1,
  "operation": "acquire",
  "config": {"backend": "vm"},
  "desired": {
    "leaseId": "cbx_0123456789ab",
    "slug": "fast-coral",
    "name": "crabbox-fast-coral-deadbeef"
  },
  "keep": false,
  "reclaim": false,
  "repo": {
    "root": "/workspace/my-app",
    "name": "my-app",
    "remoteUrl": "https://github.com/example-org/my-app.git",
    "head": "0123456789abcdef",
    "baseRef": "main"
  }
}
```

Operations:

```text
doctor
acquire
resolve
list
release
touch
cleanup
```

Successful lease response:

```json
{
  "protocolVersion": 1,
  "lease": {
    "leaseId": "cbx_0123456789ab",
    "slug": "fast-coral",
    "name": "devbox-fast-coral",
    "cloudId": "private-control-plane/devbox-fast-coral",
    "status": "ready",
    "serverType": "cpu32",
    "labels": {"region": "example"},
    "ssh": {
      "user": "dev",
      "host": "devbox-fast-coral",
      "port": "22",
      "key": "/absolute/path/id_ed25519",
      "sshConfigProxy": true,
      "proxyCommand": "private-cli proxy devbox-fast-coral %p",
      "readyCheck": "command -v git && command -v rsync && command -v tar"
    }
  }
}
```

`sshConfigProxy: true` supports an SSH alias already installed in the user's
SSH config. `proxyCommand` supports control-plane tunnels directly. Either path
is automatically reused by rsync and WebVNC.

`list` returns `{"protocolVersion":1,"leases":[...]}`. `doctor` may return a
human-readable `message`. Any operation may return `{"error":"..."}`.

For `acquire`, omitted `leaseId`, `slug`, or `name` fields inherit the
corresponding values from `desired`.
