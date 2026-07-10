# Unikraft Cloud Provider

Read when:

- choosing `provider: unikraft-cloud`;
- creating or inspecting a Unikraft Cloud instance from an OCI image;
- recovering a pending create or deletion;
- changing `internal/providers/unikraftcloud`.

[Unikraft Cloud](https://unikraft.com) runs OCI images as lightweight cloud
microVM services. Crabbox models it as a `service-control` provider: it can
create a service instance with `warmup`, inspect instances with `status` and
`list`, and delete Crabbox-owned instances with `stop` or `cleanup`. It does not
expose a generic command execution or SSH surface, so `run` rejects arbitrary
commands.

## When To Use

Use Unikraft Cloud when the workload is already packaged as an OCI image with
the desired entrypoint. Crabbox can launch that image and track the instance
through a durable local claim, but the image itself owns the process that runs
inside the microVM.

Do not use this provider for ad-hoc repository test commands. For generic
`crabbox run -- <command>` workflows, choose a delegated-run provider such as
`e2b`, `modal`, or `docker-sandbox`, or an SSH-lease provider such as `aws`,
`hetzner`, or `ssh`.

## Commands

```sh
export UKC_TOKEN=...

crabbox doctor --provider unikraft-cloud --json

crabbox warmup --provider unikraft-cloud \
  --unikraft-cloud-metro fra \
  --unikraft-cloud-image ghcr.io/example-org/my-app:latest \
  --slug ukc-smoke

crabbox status --provider unikraft-cloud --id ukc-smoke --wait
crabbox list --provider unikraft-cloud
crabbox list --provider unikraft-cloud --all
crabbox stop --provider unikraft-cloud ukc-smoke
crabbox cleanup --provider unikraft-cloud --dry-run
```

`run` is rejected before provider mutation because Unikraft Cloud executes the
image entrypoint, not an arbitrary Crabbox command.

## Auth And Account Identity

```sh
export UKC_TOKEN=...
```

Crabbox resolves the API key from `CRABBOX_UNIKRAFT_CLOUD_API_KEY`,
`UNIKRAFT_CLOUD_API_KEY`, `UKC_API_KEY`, or `UKC_TOKEN`. The key may also live in
trusted user config as `unikraftCloud.apiKey`. The provider does not register an
API key flag, so the key is never passed on the command line.

Before listing or mutating instances, Crabbox validates the token through the
Unikraft Cloud quotas endpoint and requires one consistent account UUID. Local
claims are scoped to both the normalized API endpoint and that account UUID. A
rotated token for the same account can continue using its claims; a token for a
different account cannot inspect them through the default list or use them for
`stop` or `cleanup`.

Requests use `Authorization: Bearer <token>` against
`https://api.<metro>.unikraft.cloud` unless `--unikraft-cloud-url` or
`CRABBOX_UNIKRAFT_CLOUD_API_URL` overrides the API URL. An override must identify
the endpoint root: paths, userinfo, query parameters, and fragments are
rejected. Public API URLs must use HTTPS; plain HTTP is accepted only for
loopback development endpoints. Authenticated redirects remain restricted to
the same origin and trusted `/v1/` API path, and mutation redirects must preserve
the HTTP method.

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

## Lifecycle And Recovery

1. `warmup` allocates a random `ukc_...` lease ID, a collision-safe friendly
   slug, and a unique provider resource name.
2. Before the create request, Crabbox durably records a `create-intent` claim
   containing the endpoint/account scope, resource name, request hash, and local
   ownership labels. If that claim cannot be persisted, no instance is created.
3. A successful create binds the exact returned instance UUID and resource name
   to the claim and changes it to `ready`.
4. If the create response is lost or ambiguous, Crabbox keeps the intent and
   searches complete inventory for the exact generated name. Exactly one match
   is adopted; zero or multiple matches leave the recovery claim in place rather
   than issuing another create. A definite rejection removes the intent only
   after absence is proven.
5. `list`, `status`, `stop`, and `cleanup` can resume reconciliation of a pending
   intent. Per-lease operation locks prevent concurrent processes from creating,
   adopting, or deleting the same lease at once.

The default `list` is claim-first. It shows only claims in the current
endpoint/account scope, including pending intents and claimed instances that are
currently missing from inventory. Pass `--all` to add unclaimed instances from
the complete account inventory. `status` may inspect an unclaimed raw instance
UUID, but mutation remains claim-only.

## Deletion And Cleanup Safety

`stop` accepts only a local lease ID or slug whose endpoint/account scope,
resource name, and instance UUID still match. Before calling the delete API,
Crabbox persists a `delete-attempt` marker. It accepts the request only when the
API returns one exact matching item with explicit success, then persists
`delete-accepted`.

Wire deletion uses a one-item, UUID-only batch request. Crabbox never sends a
user-supplied instance name to the mutation endpoint.

Deletion acceptance is not treated as proof that the resource is gone. Crabbox
removes the local claim only after two exact lookups report not found with a
complete inventory lookup between them that omits both the UUID and generated
name. If confirmation is interrupted or ambiguous, the claim stays available
for recovery. Retrying an accepted deletion does not send another delete; it
resumes absence confirmation.

`cleanup --provider unikraft-cloud` also starts from current-scope local claims
and never deletes unclaimed inventory. It honors normal `keep` and expiry rules
for ready or pending claims, and it resumes `delete-attempt` and
`delete-accepted` claims regardless of `keep` or expiry once deletion has
started. A pending create is first reconciled by exact generated name, so
cleanup either adopts and deletes the one matching instance or proves that no
instance exists before removing the intent. Use `--dry-run` to inspect eligible
claims without mutation.

## Capabilities

- Target: Linux only.
- SSH: no.
- Crabbox sync: no.
- Provider sync: no; package and publish an OCI image before using `warmup`.
- Generic `run`: no.
- Warmup: yes, creates a claimed Unikraft Cloud instance from an OCI image.
- Stop/delete: yes, for Crabbox-claimed instances only.
- Cleanup: yes, local-claim and account-scope based.
- Desktop/browser/code: no.
- Coordinator: no (direct from CLI only).

## Live Smoke

The dedicated live proof is opt-in and starts with read-only account checks. It
builds the current checkout unless `CRABBOX_BIN` points to a binary:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
scripts/live-unikraft-cloud-smoke.sh

CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=unikraft-cloud \
scripts/live-unikraft-cloud-smoke.sh
```

Live mutation requires both `CRABBOX_LIVE=1` and
`CRABBOX_LIVE_PROVIDERS` containing `unikraft-cloud`, `ukc`, or `all`, plus a
token in one of the standard provider variables. Without both mutation gates,
the script does not create or delete an instance.

Optional live-smoke overrides:

```text
CRABBOX_BIN
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_IMAGE
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_MEMORY_MB
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_WAIT_TIMEOUT
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_UNCERTAINTY_SECONDS
CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS
```

The image falls back to the normal provider image variables and then
`nginx:latest`; memory defaults to 256 MB and the wait timeout to 300 seconds.
Metro and API URL selection use the normal provider variables, with `fra` as the
default metro. Ambiguous-create cleanup observes a 35-second visibility window;
emergency cleanup has a bounded 90-second budget. A custom proof-directory base
must be owner-only; each run creates its own private child directory beneath it.

The live run isolates Crabbox claim state, records the baseline account
inventory, creates one uniquely named claimed instance, proves status and both
list modes, deletes it through `stop`, and requires the final UUID set to match
the baseline. Cleanup is armed before creation. Credential values stay in the
environment rather than argv, and proof output is written under a private
directory with secret and endpoint redaction.

## Gotchas

- `--class` and `--type` are rejected; use `--unikraft-cloud-memory` for the
  provider-exposed sizing knob.
- `--actions-runner` and Tailscale options are rejected because the provider has
  no SSH lease or Crabbox-managed runtime setup.
- `status --wait` polls until the instance reports `running` and fails early on
  terminal states such as `failed`, `error`, or `stopped`.
- A local claim from another API endpoint or account is intentionally not
  reusable, even if its slug matches.
- Do not delete recovery claims by hand. They are the durable ownership record
  used to reconcile ambiguous creates and accepted deletions safely.

Related docs:

- [Provider backends](../provider-backends.md)
- [Provider live smoke](../features/provider-live-smoke.md)
