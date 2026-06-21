# Semaphore

Read when:

- choosing `provider: semaphore`;
- configuring Semaphore host, project, machine, or auth;
- changing Semaphore lease creation, SSH resolution, or cleanup.

`provider: semaphore` provisions a standalone [Semaphore](https://semaphoreci.com)
CI job and hands it back to Crabbox as a plain SSH target. Semaphore owns the CI
job, project, machine image, and API auth; Crabbox owns local config, the lease
slug and repo claim, repo sync, remote command execution, and normalized
`list`/`status` output. The integration uses Semaphore's REST API directly — no
`sem` agent or self-hosted runner is involved.

## How a lease works

1. `warmup`/`run` resolve the configured project name to a project ID, then
   create a standalone job named `crabbox testbox`. The job runs a keepalive
   command that prepares the work root, prints a ready marker, and sleeps for the
   idle-timeout duration.
2. Crabbox polls the job until it reaches `RUNNING` and exposes an agent IP plus
   an `ssh` port, then fetches the job's debug SSH key.
3. The SSH key is written under the lease's testbox key directory and the lease
   is exposed as SSH user `semaphore` at the agent IP/port.

Lease IDs are `sem_<job-id>`. Crabbox only manages jobs whose name begins with
`crabbox testbox`, so `list`/`status`/`stop` ignore unrelated Semaphore jobs.

## Auth

Provide the host, project, and an API token. Prefer environment variables or
user config; do not commit tokens to repo config.

```sh
export CRABBOX_SEMAPHORE_HOST=example-org.semaphoreci.com
export CRABBOX_SEMAPHORE_PROJECT=my-app
export CRABBOX_SEMAPHORE_TOKEN=...
```

Semaphore-native variable names are also accepted as fallbacks:

```sh
export SEMAPHORE_HOST=example-org.semaphoreci.com
export SEMAPHORE_PROJECT=my-app
export SEMAPHORE_API_TOKEN=...
```

Create an API token at `https://<host>/me/api-tokens`. The host must be a bare
host name (for example `example-org.semaphoreci.com`), not a full API URL.

## Config

```yaml
provider: semaphore
target: linux
semaphore:
  host: example-org.semaphoreci.com
  project: my-app
  machine: f1-standard-2
  osImage: ubuntu2204
  idleTimeout: 30m
```

Defaults when unset: `machine: f1-standard-2`, `osImage: ubuntu2204`,
`idleTimeout: 30m`. `host`, `project`, and `token` are required.

| Setting     | Config key           | Flag                       | Env (and fallback)                                |
|-------------|----------------------|----------------------------|---------------------------------------------------|
| Host        | `semaphore.host`     | `--semaphore-host`         | `CRABBOX_SEMAPHORE_HOST` (`SEMAPHORE_HOST`)        |
| Project     | `semaphore.project`  | `--semaphore-project`      | `CRABBOX_SEMAPHORE_PROJECT` (`SEMAPHORE_PROJECT`)  |
| Token       | `semaphore.token`    | —                          | `CRABBOX_SEMAPHORE_TOKEN` (`SEMAPHORE_API_TOKEN`)  |
| Machine     | `semaphore.machine`  | `--semaphore-machine`      | `CRABBOX_SEMAPHORE_MACHINE`                        |
| OS image    | `semaphore.osImage`  | `--semaphore-os-image`     | `CRABBOX_SEMAPHORE_OS_IMAGE`                       |
| Idle timeout| `semaphore.idleTimeout` | `--semaphore-idle-timeout` | `CRABBOX_SEMAPHORE_IDLE_TIMEOUT`              |

The token has no flag; pass it via env or config only. Semaphore API error
bodies redact the configured token before they reach CLI diagnostics.

Equivalent one-off invocations:

```sh
crabbox warmup --provider semaphore --semaphore-host example-org.semaphoreci.com --semaphore-project my-app
crabbox run --provider semaphore --semaphore-machine f1-standard-4 -- pnpm test
crabbox ssh --provider semaphore --id <slug>
crabbox status --provider semaphore --id <slug>
crabbox stop --provider semaphore <slug>
```

## Behavior

- `warmup` creates a job, waits until it is reachable over SSH, and records a
  local Crabbox claim.
- `run` acquires (or resolves) a job, syncs the current Git manifest over SSH,
  and runs the command through Crabbox's standard SSH executor.
- `ssh` prints the SSH command for the resolved job.
- `status`/`list` report Crabbox-managed running jobs, mapped to local claims
  where available.
- `stop` posts the Semaphore job-stop request, then removes the local claim and
  stored SSH key.
- The idle timeout is enforced by the job's keepalive `sleep`, not by Crabbox;
  `Touch`/heartbeat is a no-op for this provider.

## Boundaries

- Linux only.
- No Crabbox coordinator (broker); Semaphore API auth is local/provider-native.
- No VNC, desktop, browser, code-server, or Actions hydration.
- `--class` does not map cleanly to Semaphore machines — set the machine with
  `--semaphore-machine` or `semaphore.machine`.
- `--type` is ignored; use the Semaphore-specific machine field instead.
- `--checksum` works, since Semaphore exposes a real SSH target.

## Troubleshooting

- `semaphore provider requires semaphore.host ... and semaphore.token`: set the
  host and token via config or the env vars above.
- `semaphore.project is required`: set `semaphore.project`,
  `--semaphore-project`, or `CRABBOX_SEMAPHORE_PROJECT`.
- `semaphore host ... must be a host name, not an API URL`: pass the bare host,
  not a `https://.../api/...` URL.
- `project "..." not found`: the project name does not match any project the
  token can access.
- `returned 401`: the token is wrong for the host, expired, or lacks access to
  the project.
- `job ... did not reach RUNNING state within timeout` / no SSH endpoint: the
  Semaphore agent has not attached debug SSH metadata yet; retry once capacity
  is available.
- `invalid semaphore idle timeout`: use Go duration syntax such as `30m`, `1h`,
  or `90m`.

Related docs:

- [Provider: Semaphore](../providers/semaphore.md)
- [Providers](providers.md)
- [Provider backends](../provider-backends.md)
