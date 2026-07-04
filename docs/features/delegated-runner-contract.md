# Delegated Runner Contract

Read this when:

- adding a delegated-run provider that does not expose SSH;
- designing a hosted runner model, deployment, or sandbox image for Crabbox;
- deciding whether a provider PR has enough proof to merge.

Delegated-run providers are not SSH leases. Crabbox cannot assume rsync,
OpenSSH, a shell-compatible bootstrap, or direct process control. The provider
owns workspace transport and command execution, while Crabbox owns the local CLI
surface, config, claims, slugs, timing records, and normalized status output.

This page defines the minimum portable runner contract a new delegated provider
must satisfy before it should become a built-in provider.

## Why This Exists

Several hosted execution platforms can run code only through a prediction,
sandbox API, job, or deployment. A provider adapter can still fit Crabbox when
that remote side implements the same small runner shape.

Without this contract, each provider PR has to invent its own answers for:

- how the checkout reaches the runner;
- how argv, shell mode, workdir, env, and timeout are represented;
- what a successful platform run means versus a successful user command;
- how logs are streamed without duplication;
- how cancellation and cleanup work;
- what live proof is needed before merge.

Provider-specific APIs can differ. The Crabbox-facing behavior should not.

## Minimum Runtime Shape

A delegated runner must provide one command execution operation with these
semantics:

```json
{
  "archive": "data:application/gzip;base64,... or https://...",
  "command": ["npm", "test"],
  "shell": false,
  "workdir": "/workspace/crabbox",
  "env": {
    "EXAMPLE_FLAG": "1"
  },
  "timeoutSecs": 3600,
  "syncDelete": true
}
```

The provider adapter may translate this into a provider-specific request, but
the meaning must stay stable:

- `archive` contains the Crabbox sync archive or a pointer to it.
- `command` is argv when `shell=false`.
- `command` is a single shell string, usually in `command[0]`, when
  `shell=true`.
- `workdir` is an absolute, dedicated workspace directory on the runner.
- `env` contains only explicitly forwarded command environment values. Provider
  API tokens and Crabbox auth variables must be stripped.
- `timeoutSecs` is the command execution budget, not necessarily the platform
  billing timeout.
- `syncDelete=true` means the runner should remove files from the previous
  workspace contents that are absent from the new archive, if it reuses state.

If the provider cannot support one of these fields, the adapter must document
that limitation and reject the corresponding Crabbox option instead of silently
changing behavior.

## Minimum Result Shape

The runner output must distinguish platform lifecycle from command exit status:

```json
{
  "exitCode": 0,
  "stdout": "optional final stdout\n",
  "stderr": "optional final stderr\n",
  "artifacts": []
}
```

Rules:

- A provider-level `succeeded` state is not enough for Crabbox success. The
  runner result must include `exitCode`.
- `exitCode == 0` maps to a successful Crabbox command.
- `exitCode != 0` maps to the user command's exit code.
- Missing or malformed result JSON is a provider failure, not user command
  success.
- Provider `failed`, `canceled`, or timeout states must include redacted
  provider diagnostics when available.

Streaming providers should stream stdout/stderr as the command runs. Polling
providers must print only new log content on each poll; repeated complete-log
responses must be deduplicated by the adapter.

## Workspace Transport

The first supported transport is a bounded gzip archive. Provider adapters must
choose one explicit upload path:

- data URL for small workspaces;
- signed object URL owned by the operator or coordinator;
- provider-native file upload;
- runner-side fetch from a short-lived URL.

The adapter must enforce a maximum archive size before creating a paid or
stateful provider resource. If large workspaces need object storage, that is a
separate feature and should not be implied by a data-URL MVP.

Do not silently upload source code to an unspecified third-party store.

## Credentials And Env

Provider credentials are control-plane credentials. They are never runner env.

Required behavior:

- Read provider API tokens from environment or the provider's native auth store,
  not from repo YAML or argv.
- Reject or require explicit operator approval when repo config selects a
  non-default API URL while inheriting an ambient provider token.
