# E2B

Read when:

- choosing `provider: e2b`;
- reviewing delegated sandbox execution behavior;
- changing E2B provider sync, status, or command streaming.

`provider: e2b` delegates Linux sandbox lifecycle and command execution to
[E2B](https://e2b.app). Crabbox creates an E2B sandbox from a template, tags it
with Crabbox metadata, uploads the local Git-managed working set as a gzipped
archive, and streams remote command output back through E2B's process API. There
is no Crabbox-managed SSH target; the sandbox owns command transport.

E2B is a delegated-run provider: Linux-only, never brokered through the
coordinator, and run directly from the CLI.

## Auth

```sh
export E2B_API_KEY=e2b_...
```

`provider: e2b` fails fast if no API key is configured. Optional endpoint
overrides:

```sh
export E2B_API_URL=https://api.e2b.app
export E2B_DOMAIN=e2b.app
```

Each setting also reads a `CRABBOX_`-prefixed variant
(`CRABBOX_E2B_API_KEY`, `CRABBOX_E2B_API_URL`, `CRABBOX_E2B_DOMAIN`,
`CRABBOX_E2B_TEMPLATE`, `CRABBOX_E2B_WORKDIR`, `CRABBOX_E2B_USER`), which takes
precedence over the bare name.

## Config

```yaml
provider: e2b
target: linux
e2b:
  template: base
  workdir: crabbox
  user: ""
```

Relative `e2b.workdir` values resolve inside the selected sandbox user's home.
The default user home is `/home/user`; `user: ubuntu` resolves under
`/home/ubuntu`, and `user: root` resolves under `/root`. Absolute workdirs are
used as-is. `e2b.user` must be a login name, not a path; values such as `..` or
`team/dev` are rejected before any sandbox or process call.

Equivalent one-off flags:

```sh
crabbox warmup --provider e2b --e2b-template base
crabbox run --provider e2b --e2b-workdir repo -- pnpm test
crabbox status --provider e2b --id <slug>
crabbox stop --provider e2b <slug>
```

Available flags: `--e2b-template`, `--e2b-workdir`, `--e2b-user`,
`--e2b-api-url`, and `--e2b-domain`. `--class` and `--type` are rejected for
`provider: e2b`; sandbox sizing comes from the chosen template.

## Behavior

- `warmup` creates an E2B sandbox from `e2b.template` (default `base`), stores
  Crabbox metadata on the sandbox, and records a local `cbx_...` lease claim. The
  sandbox is kept until an explicit `stop` regardless of `--keep`.
- `run` creates or reuses a sandbox, syncs the manifest into the resolved
  workspace path, streams stdout/stderr, and returns the remote exit code.
  Sandboxes always have internet access enabled.
- The sandbox timeout is derived from `--ttl`: unset defaults to 5 minutes and
  any value is capped at 1 hour.
- Commands run under `/bin/bash -l -c`. When `e2b.user` is set, both file uploads
  and the command run as that user.
- `--keep-on-failure` keeps a newly created failed sandbox until its timeout
  instead of deleting it immediately.
- `list`, `status`, and `stop` operate only on Crabbox-owned E2B sandboxes
  (those tagged with the Crabbox provider metadata).

A sandbox not created through a Crabbox claim can still be addressed by its E2B
sandbox id using the synthetic lease form `e2b_<sandbox-id>`, provided it carries
Crabbox metadata.

### Workspace safety

Workdirs must resolve to a dedicated absolute directory. Broad roots such as
`/`, `/home`, `/tmp`, `/usr`, `/var`, and other top-level system paths are
rejected before sandbox creation or sync touches the filesystem.

### Unsupported run options

Because E2B owns sync and command transport, these `run` options are rejected:

- `--checksum`, `--sync-only`, `--force-sync-large`, and `--full-resync` — E2B
  delegates sync (archive upload only).
- `--script`, `--script-stdin`, `--fresh-pr`, `--capture-stdout`,
  `--capture-stderr`, `--capture-on-fail`, `--download`, `--artifact-glob`,
  `--env-helper`, `--emit-proof`, and `--stop-after` — E2B delegates run
  execution.

Large-sync preflight guardrails do not apply: without `--force-sync-large`
support, the archive is uploaded as built.

## Limitations

E2B is not an SSH lease backend. Commands that require a Crabbox-managed SSH
target — `ssh`, `vnc`, `code`, and Actions runner hydration — are not available.
For those, use a provisioning provider such as
[Hetzner](hetzner.md), [AWS](aws.md), [static SSH](../providers/ssh.md), or
[Daytona](daytona.md).
