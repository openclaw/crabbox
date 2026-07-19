# GitHub Codespaces Provider

Read this when you are:

- choosing `provider: github-codespaces`;
- validating a direct GitHub Codespaces SSH lease;
- changing `internal/providers/githubcodespaces` or the guarded live smoke.

GitHub Codespaces is a Linux-only **SSH lease** provider. Crabbox creates a
Codespace from a GitHub repository, asks `gh codespace ssh --config` for the
OpenSSH connection details, stores that generated SSH config in Crabbox state,
and then uses the normal Crabbox SSH sync, `run`, `ssh`, `status`, and
`stop` paths.

The provider is **direct-only** in this release. It never routes through the
coordinator, so the local CLI must have GitHub CLI authentication and the
operator owns quota, billing, retention, and cleanup.

## When To Use It

Use GitHub Codespaces when the desired execution surface is a repository-backed
Codespace and the project already has an SSH-enabled Linux devcontainer. Prefer
AWS, Azure, GCP, Hetzner, Linode, or DigitalOcean when you need a plain VM,
coordinator-side credentials, broad OS support, or cloud-specific cost controls.

## Commands

```sh
crabbox doctor --provider github-codespaces --github-codespaces-repo example-org/my-app
crabbox warmup --provider github-codespaces --github-codespaces-repo example-org/my-app --type basicLinux32gb
crabbox run --provider github-codespaces --id my-app -- pnpm test
crabbox ssh --provider github-codespaces --id my-app
crabbox stop --provider github-codespaces my-app
crabbox cleanup --provider github-codespaces --dry-run
```

Aliases: `codespaces`, `gh-codespaces`.

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
GitHub Codespace name when a matching local Crabbox claim exists. Crabbox
refuses to manage an unclaimed Codespace by name.

## Requirements

- Install the GitHub CLI as `gh`, or point Crabbox at it with
  `githubCodespaces.ghPath`, `CRABBOX_GITHUB_CODESPACES_GH_PATH`, or
  `--github-codespaces-gh-path`.
- Authenticate `gh` with an account that can create Codespaces for the selected
  repository:

  ```sh
  gh auth login
  gh auth status
  ```

- Ensure `GH_TOKEN`, `GITHUB_TOKEN`, or the `gh` credential store has a token
  with access to Codespaces and the selected repository on GitHub.com or
  GHE.com. GitHub Enterprise Server hosts use `GH_ENTERPRISE_TOKEN`,
  `GITHUB_ENTERPRISE_TOKEN`, or the host-specific `gh` credential store; the
  other token family is stripped from each `gh` command. For local GitHub.com
  auth, refresh the missing OAuth
  scope before live smoke:
  ```sh
  gh auth refresh -h github.com -s codespace
  gh codespace list --limit 1
  ```
- Configure the repository with an SSH-enabled Linux devcontainer. The image
  must run an SSH server and include Git, `rsync`, and `tar`. A common
  devcontainer feature is `ghcr.io/devcontainers/features/sshd:1`.
- Keep local OpenSSH and `rsync` available for Crabbox's data plane.

The provider asks GitHub for an OpenSSH config rather than shelling through
`gh codespace ssh -- <command>`. That keeps the normal Crabbox sync/run/ssh
behavior intact, including `rsync -e "ssh -F ..."`.

## Configuration

Use the full example in trusted user config or an explicitly selected
`CRABBOX_CONFIG` file. Repository-local config cannot change the repository,
GitHub API or CLI routing, or retention and release deletion policy.

```yaml
provider: github-codespaces
target: linux
githubCodespaces:
  repo: example-org/my-app
  ref: main
  machine: basicLinux32gb
  devcontainerPath: .devcontainer/devcontainer.json
  workingDirectory: /workspaces/my-app
  geo: ""
  idleTimeout: 30m
  retentionPeriod: 168h
  deleteOnRelease: true
  ghPath: gh
  workRoot: /workspaces/my-app
```

Config keys under `githubCodespaces:`:

