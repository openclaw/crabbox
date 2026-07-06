# Unikraft Cloud Provider

Read when:

- choosing `provider: unikraft-cloud`;
- creating or inspecting a Unikraft Cloud instance from an OCI image;
- changing `internal/providers/unikraftcloud`.

[Unikraft Cloud](https://unikraft.com) runs OCI images as lightweight cloud
microVM services. Crabbox models it as a `service-control` provider: it can
create a service instance with `warmup`, inspect instances with `status` and
`list`, and delete Crabbox-owned instances with `stop`. It does not expose a
generic command execution or SSH surface, so `run` rejects arbitrary commands.

## When To Use

Use Unikraft Cloud when the workload is already packaged as an OCI image with
the desired entrypoint. Crabbox can launch that image and track the instance
through local claims, but the image itself owns the process that runs inside the
microVM.

Do not use this provider for ad-hoc repository test commands. For generic
`crabbox run -- <command>` workflows, choose a delegated-run provider such as
`e2b`, `modal`, or `docker-sandbox`, or an SSH-lease provider such as `aws`,
`hetzner`, or `ssh`.

## Commands

```sh
export UKC_TOKEN=...

crabbox warmup --provider unikraft-cloud \
  --unikraft-cloud-metro fra \
  --unikraft-cloud-image ghcr.io/example-org/my-app:latest \
  --slug ukc-smoke

crabbox status --provider unikraft-cloud --id ukc-smoke --wait
crabbox list --provider unikraft-cloud
crabbox stop --provider unikraft-cloud ukc-smoke
```

`warmup` creates an instance from the configured OCI image and records a local
claim scoped to the Unikraft Cloud API endpoint. `stop` requires that local
claim, then stops and deletes the instance. Unclaimed instances are visible in
`list` and can be inspected by raw instance ID with `status`, but Crabbox will
not delete them.

`run` is rejected before provider mutation because Unikraft Cloud executes the
image entrypoint, not an arbitrary Crabbox command.

## Auth

```sh
export UKC_TOKEN=...
```

Crabbox resolves the API key from `CRABBOX_UNIKRAFT_CLOUD_API_KEY`,
`UNIKRAFT_CLOUD_API_KEY`, `UKC_API_KEY`, or `UKC_TOKEN`. The key may also live in
trusted user config as `unikraftCloud.apiKey`. The provider does not register an
API key flag, so the key is never passed on the command line.

Requests use `Authorization: Bearer <token>` against
`https://api.<metro>.unikraft.cloud` unless `--unikraft-cloud-url` or
`CRABBOX_UNIKRAFT_CLOUD_API_URL` overrides the API URL. Public API URLs must use
HTTPS; plain HTTP is accepted only for loopback test endpoints. Userinfo, query
parameters, and fragments are rejected.

## Config

```yaml
provider: unikraft-cloud
target: linux
unikraftCloud:
  metro: fra
  image: ghcr.io/example-org/my-app:latest
  memoryMB: 256
```

Provider flags:

```text
--unikraft-cloud-url
--unikraft-cloud-metro
--unikraft-cloud-image
--unikraft-cloud-memory
```

Environment overrides:

```text
CRABBOX_UNIKRAFT_CLOUD_API_KEY / UNIKRAFT_CLOUD_API_KEY / UKC_API_KEY / UKC_TOKEN
CRABBOX_UNIKRAFT_CLOUD_API_URL / UNIKRAFT_CLOUD_API_URL
CRABBOX_UNIKRAFT_CLOUD_METRO / UNIKRAFT_CLOUD_METRO / UKC_METRO
CRABBOX_UNIKRAFT_CLOUD_IMAGE / UNIKRAFT_CLOUD_IMAGE
```

Default metro is `fra`. An image is required for `warmup` because Crabbox does
not choose a default application image.

## Capabilities

- Target: Linux only.
- SSH: no.
- Crabbox sync: no.
- Provider sync: no; package and publish an OCI image before using `warmup`.
- Generic `run`: no.
- Warmup: yes, creates a claimed Unikraft Cloud instance from an OCI image.
- Stop/delete: yes, for Crabbox-claimed instances only.
- Desktop/browser/code: no.
- Coordinator: no (direct from CLI only).

## Gotchas

- `--class` and `--type` are rejected; use `--unikraft-cloud-memory` for the
  provider-exposed sizing knob.
- `--actions-runner` and Tailscale options are rejected because the provider has
  no SSH lease or Crabbox-managed runtime setup.
- `status --wait` polls until the instance reports `running`.
- Local claims are scoped to the API endpoint, so a claim created for one metro
  cannot be used to stop an instance in another metro.
- If claim creation fails after the remote create succeeds, Crabbox attempts to
  delete the newly created instance before returning the original error.

Related docs:

- [Provider backends](../provider-backends.md)
