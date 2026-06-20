# prewarm

`crabbox prewarm` leases a reusable box and prepares it for test runs. For
SSH-backed providers it runs the same configured GitHub Actions hydration that
`crabbox run` would otherwise do on first use; for delegated providers such as
Blacksmith Testbox, hydration stays provider-owned.

```sh
crabbox prewarm
crabbox prewarm --provider azure --probe-command 'node -v && pnpm -v'
crabbox prewarm --pool example/app/main/aws/linux/c6i.2xlarge
crabbox prewarm --dry-run
```

The command prints the lease id and keeps the box running. Stop it with
`crabbox stop <id>` when the test burst is done.

If Actions hydration, the optional probe, or ready-pool registration fails,
`prewarm` automatically releases a newly created SSH lease. If automatic
release cannot resolve or stop the lease, the error output prints the exact
`crabbox stop` command to run; delegated-run providers retain their
provider-owned lifecycle behavior.

## Reuse

Run follow-up commands against the printed lease id or slug:

```sh
crabbox run --id <id-or-slug> --no-sync -- pnpm test
crabbox run --id <id-or-slug> -- pnpm test
```

Use `--no-sync` when the prewarmed checkout already contains the code you want
to test. Omit it when local edits must be copied; fingerprint sync should skip
the upload quickly when nothing changed.

## Behavior

- Creates a fresh lease with the normal `warmup` flags.
- Runs `actions hydrate` when `actions.workflow` is configured and the provider
  is SSH-backed.
- Skips hydration for delegated-run providers and reports
  `hydration=provider-owned`.
- Optionally runs `--probe-command` without source sync to prove the hydrated
  runtime is usable.
- Optionally registers the hydrated lease in a broker ready pool with `--pool`.
- `--timing-json` includes `hydrateMs` and `probeMs`.

## Flags

Common lease flags such as `--provider`, `--target`, `--class`, `--type`,
`--market`, `--ttl`, `--idle-timeout`, `--slug`, and `--cache-volume` work the
same way they do on `warmup`.

```text
--no-hydrate                 skip Actions hydration
--github-runner              hydrate with a GitHub self-hosted runner
--repo owner/name            GitHub repository for hydration
--workflow <file|name|id>    hydration workflow
--job <name>                 expected hydration job
--ref <ref>                  workflow ref
--wait-timeout <duration>    hydration wait timeout
--keep-alive-minutes <n>     GitHub-runner keep-alive window
--probe-command <command>    shell probe to run after hydration
--pool <key>                 register the hydrated lease in a broker ready pool
--dry-run                    print planned commands
--timing-json                print machine-readable timing
```

## See Also

- [warmup](warmup.md)
- [actions](actions.md)
- [job](job.md)
- [Broker ready pools](../spec/broker.md)