| Key | Default | Notes |
| --- | --- | --- |
| `apiUrl` | `https://api.github.com` | Trusted config only; HTTPS is required except for loopback testing. The API host scopes auth, identity checks, and mutations; standard `api.SUBDOMAIN.ghe.com` endpoints map to the GitHub CLI host `SUBDOMAIN.ghe.com`. |
| `ghPath` | `gh` | Trusted config only; local GitHub CLI executable. |
| `repo` | inferred from the GitHub remote when possible | Repository in `owner/name` form. Trusted config, environment, or CLI flag only; repo-local config cannot redirect Codespaces creation. Required when no GitHub remote can be inferred. |
| `ref` | empty | Git ref for new Codespaces. Empty uses GitHub's default behavior. |
| `machine` | `basicLinux32gb` | GitHub Codespaces machine slug. `--type` is an alias for this value. |
| `devcontainerPath` | empty | Optional devcontainer path for creation. |
| `workingDirectory` | empty | Optional Codespaces working directory setting. |
| `geo` | empty | Optional GitHub geographic location preference. |
| `idleTimeout` | `30m` | Trusted config, environment, or CLI only; repository-local config is ignored. Codespaces idle timeout sent to GitHub on create. |
| `retentionPeriod` | `168h` | Trusted config, environment, or CLI only; repository-local config is ignored. Codespaces retention period sent to GitHub on create. |
| `deleteOnRelease` | `true` | Trusted config, environment, or CLI only; repository-local config is ignored. Delete on `stop` unless a retained lease claim says release by stopping. |
| `workRoot` | `/workspaces/<repo>` when repo is known | Remote path Crabbox syncs to and runs from. |

Provider flags:

```text
--github-codespaces-repo
--github-codespaces-ref
--github-codespaces-machine
--github-codespaces-devcontainer-path
--github-codespaces-working-directory
--github-codespaces-geo
--github-codespaces-idle-timeout
--github-codespaces-retention-period
--github-codespaces-delete-on-release
--github-codespaces-gh-path
--github-codespaces-work-root
```

Environment overrides:

```text
CRABBOX_GITHUB_CODESPACES_API_URL
CRABBOX_GITHUB_CODESPACES_GH_PATH
CRABBOX_GITHUB_CODESPACES_REPO
CRABBOX_GITHUB_CODESPACES_REF
CRABBOX_GITHUB_CODESPACES_MACHINE
CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH
CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY
CRABBOX_GITHUB_CODESPACES_GEO
CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT
CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD
CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE
CRABBOX_GITHUB_CODESPACES_WORK_ROOT
```

Do not put GitHub tokens in Crabbox config or on command lines. Use
`GH_TOKEN`, `GITHUB_TOKEN`, or the GitHub CLI credential store.

## Lifecycle

1. Read host-scoped GitHub CLI auth, then resolve the stable authenticated user
   ID and login through the configured API base.
2. Resolve the repository from `githubCodespaces.repo`, flags, env, or the
   current GitHub remote.
3. Check available Codespaces machines for the repo/ref.
4. Generate a random recovery nonce, verify that its derived display name is
   absent from inventory, then durably persist a local recovery claim bound to
   the API endpoint, repository, GitHub user ID/login, and nonce-derived display
   name.
5. Create a Codespace with the configured machine, ref, devcontainer path,
   working directory, geo, idle timeout, retention period, and Crabbox display
   name.
6. Validate the provider-assigned Codespace ID, name, environment ID, owner ID,
   owner login, and repository; let the controller accept that raw identity;
   then bind it to the existing claim.
7. Wait for the Codespace to become available.
8. Ask `gh codespace ssh --config -c <codespace>` for OpenSSH config, store it
   under Crabbox state, select the matching target, and wait for SSH readiness.
9. Use normal Crabbox SSH and rsync behavior for `run`, `sync`, and `ssh`.
10. On `stop`, stop the claim-owned Codespace, refetch its identity and Git
    status, then delete or retain it according to the release policy.

If a create response is lost, Crabbox retains the recovery claim and reconciles
only one Codespace with the nonce-derived display name and exact repository in
the same GitHub account. Missing or duplicate matches fail closed. Retry
`crabbox stop --provider github-codespaces <lease-or-slug>` after GitHub
inventory converges. Pending claims created by older builds without a recovery
nonce are never adopted automatically; inspect the matching Codespace and claim
manually before deciding whether either should be removed.

If a retained Codespace is stopped, resolving it later starts it and waits for
availability before refreshing the generated SSH config.

## Ownership And Cleanup

GitHub Codespaces does not expose custom user labels. Crabbox therefore uses a
local claim as the ownership predicate. Release and cleanup require the claim to
match the provider, API endpoint, repository, Codespace ID/name, environment ID,
owner ID, and creating GitHub user ID/login. The display name is recovery-only
and may be renamed after creation; the Codespace ID/name, environment ID, owner
ID, and repository are the bound resource identity.

