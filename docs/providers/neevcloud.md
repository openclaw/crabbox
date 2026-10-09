# NeevCloud Provider

Read this when:
- choosing `provider: neevcloud`;
- configuring the NeevCloud API endpoint, organization, project, template, workdir, or internet access;
- changing `internal/providers/neevcloud`.

NeevCloud provides isolated Linux sandboxes through the NeevCloud AI
platform API. It is a **delegated-run** provider: Crabbox calls the NeevCloud
lifecycle API to create, find and delete sandboxes, uploads a portable archive
to the sandbox runtime, and runs the command through its streaming exec API.
There is no direct Crabbox SSH target and no local rsync.

Crabbox owns local config, repo claims, slug allocation, archive sync
guardrails, lease labels, run timing summaries, and normalized `list`/`status`
output. NeevCloud owns sandbox state, file upload, command transport, egress
policy, and sandbox deletion.

## When To Use

Use NeevCloud when commands should run in a hosted, isolated Linux sandbox and
archive sync plus delegated exec is enough: test runs, build checks, and
agent-style command execution.

Use an SSH-lease provider when you need `crabbox ssh`, VNC, code-server,
Actions hydration, direct rsync, or desktop/browser/code capability flags.
NeevCloud is Linux-only; desktop, browser, code, VNC, SSH, Tailscale, and
Actions runner hydration are not available through Crabbox.

## Setup

Create an API key in the NeevCloud console for the project that should own the
sandboxes, and note its organization and project IDs.

## Auth

```sh
export CRABBOX_NEEVCLOUD_API_KEY=sk-nc-...
# or
export NEEV_API_KEY=sk-nc-...
```

`CRABBOX_NEEVCLOUD_API_KEY` takes precedence when both are set. Crabbox reads
the key only from the environment; it is never accepted as a flag and never
stored in config or claims. Lifecycle requests send it as
`Authorization: Bearer`; sandbox runtime requests send it as `X-Api-Key` to
the sandbox's own connect URL only. It is never forwarded into the sandbox
environment.

Rotate the key if it was ever pasted into a chat, shell history, issue, PR,
log, or persistent artifact.

## Config

```yaml
provider: neevcloud
neevcloud:
  orgId: org-...
  projectId: prj-...
  template: sb-ubuntu-26-04-dev
  workdir: /workspace/crabbox
  timeoutSecs: 0
  allowInternet: true
```

| Key | Env | Flag | Default |
| --- | --- | --- | --- |
| `baseUrl` | `CRABBOX_NEEVCLOUD_BASE_URL` | `--neevcloud-base-url` | `https://api.ai.neevcloud.com` |
| `orgId` | `CRABBOX_NEEVCLOUD_ORG_ID` | `--neevcloud-org-id` | required |
| `projectId` | `CRABBOX_NEEVCLOUD_PROJECT_ID` | `--neevcloud-project-id` | required |
| `template` | `CRABBOX_NEEVCLOUD_TEMPLATE` | `--neevcloud-template` | `sb-ubuntu-26-04-dev` |
| `workdir` | `CRABBOX_NEEVCLOUD_WORKDIR` | `--neevcloud-workdir` | `/workspace/crabbox` |
| `timeoutSecs` | `CRABBOX_NEEVCLOUD_TIMEOUT_SECS` | `--neevcloud-timeout-secs` | `0` (use Crabbox TTL) |
| `allowInternet` | `CRABBOX_NEEVCLOUD_ALLOW_INTERNET` | `--neevcloud-allow-internet` | `true` |

`baseUrl` is trusted endpoint config: it may come from user config, the
environment, or a flag, but a repository `crabbox.yaml` cannot set it. It must
be HTTPS except for loopback development endpoints. The workdir must be a
subdirectory of `/workspace`, the only tree the sandbox runtime addresses.

`allowInternet: false` creates the sandbox with an allow-list egress policy
and no internet access.

## Commands

```sh
crabbox doctor --provider neevcloud
crabbox warmup --provider neevcloud --keep
crabbox run --provider neevcloud -- go test ./...
crabbox run --provider neevcloud --id <lease-or-slug> -- make check
crabbox list --provider neevcloud
crabbox status --provider neevcloud --id <lease-or-slug>
crabbox stop --provider neevcloud --id <lease-or-slug>
```

`doctor` is read-only. It reports `config`, `endpoint`, `auth` and `project`
checks separately, so a wrong base URL, a rejected key, and a wrong project
fail with different messages.

## Lifecycle

- **Create.** Crabbox creates a sandbox named `crabbox-<slug>-<lease>` from the
  configured template, with lease labels and a max lifetime from `timeoutSecs`
  or the Crabbox TTL. Names are unique per project, so a retried create
  collides; Crabbox adopts the existing sandbox only when its lease label
  matches.
- **Ready.** Crabbox waits for phase `Ready` and a connect URL, and fails fast
  on `RestoreFailed`.
- **Sync.** The archive is uploaded with the resumable upload protocol in
  1 MiB chunks to `/workspace/.crabbox-sync-*.tgz`, then extracted into the
  workdir.
- **Run.** The command runs as `/bin/sh -c` in the workdir and streams stdout
  and stderr. Exec is capped at one hour per command.
- **Stop.** Crabbox deletes only a sandbox bound to an exact local claim whose
  labels still name the same lease, then removes the claim.

## Labels

NeevCloud accepts at most 16 labels per sandbox, with keys of lowercase
letters, digits, `.`, `_` and `-`, and values of letters, digits, `.`, `_` and
`-` up to 63 characters. Crabbox sends an allow-listed subset of its lease
labels (`crabbox`, `provider`, `lease`, `slug`, `state`, `keep`, TTL fields,
`server_type`, and class/profile fields) and drops any value the API would
reject, such as timestamps containing `:`. List and resolve filter on these
labels server-side and re-check them locally.

## Claims

Claims are scoped by base URL, organization and project, so leases created
against one endpoint or project never resolve or delete in another.

`run --id` reuses a sandbox only through its exact local claim, whether the id
is a lease id, slug, `nvc_` id or raw sandbox id, and applies the same
repository ownership check as any other lease; `--reclaim` moves ownership.
Upload, exec and cleanup run fenced on the admitted claim, so a concurrent
reclaim stops a stale run before it touches the sandbox. A sandbox with
Crabbox labels but no local claim, such as one created on another machine, is
listed but never run or deleted.

## Limitations

- No SSH, VNC, desktop, browser, code-server, Tailscale or Actions hydration.
- Commands run as the sandbox's default user; `--user` other than root is
  rejected.
- Exec is capped at one hour per command.
- `stop --reclaim` is not supported.
