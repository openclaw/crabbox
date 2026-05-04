# islo

Read when:

- choosing `provider: islo`;
- changing islo CLI forwarding;
- deciding what Crabbox owns versus islo owns.

Crabbox can use [islo.dev](https://islo.dev) sandboxes as the machine backend without using the Crabbox broker. Select it with `--provider islo` for one command, or put `provider: islo` in config when a repo or machine should use it by default.

## One-Liners

If you already have an islo sandbox, no Crabbox YAML is required:

```sh
crabbox run --provider islo --id my-sandbox -- pnpm test
```

If Crabbox has already claimed a friendly slug for that sandbox, the slug works too:

```sh
crabbox run --provider islo --id blue-lobster -- pnpm test:changed
crabbox status --provider islo --id blue-lobster
crabbox stop --provider islo blue-lobster
```

That path only needs islo auth and a reachable sandbox. Crabbox resolves the name or slug, preserves the local repo claim, forwards the command to `islo use`, and prints `sync=delegated` in the final summary.

To create a fresh sandbox without YAML, provide the image and source as flags:

```sh
crabbox warmup \
  --provider islo \
  --islo-image docker.io/library/ubuntu:24.04 \
  --islo-source github://openclaw/crabbox:main \
  --idle-timeout 90m
```

The same flags work for one-shot `run` when no `--id` is supplied:

```sh
crabbox run \
  --provider islo \
  --islo-image docker.io/library/ubuntu:24.04 \
  -- pnpm test
```

YAML is a convenience, not a requirement, when the command line already tells Crabbox which backend and image to use. Environment variables such as `CRABBOX_ISLO_IMAGE`, `CRABBOX_ISLO_SOURCE`, `CRABBOX_ISLO_WORKDIR`, `CRABBOX_ISLO_GATEWAY_PROFILE`, `CRABBOX_ISLO_SESSION`, and `CRABBOX_ISLO_ORG` are also supported for shell defaults or scripts.

## Repo Config

Use repo config when every agent or maintainer should get the same islo defaults without repeating flags:

```yaml
provider: islo
islo:
  image: docker.io/library/ubuntu:24.04
  source: github://openclaw/crabbox
  workdir: /workspace/crabbox
  gatewayProfile: default
  session: main
  idleTimeout: 90m
```

`islo-sandbox` is accepted as a long-form provider alias, but docs and scripts should prefer `islo`.

## Forwarded Commands

Crabbox forwards machine operations to the islo CLI:

```sh
islo use <name> [--image <image>] [--source <repo>] [--workdir <dir>] [--gateway-profile <profile>] [--session <session>] -- true
islo use <name> [--image <image>] ... -- <command>
islo status <name> [-o json]
islo ls [-o json]
islo rm <name> --force
```

The wrapper is deliberately thin. If islo adds behavior to those commands, Crabbox should prefer forwarding rather than reimplementing it.

`crabbox list --provider islo --json` consumes islo's native `--output json` rather than parsing tables. If islo's JSON shape changes, the parser in `internal/cli/islo.go:parseIsloListJSON` is the single update site.

## Auth

Auth stays with islo. Run `islo login` before using this provider. Crabbox does not call the Crabbox login broker, does not send work to the Cloudflare coordinator, and does not hold islo credentials.

## Ownership Boundary

- islo owns provisioning, sandbox image setup, repo cloning via `--source`, command transport, logs emitted by its CLI, gateway/network policy, SSH cert provisioning, and idle expiry.
- Crabbox owns local YAML/env config, friendly slugs, repo claims, provider selection, command quoting, and final timing summaries.

Because islo owns sync in this mode, Crabbox sync flags such as `--sync-only`, `--checksum`, `--force-sync-large`, and sync guardrails do not apply. `crabbox run` prints `sync=delegated` in the final summary.

`islo.image` is required only when Crabbox needs to warm or acquire a sandbox. Reusing an existing sandbox name or slug does not need image config.

## Choosing The Path

Use the one-liner when:

- you already have a sandbox name;
- you are trying islo on one command;
- an agent can pass provider and image directly as flags.

Use repo YAML when:

- the repo should default to islo;
- multiple agents should share the same image/source/session;
- you want `crabbox warmup` to work without extra env.

Related docs:

- [Providers](providers.md)
- [run command](../commands/run.md)
- [warmup command](../commands/warmup.md)
- [Source map](../source-map.md)
