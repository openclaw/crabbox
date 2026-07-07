# CubeSandbox Provider

Read when:

- choosing `provider: cubesandbox`;
- running commands in a CubeSandbox deployment through its E2B-compatible API;
- configuring Cube API, CubeProxy, templates, or data-plane access;
- changing `internal/providers/cubesandbox`.

[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) is an
E2B-compatible MicroVM sandbox platform. Crabbox treats it as a `delegated-run`
provider: CubeSandbox owns sandbox lifecycle, file upload, and command
execution, while Crabbox owns local config, repo claims, archive sync,
guardrails, timing summaries, and normalized `list`/`status` output.

## When To Use

Use CubeSandbox when commands should run inside a CubeSandbox KVM MicroVM and
you do not need a normal SSH lease. The provider works with self-hosted
CubeSandbox deployments and the standard Cube API / CubeProxy layout.

Use an SSH-lease provider such as `aws`, `hetzner`, or `ssh` when the workflow
requires `crabbox ssh`, VNC, code-server, GitHub Actions runner hydration, or
host-managed SSH access.

## Prerequisites

- A reachable Cube API endpoint, usually `http://<cubeapi-host>:3000`.
- A ready CubeSandbox template ID, for example from `CUBE_TEMPLATE_ID`.
- CubeProxy data-plane routing. If DNS for `49983-<sandbox>.<domain>` is not
  available from the Crabbox host, configure `CUBE_PROXY_NODE_IP`.

## Commands

```sh
export CUBE_API_URL=http://cubeapi.example.internal:3000
export CUBE_TEMPLATE_ID=<template-id>
export CUBE_PROXY_NODE_IP=cubeproxy.example.internal
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app

crabbox run --provider cubesandbox -- go test ./...
crabbox run --provider cubesandbox --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox warmup --provider cubesandbox --slug cube-smoke
crabbox status --provider cubesandbox --id cube-smoke --wait
crabbox stop --provider cubesandbox cube-smoke
```

`run` without `--id` creates a sandbox from the configured template, archive
syncs the checkout into the sandbox workdir, runs the command through envd's
Connect process API, and deletes the sandbox unless `--keep` or failure-retention
options keep it. `warmup` creates a retained sandbox; stop it explicitly.

## Auth

```sh
export CUBE_API_KEY=...
```

`CRABBOX_CUBESANDBOX_API_KEY` also works and wins over `CUBE_API_KEY`.
For E2B-compatible environments, `E2B_API_KEY` is accepted as a fallback. The
key is sent as `Authorization: Bearer <token>` to the Cube API and is never
registered as a command-line flag.

Many self-hosted CubeSandbox deployments accept unauthenticated local API
traffic or any non-empty E2B-compatible key. Leave the key unset only on trusted
networks where the Cube API is intentionally unauthenticated.

## Config

```yaml
provider: cubesandbox
target: linux
cubeSandbox:
  apiUrl: http://cubeapi.example.internal:3000
  template: <template-id>
  domain: cube.app
  workdir: crabbox
  user: root
  proxyNodeIp: cubeproxy.example.internal
  proxyPortHttp: 80
  proxyScheme: http
```

Provider flags:

```text
--cubesandbox-api-url
--cubesandbox-domain
--cubesandbox-template
--cubesandbox-workdir
--cubesandbox-user
--cubesandbox-proxy-node-ip
--cubesandbox-proxy-port-http
--cubesandbox-proxy-scheme
```

Environment overrides:

```text
CRABBOX_CUBESANDBOX_API_KEY / CUBE_API_KEY / E2B_API_KEY
CRABBOX_CUBESANDBOX_API_URL / CUBE_API_URL / E2B_API_URL
CRABBOX_CUBESANDBOX_DOMAIN / CUBE_SANDBOX_DOMAIN
CRABBOX_CUBESANDBOX_TEMPLATE / CUBE_TEMPLATE_ID
CRABBOX_CUBESANDBOX_WORKDIR
CRABBOX_CUBESANDBOX_USER
CRABBOX_CUBESANDBOX_PROXY_NODE_IP / CUBE_PROXY_NODE_IP
CRABBOX_CUBESANDBOX_PROXY_PORT_HTTP / CUBE_PROXY_PORT_HTTP
CRABBOX_CUBESANDBOX_PROXY_SCHEME / CUBE_PROXY_SCHEME
```

Defaults: API URL `http://127.0.0.1:3000`, sandbox domain `cube.app`, workdir
`/root/crabbox`, process user `root`, proxy port `80`, and proxy scheme derived
from the proxy port (`http` unless port is `443`). A template is required for
new sandboxes.

When `proxyNodeIp` is set, Crabbox connects to
`<proxyScheme>://<proxyNodeIp>:<proxyPortHttp>` and preserves the envd virtual
sandbox host header (`49983-<sandbox>.<domain>`). Leave `proxyNodeIp` empty when
that virtual host already resolves from the Crabbox host.

## Capabilities

- Target: Linux only.
- SSH: no.
- Crabbox sync: no normal SSH/rsync; archive sync is delegated through envd file
  upload.
- Generic `run`: yes, through `/bin/bash -l -c` in the CubeSandbox envd process
  API.
- Warmup: yes, retained sandbox from a template.
- Stop/delete: yes, for Crabbox-claimed sandboxes.
- Desktop/browser/code: no.
- Coordinator: no (direct from CLI only).

## Gotchas

- `--class` and `--type` are rejected; choose the template and CubeSandbox node
  capacity outside Crabbox.
- `--checksum` is rejected because sync is archive-based.
- The provider requires a template ID for create/warmup/run-without-id.
- HTTP Cube API URLs are supported for self-hosted deployments. Do not send real
  API keys over untrusted HTTP networks.
- Reuse, status, and stop require a local claim bound to the exact Cube API
  endpoint, sandbox ID, provider, and canonical remote lease metadata. Labels
  alone never authorize a mutable or destructive operation.
- To adopt a labelled legacy or externally restored sandbox, use its exact
  CubeSandbox sandbox ID with `run --id <sandbox-id> --reclaim` or
  `stop --id <sandbox-id> --reclaim`. Conflicting claims fail closed.

Related docs:

- [Provider backends](../provider-backends.md)
- [Provider live smoke](../features/provider-live-smoke.md)