- Do not follow cross-origin redirects with authorization headers.
- Redact tokens from HTTP errors, JSON errors, logs, docs examples, and
  recorded run output.
- Strip provider auth variables from the forwarded runner env summary and
  request payload.

Forwarded user env still follows the normal Crabbox allowlist rules. A delegated
runner must not inherit the full local process environment by default.

## Claims, Status, And Stop

A delegated provider must map provider resources into Crabbox's normal local
claim model:

- create or reuse a Crabbox lease ID and slug;
- record the provider's raw run, session, prediction, or sandbox ID in the
  claim;
- resolve `status` and `stop` by lease ID, slug, or documented raw provider ID;
- avoid listing unrelated account resources that cannot be identified as
  Crabbox-created;
- cancel or stop the provider resource on local context cancellation when the
  provider supports it.

If a provider resource is created before local claim setup fails, the adapter
must cancel that resource before returning the error.

## Unsupported SSH Features

Delegated providers must reject SSH-only run features unless they implement an
equivalent contract:

- `--fresh-pr`;
- uploaded POSIX scripts, except module-run providers that explicitly treat
  scripts as module source;
- `--full-resync` and checksum rsync;
- local stdout/stderr capture files;
- capture-on-fail bundles;
- `--download` unless `FeatureRunDownloads` is implemented;
- artifact globs unless `FeatureRunArtifacts` is implemented;
- `--emit-proof` unless `FeatureRunProof` is implemented;
- env helpers;
- `--stop-after`;
- Actions hydration;
- SSH, VNC, WebVNC, and code-server.

Use `RejectDelegatedSyncOptionsForSpec` as the default guard. Add capability
flags only when the provider really implements the corresponding behavior.

## Provider Spec

A runner-backed delegated provider should normally declare:

```go
core.ProviderSpec{
    Name:        "example",
    Family:      "example",
    Kind:        core.ProviderKindDelegatedRun,
    Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
    Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
    Coordinator: core.CoordinatorNever,
}
```

Do not advertise SSH, desktop, browser, code, Tailscale, coordinator support, or
artifact/download/proof features until those paths are implemented and tested.

## Live Proof Bar

Offline tests are necessary but not sufficient for a new hosted delegated
provider. Before merge, attach redacted proof from a guarded live run against a
compatible runner.

Minimum proof:

1. `crabbox providers --json` shows the provider metadata and truthful
   capabilities.
2. Missing-auth `doctor` or `run` fails before provider resource creation.
3. A live run creates one provider resource, uploads or stages a small
   workspace, executes a command, streams logs without duplication, maps
   `exitCode=0` to success, and records timing.
4. A live nonzero command maps to the command exit code, not generic provider
   success.
5. `status` resolves the created claim.
6. `stop` or non-kept cleanup cancels/deletes the provider resource.
7. Final `list` does not show leftover Crabbox-owned resources.
8. Logs and errors do not expose provider tokens, upload URLs with credentials,
   private file paths, or forwarded secret values.

If no live provider credentials or compatible runner deployment exist yet, keep
the provider PR unmerged and land only reusable contract, tests, or docs that do
not advertise the provider as usable.

## Review Checklist

Before reviewing a delegated provider PR, check:

- The runner input and output schema is documented.
- Success depends on runner `exitCode`, not only provider terminal state.
- Token resolution, redirect behavior, and redaction are tested.
- Provider auth env is stripped from runner payloads.
- Archive size is bounded before paid resource creation.
- Claim setup failure cancels already-created resources.
- `list` avoids unrelated account resources.
- Unsupported SSH-only flags fail with clear errors.
- Provider metadata does not overstate lifecycle, artifacts, SSH, or
  coordinator support.
- Live proof exists or the PR is intentionally not merge-ready.

## Related Docs

- [Provider backends](../provider-backends.md)
- [Authoring a provider](provider-authoring.md)
- [Environment forwarding](env-forwarding.md)
- [Artifacts](artifacts.md)
