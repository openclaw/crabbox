# External Provider

Use `provider: external` when lifecycle and connection discovery belong to an
internal, proprietary, or separately versioned tool. Crabbox launches one
configured executable per operation. There is no shell evaluation and no
provider-specific code in Crabbox.

The executable owns provisioning, inventory, resume, release, and any private
authentication. It returns an SSH target; Crabbox then owns dirty-tree sync,
rsync, commands, results, SSH sessions, and native VNC forwarding when the
provider returns desktop metadata.

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

Local claims are scoped to a fingerprint of `external.command`, `external.args`,
and `external.config`. This lets multiple external backends or namespaces reuse
the same slug without cleanup for one configuration removing claims or routing
files owned by another. Legacy unscoped claims are not reconciled by cleanup;
stop them directly by lease ID or with the generated routing file.

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
CRABBOX_LIVE_EXTERNAL_COMMAND=/absolute/path/provider \
scripts/live-smoke.sh
```

The command may also come from `external.command` in the Crabbox config.
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
