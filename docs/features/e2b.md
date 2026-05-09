# E2B

Read when:

- choosing `provider: e2b`;
- reviewing delegated sandbox execution behavior;
- changing E2B provider sync, status, or command streaming.

`provider: e2b` delegates Linux sandbox lifecycle and command execution to E2B.
Crabbox creates E2B sandboxes with Crabbox metadata, syncs the local
Git-managed working set as a gzipped archive, and streams remote command output
through E2B's process API.

## Auth

```sh
export E2B_API_KEY=e2b_...
```

Optional overrides:

```sh
export E2B_API_URL=https://api.e2b.app
export E2B_DOMAIN=e2b.app
```

## Config

```yaml
provider: e2b
target: linux
e2b:
  template: base
  workdir: crabbox
  user: ""
```

Relative `e2b.workdir` values resolve inside the selected E2B user's home. The
default user home is `/home/user`, `user: ubuntu` resolves under `/home/ubuntu`,
and `user: root` resolves under `/root`. Absolute workdirs are used as-is.

Equivalent one-off flags:

```sh
crabbox warmup --provider e2b --e2b-template base
crabbox run --provider e2b --e2b-workdir repo -- pnpm test
crabbox status --provider e2b --id <slug>
crabbox stop --provider e2b <slug>
```

## Behavior

- `warmup` creates an E2B sandbox from `e2b.template`, stores Crabbox metadata,
  and records a local `cbx_...` lease claim.
- `run` creates or reuses a sandbox, syncs the manifest into
  `<e2b user home>/<e2b.workdir>` unless the workdir is absolute, streams
  stdout/stderr, and returns the remote exit code.
- `--sync-only` performs only the archive upload and extraction.
- Workdirs must resolve to dedicated absolute directories. Broad roots such as
  `/`, `/home`, and `/tmp` are rejected before sandbox creation or sync touches
  the sandbox filesystem.
- `--checksum` is rejected because E2B does not expose a Crabbox SSH target.
- `list`, `status`, and `stop` operate only on Crabbox-owned E2B sandboxes.

E2B is not an SSH lease backend today. Commands that require a Crabbox SSH
target, such as `ssh`, `vnc`, `code`, and Actions runner hydration, should use
Hetzner, AWS, static SSH, or Daytona instead.