Deletion is conservative:

- Crabbox refuses to release a Codespace without a local claim.
- Crabbox stops and refetches a Codespace immediately before delete, then
  refuses deletion when GitHub reports uncommitted or unpushed changes or a
  changed resource identity.
- A successful asynchronous delete response does not remove local ownership;
  Crabbox retains the claim and SSH config until an identity-checked GET confirms
  absence.
- Cleanup serializes each claim, rereads its current expiry, and mutates only
  expired claim-owned Codespaces. A dirty Codespace is stopped and retained.
- Account switches are rejected when the API-authenticated GitHub user ID/login
  differs from the claim identity.

Use dry-run cleanup before mutation:

```sh
crabbox list --provider github-codespaces --json
crabbox cleanup --provider github-codespaces --dry-run
crabbox cleanup --provider github-codespaces
```

## SSHD And Devcontainer Contract

`gh codespace ssh --config` requires an SSH server inside the Codespace. A plain
devcontainer image that does not start `sshd` is not enough for Crabbox because
Crabbox needs direct OpenSSH and rsync access.

For a devcontainer-based smoke, include an SSH feature such as:

```json
{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "features": {
    "ghcr.io/devcontainers/features/sshd:1": {}
  }
}
```

The ready path also expects Git, `rsync`, `tar`, and a writable work root.

## Guarded Live Smoke

The repeatable live check is opt-in and local-only:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=github-codespaces \
CRABBOX_GITHUB_CODESPACES_SMOKE_REPO=example-org/my-app \
CRABBOX_GITHUB_CODESPACES_SMOKE_REF=main \
CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH=.devcontainer/devcontainer.json \
scripts/live-smoke.sh
```

Authenticate with `gh auth login` or inject `GH_TOKEN` through an approved
credential manager before running the command. Do not type a PAT in an inline
shell assignment because shell history can retain the complete command.

The script defaults to a skipped classification and does not call Crabbox unless
`CRABBOX_LIVE=1`, the provider filter selects `github-codespaces`, a smoke repo
is supplied, and GitHub credentials are explicitly available. It runs a read-only
doctor first, creates a short-lived Codespace lease, runs a command through the
normal synced Crabbox path, prints the Crabbox SSH command, releases the lease,
runs dry-run cleanup, and verifies the claim-owned inventory is empty.
Python 3 is preflighted before creation because inventory validation uses it. If
`CRABBOX_GITHUB_CODESPACES_SMOKE_REF` is omitted, the script detects the
repository's default branch. A devcontainer path is passed only when
`CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH` or
`CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH` is explicitly set; otherwise
Codespaces selects the repository default.
`scripts/live-smoke.sh` delegates this provider to the standalone
`scripts/live-github-codespaces-smoke.sh`, so the standalone script can also be
run directly when isolating Codespaces smoke failures.

Final classifications include:

```text
classification=live_github_codespaces_smoke_passed
classification=environment_blocked
classification=credential_bound
classification=quota_blocked
classification=validation_failed
classification=cleanup_failed
```

If credentials, entitlement, quota, or local `gh` auth are unavailable, report
the classification instead of treating the live smoke as a provider failure.

## Capabilities

- **OS target**: Linux only.
- **SSH**: yes, from `gh codespace ssh --config`.
- **Crabbox sync**: yes, through normal OpenSSH/rsync.
- **Coordinator**: never; direct CLI only.
- **Desktop / browser / code**: not advertised in this release.
- **Tailscale**: not advertised; GitHub's SSH path is used.
- **Cleanup**: yes, claim-owned only.

## Gotchas

- `--class` is not supported. Use `--type <machine>` or
  `--github-codespaces-machine <machine>`.
- `provider=github-codespaces` supports `target=linux` only.
- A Codespace without an SSH server fails during SSH config or readiness.
- Manual Codespaces are intentionally invisible to Crabbox unless a local
  Crabbox claim exists.
- `deleteOnRelease: true` still refuses deletion when GitHub reports uncommitted
  or unpushed work. This includes a local dirty checkout synced by Crabbox:
  automatic deletion cannot safely distinguish that known input from later
  remote-only edits. Inspect the stopped Codespace, preserve any work, then make
  an explicit deletion decision with GitHub CLI or the GitHub UI. Crabbox does
  not expose a force-delete override for this safety boundary.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
- [providers command](../commands/providers.md)
- [ssh command](../commands/ssh.md)
