# External Provider

Use `provider: external` when lifecycle and connection discovery belong to an
internal, proprietary, or separately versioned tool. Choose either:

- the versioned JSON protocol for adapters that need arbitrary logic; or
- declarative lifecycle commands for CLIs whose resource name and SSH target
  are deterministic.

There is no shell evaluation and no provider-specific code in Crabbox. The
external tool owns provisioning, inventory, resume, release, and private
authentication. Crabbox owns dirty-tree sync, rsync, commands, results, SSH
sessions, native VNC forwarding, and local WebVNC for desktop-capable direct
SSH leases.

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
the Crabbox config file. Generated SSH, retry, and stop commands store resolved
command, arguments, and config in a private per-lease routing file and print
only that file's opaque path. Kept acquisition failures persist this routing
before the SSH readiness wait, so the printed recovery commands can still
resolve or release the lease. Routing files use mode `0600` and are removed
after successful release.

Local claims are scoped to a fingerprint of the selected protocol command or
declarative lifecycle, connection templates, and `external.config`. This lets
multiple external backends or namespaces reuse the same slug without cleanup
for one configuration removing claims or routing files owned by another.
Legacy unscoped claims are not reconciled by cleanup; stop them directly by
lease ID or with the generated routing file.

## Declarative lifecycle

Use declarative lifecycle commands when the provider CLI can create a
preselected resource name and expose that resource through a deterministic SSH
alias or hostname:

```yaml
provider: external
target: linux
external:
  lifecycle:
    doctor:
      argv: [devboxctl, list, --format, json]
    acquire:
      argv: [devboxctl, new, "{{resourceName}}", --size, "{{config.size}}"]
    resolve:
      argv: [devboxctl, inspect, "{{resourceName}}"]
    list:
      argv: [devboxctl, list, --format, json]
      output: json-name-array
      namePrefix: "cbx-"
    release:
      argv: [devboxctl, rm, --yes, "{{resourceName}}"]
    touch:
      argv: [devboxctl, touch, "{{resourceName}}"]
    cleanup:
      argv: [devboxctl, gc]
  connection:
    resourceName: "{{leaseIdSlug}}"
    cloudId: devboxes/{{resourceName}}
    serverType: "{{config.size}}"
    labels:
      backend: container
    ssh:
      user: "{{env.DEVBOX_USER}}"
      host: "{{resourceName}}"
      port: "22"
      sshConfigProxy: true
      readyCheck: command -v git && command -v rsync && command -v tar
  config:
    size: cpu16
  workRoot: /home/developer/crabbox
```

`acquire`, `list`, `release`, and `connection.ssh.user` are required.
`connection.resourceName` defaults to `{{name}}`; use it when the provider has
stricter resource-name limits than Crabbox. Crabbox stores the resolved value
with the lease so `json-name-array` inventory can recover the original lease
ID and slug. `connection.ssh.host` defaults to `{{resourceName}}`. Optional
operations are skipped when absent; Crabbox still maintains local touch
metadata.

Each `argv` item is passed directly to the configured executable. Shell
operators, pipes, variable expansion, and command substitution are not
evaluated. Supported placeholders:

```text
{{leaseId}} {{leaseIdSlug}} {{slug}} {{name}} {{resourceName}} {{id}} {{state}}
{{keep}} {{reclaim}} {{releaseOnly}} {{force}}
{{all}} {{refresh}} {{dryRun}}
{{repo.root}} {{repo.name}} {{repo.remoteUrl}} {{repo.head}} {{repo.baseRef}}
{{config.<scalar-key>}}
{{env.<NAME>}}
```

Environment placeholders require the named variable to be set. Do not place
secret environment values in lifecycle arguments: process arguments may be
visible to other local processes. Provider CLIs should use their normal
credential store or inherited environment for authentication.

`leaseIdSlug` is the lease ID normalized as a lowercase slug, suitable for
providers that require DNS-style names. `resourceName` is the expanded
`connection.resourceName`.

`list.output` accepts:

- `json-name-array`: stdout is a JSON array of resource names;
- `json-lease-array`: stdout is a JSON array using the protocol lease shape
  documented below.

For `json-name-array`, optional `list.namePrefix` discards inventory names
outside the expanded prefix before Crabbox constructs leases. Use it when a
provider CLI lists resources that are not all owned by this configuration.

Declarative configuration and resolved connection templates are included in
the private per-lease routing file. This lets generated retry, daemon, SSH, and
stop commands work without the original config file.

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
CRABBOX_EXTERNAL_ARG
CRABBOX_EXTERNAL_WORK_ROOT
CRABBOX_EXTERNAL_ROUTING_FILE
```

The repository live harness can exercise a configured external provider:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=external \
scripts/live-smoke.sh
```

The provider may come from declarative lifecycle configuration or
`external.command` in the Crabbox config. Set
`CRABBOX_LIVE_EXTERNAL_COMMAND=/absolute/path/provider` to override the
protocol command.
`CRABBOX_LIVE_EXTERNAL_ARG` adds one command argument through
`CRABBOX_EXTERNAL_ARG` for quick local smoke runs; use config `external.args`
for repeatable multi-argument setups.
`CRABBOX_LIVE_COMMAND` overrides the command executed inside the leased box.
The live harness also needs `jq` and `rg` on the operator machine.

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
is automatically reused by rsync and WebVNC. A non-empty `proxyCommand` implies
proxy-backed SSH even if `sshConfigProxy` is omitted.

When `readyCheck` is omitted, Crabbox uses a generic Linux tool check for
`bash`, `python3`, `git`, `rsync`, and `tar`. Return an explicit `readyCheck`
when the external provider needs a stronger guest bootstrap signal.

`list` returns `{"protocolVersion":1,"leases":[...]}`. `doctor` may return a
human-readable `message`. Any operation may return `{"error":"..."}`; error-only
responses do not need `protocolVersion`.

For `acquire`, omitted `leaseId`, `slug`, or `name` fields inherit the
corresponding values from `desired`.

`leaseId` is Crabbox's local lease identity. For new `acquire` responses and
non-release `resolve` responses, it must be the generated `cbx_...` value from
`desired`. External systems should put their own resource identifier in
`cloudId`; Crabbox persists claims and stop routing by `leaseId`. Release-only
paths still accept older protocol-v1 provider IDs so existing leases can be
stopped, but path-shaped legacy IDs are not used for local claim deletion.
