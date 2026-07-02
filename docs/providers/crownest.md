# Crownest Provider

Read this when:

- choosing `provider: crownest`;
- configuring the Crownest API endpoint, project, template, timeout, or cleanup behavior;
- changing `internal/providers/crownest`.

Crownest provides hosted Linux Workspace Runs through the Crownest API. It is a
**delegated-run** provider: Crabbox builds a portable checkout archive, uploads
it through Crownest's staged archive-transfer API, starts a Workspace Run, and
streams the Crownest event stream back to the local terminal. There is no direct
Crabbox SSH target and no local rsync.

Crabbox owns local config, repo claims, slug allocation, archive sync guardrails,
command quoting, run timing summaries, and normalized `list`/`status` output.
Crownest owns sandbox creation, archive extraction, command execution, event
streaming, evidence, and sandbox deletion.

## When To Use

Use Crownest when commands should run in a hosted Linux sandbox without asking
each user to deploy their own runner. It fits dirty-checkout test runs and
coding-agent workflows where archive sync, streamed logs, exit codes, and
Crownest Workspace Run evidence are the important product surface.

Use an SSH-lease provider such as AWS, Hetzner, Azure, Google Cloud, Static SSH,
or Local Container when you need `crabbox ssh`, VNC, code-server, Actions
hydration, direct rsync behavior, ports, desktop/browser/code capability flags,
or command environment forwarding.

Crownest is Linux-only. Desktop, browser, code, VNC, SSH, Tailscale, Actions
runner hydration, artifact globs, proof emission, and downloads are not
available through this provider yet.

## Setup

Create a Crownest API key, then export it through the environment. Crabbox never
accepts the key as a command-line flag and does not persist it in `crabbox.yaml`,
`.crabbox.yaml`, or trusted user config.

## Auth

```sh
export CRABBOX_CROWNEST_API_KEY=cn_live_...
# or
export CROWNEST_API_KEY=cn_live_...
```

`CRABBOX_CROWNEST_API_KEY` takes precedence when both variables are present. The
key is sent only as a bearer token to the configured Crownest API endpoint. It is
not stored in Crabbox claims or config and is not forwarded into the Workspace
Run command environment.

Rotate the key if it was ever pasted into a chat, shell history, issue, PR, log,
or persistent artifact.

## Commands

```sh
crabbox doctor --provider crownest
crabbox warmup --provider crownest --crownest-template python-node
crabbox run --provider crownest -- pnpm test
crabbox run --provider crownest --keep -- pnpm test
crabbox run --provider crownest --id quiet-river -- pnpm test
crabbox status --provider crownest --id quiet-river
crabbox list --provider crownest --json
crabbox stop --provider crownest quiet-river
crabbox cleanup --provider crownest --dry-run
```

`warmup` always keeps the sandbox until an explicit `stop`. A `run` without
`--id` creates or starts a Crownest-backed Workspace Run and deletes the backing
sandbox after the command unless `--keep` asks Crabbox to retain it.

## Config

```yaml
provider: crownest
target: linux
crownest:
  projectId: prj_...
  template: python-node
  timeoutSecs: 600
  forgetMissing: false
```

Trusted user config may also set `crownest.apiUrl`. Repository config cannot set
the API URL, because redirecting API-key traffic from a checked-in file is not
trusted.

Provider flags, each overriding the matching config key:

```text
--crownest-url
--crownest-project-id
--crownest-template
--crownest-timeout-secs
--crownest-forget-missing
```

Environment overrides:

```text
CRABBOX_CROWNEST_API_KEY
CROWNEST_API_KEY
CRABBOX_CROWNEST_API_URL
CROWNEST_API_URL
CRABBOX_CROWNEST_PROJECT_ID
CROWNEST_PROJECT_ID
CRABBOX_CROWNEST_TEMPLATE
CROWNEST_TEMPLATE
CRABBOX_CROWNEST_TIMEOUT_SECS
CROWNEST_TIMEOUT_SECS
CRABBOX_CROWNEST_FORGET_MISSING
CROWNEST_FORGET_MISSING
```

Defaults: API URL `https://api.crownest.dev`, template `python-node`, and
command timeout `600` seconds.

The API URL must be absolute, must not include userinfo, query parameters, or a
fragment, and must use HTTPS except for loopback development endpoints.

## Lifecycle

1. `warmup` creates a Crownest sandbox from the configured template and stores a
   local Crabbox claim with a `cnsbx_...` lease ID.
2. `run` without `--id` creates a Crownest Workspace Run from the configured
   template. `run --id` reuses a Crabbox-claimed Crownest sandbox.
3. Crabbox builds a portable checkout archive and asks Crownest for a staged
   archive transfer target.
4. Crabbox uploads the archive to that transfer URL, finalizes the transfer, and
   starts the Workspace Run.
5. Crownest extracts the archive into the Workspace Run workspace and executes
   the command.
6. Crabbox streams stdout/stderr events and returns the final command exit code.
7. One-shot sandboxes are deleted after successful `run` unless `--keep` is set.
   Retained sandboxes can be reused with `--id` and later removed with `stop`.

## Capabilities

- SSH: no.
- Crabbox sync: yes, delegated archive upload and Crownest extraction.
- Provider sync: no separate provider-native sync step.
- Desktop / browser / code / VNC: no.
- Actions hydration: no.
- Coordinator broker: no, Crownest always runs direct from the CLI.
- Pause/resume: not advertised in v1.
- Command env forwarding: no. Crownest Workspace Runs currently reject env
  values until secret-backed env storage is added on the Crownest side.

## Limitations

- `--no-sync` is rejected because Crownest Workspace Runs require archive upload.
- `--sync-only`, `--checksum`, artifact globs, required artifacts, proof
  emission, and downloads are rejected unless Crownest advertises those
  capabilities in a later provider revision.
- Crownest raw sandbox IDs are not accepted as arbitrary provider IDs. Use a
  Crabbox-created slug or `cnsbx_<sandbox-id>` lease ID from `warmup`, `run
  --keep`, or `list`.
- `cleanup` only operates on local Crabbox claims for the configured Crownest API
  endpoint, project, and template. Missing remote sandboxes are forgotten only
  when `crownest.forgetMissing` or `--crownest-forget-missing` is set.
