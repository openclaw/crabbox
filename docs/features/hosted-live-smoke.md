# Bounded Hosted Live Smoke

Status: design proposal. This document does not activate a credentialed GitHub
Actions workflow.

This is the tier-two design for
https://github.com/openclaw/crabbox/issues/944. The source-discovered lifecycle
gate landed in https://github.com/openclaw/crabbox/pull/948, the secretless
hosted end-to-end tier landed in https://github.com/openclaw/crabbox/pull/981,
and provider-specific live-test documentation was expanded in
https://github.com/openclaw/crabbox/pull/979. Those tiers remain the pull-request
gates. This proposal adds periodic, authenticated create/use/destroy proof
without making funded provider access reachable from pull-request code.

## Goals And Non-Goals

The hosted tier should answer one narrow question: can the released provider
adapter create one inexpensive resource, wait for it, run a trivial command,
observe it in inventory, and prove that it was destroyed? It should also prove
that a separate reconciliation path can find and remove an abandoned smoke
resource.

This tier is not a broad provider matrix, a performance benchmark, a substitute
for hermetic lifecycle tests, or a required pull-request check. Adding a
provider requires a separate policy decision and must not happen merely because
the provider appears in the generated coverage matrix.

## Phase-One Provider Allowlist

Phase one contains exactly two providers:

| Provider | Fixed smoke shape | Why it is suitable |
| --- | --- | --- |
| `digitalocean` | One `s-1vcpu-1gb` Droplet in one configured region | The dedicated runner already checks empty owned inventory, uses the smallest documented general-purpose size, performs create/status/run/list/stop, and verifies tag-owned cleanup. DigitalOcean custom token scopes allow a dedicated team token to be narrower than an account-wide personal token. |
| `scaleway` | One `DEV1-S` Instance in one configured zone | The dedicated runner has the same empty-inventory and final-cleanup checks, uses an inexpensive development instance type, retries cleanup, and records provider-native ownership tags. A dedicated Scaleway project and IAM application give the workflow an isolated quota and credential boundary. |

Both are direct Linux SSH providers, so the rows prove the same lifecycle while
exercising independent APIs and ownership encodings. Their dedicated runners
already classify environment, quota, validation, and cleanup outcomes. This is
safer for the first unattended funded tier than selecting a provider only for a
faster theoretical boot time.

AWS is intentionally deferred. It has mature ownership tags and a read-only
orphan audit, but its account, IAM, networking, image, region, and Spot choices
make the first credential policy materially larger. In particular,
`scripts/aws-crabbox-orphan-audit.sh --terminate` currently fails closed; it is
not a destructive sweeper. Delegated sandbox providers are also deferred until
they have a dedicated hosted runner, provider-native run ownership metadata,
and a separately tested stale-resource sweeper.

The allowlist must be literal workflow configuration, reviewed like a security
boundary. It must not be generated from the provider registry or accept a
free-form provider dispatch input.

## Trust And Trigger Boundary

The future workflow must have only `schedule` and `workflow_dispatch` triggers.
It must not contain `pull_request`, `pull_request_target`, `push`,
`workflow_run`, `repository_dispatch`, or reusable `workflow_call` triggers.
The job must:

- use the GitHub environment named `live-smoke`, with every provider credential
  stored only as an environment secret;
- restrict environment deployments to the `main` branch;
- guard execution with `github.repository == 'openclaw/crabbox'` and
  `github.ref == 'refs/heads/main'`;
- check out the exact `main` commit selected by the trigger, never a dispatch
  ref, pull-request head, user-supplied SHA, or artifact containing executable
  code;
- use only SHA-pinned actions and `permissions: contents: read`;
- expose a closed dispatch choice of `digitalocean` or `scaleway`; scheduled
  runs execute both sequentially;
- set `max-parallel: 1`, `fail-fast: false`, and
  `cancel-in-progress: false`.

Only repository users who can run workflows may request a dispatch. The
checked-out ref and closed provider input still need explicit guards because a
trusted user can otherwise dispatch a workflow from a non-default branch.

GitHub environment required reviewers apply to every deployment, including
scheduled smoke and sweeper jobs; they cannot be enabled only for manual
dispatches. The recommended unattended configuration therefore uses a
main-only protected environment without required reviewers. If maintainers
enable reviewers, scheduled runs and orphan sweeps become approval queues by
design and must not be described as automatic. A custom deployment protection
rule may later distinguish event types, but it is not a phase-one dependency.

## Spend Ceilings

The initial budget is USD 0.25 per provider row, USD 0.50 per scheduled run,
and USD 20 per calendar month across both providers. A nightly two-provider
schedule has a planned maximum of USD 15.50 in a 31-day month, leaving USD 4.50
for approved manual retries.

These are admission limits, not estimates to display after spending. Enforce
them in layers:

1. Each provider gets a dedicated project or team with a provider-side quota
   of one simultaneously active compute resource and no GPU, volume, snapshot,
   load-balancer, reserved-IP, or other billable product entitlement needed by
   the smoke. The fixed size, region/zone, and image are environment variables,
   not dispatch inputs.
2. The provider project must have a hard USD 10 monthly credit or billing limit.
   A budget email is not a hard limit. If a provider cannot supply an
   enforceable cap or a prepaid/credit-only project, its scheduled row stays
   disabled until an equivalent fail-closed admission controller is reviewed.
3. Preflight validates the allowlisted size and confirms there is no existing
   live-smoke resource before creation. A row may create exactly one resource.
4. Every resource gets `--ttl 20m` and `--idle-timeout 5m`. The mutation job has
   `timeout-minutes: 25`; its cleanup job has `timeout-minutes: 10`; the entire
   workflow has one provider row active at a time.
5. A scheduled run never retries a mutation automatically. A manual retry
   selects one provider, preserving the monthly reserve and avoiding duplicate
   uncertain creates.

Provider quotas bound simultaneous exposure, TTL and timeouts bound the normal
run, and the project credit limit bounds cumulative exposure if all workflow
cleanup fails. Activation is blocked until all three limits are real. Workflow
timeouts alone are not a spend ceiling because cancellation can leave the
provider resource running.

To abort before mutation, disable the schedule or reject the environment
deployment. To abort after mutation, disable new dispatches and revoke the
provisioning credential, but retain the independent read/delete sweeper
credential until inventory is empty. Do not cancel a running mutation merely to
stop spending: allow its cleanup path to finish, then use the sweeper dispatch.

## Resource Identity And Cleanup

Cleanup is defense in depth rather than one shell trap. Before this tier is
activated, both provider adapters or their dedicated runners must put equivalent
provider-native ownership data on the resource at creation time:

```text
crabbox=true
purpose=live-smoke
repository=openclaw/crabbox
run_id=<github.run_id>
run_attempt=<github.run_attempt>
created_at=<UTC epoch>
expires_at=<UTC epoch, no later than creation plus 20 minutes>
keep=false
```

DigitalOcean's flat tags may encode these fields differently from Scaleway's
key/value tags. That translation belongs in the provider adapter or
provider-specific runner, not in provider-neutral core. The resource name or
slug should also start with `crabbox-live-smoke-`, but a name match alone never
authorizes deletion.

The cleanup pattern has three layers:

1. The runner arms an `EXIT` trap before the first create request and performs
   an idempotent targeted stop when it has an exact resource ID.
2. A separate cleanup job uses `if: ${{ always() }}` and the provider's
   read/delete credential. It consumes only non-secret resource identity
   metadata from the mutation job, re-reads the live resource, verifies every
   ownership field and the expected provider project, then deletes and polls
   until the resource is absent. A missing resource is success. An identity
   mismatch is a hard failure and never a reason to broaden deletion.
3. An independent scheduled sweeper runs every 15 minutes and can also be
   dispatched manually. It lists the dedicated project directly through the
   provider API and destroys only resources with the complete ownership set
   above whose `expires_at` is at least 25 minutes old. This 45-minute minimum
   age avoids racing a 20-minute lease plus delayed cleanup. It also removes
   managed SSH keys only after proving they belong to the same resource/run.

The sweeper must not depend on a GitHub artifact, local Crabbox claim, or the
original runner. Those are precisely the things that may be missing after a
hard cancellation. Its delete operation is provider-specific, ownership
checked, idempotent, and covered by offline fixtures plus a manual canary before
the nightly schedule is enabled.

`if: always()` is not an absolute guarantee: a force-cancelled workflow, GitHub
Actions outage, or credential failure can prevent the cleanup job from
starting. The independent sweeper and provider-side credit limit are therefore
required, not optional follow-ups.

## Credential Governance

The `live-smoke` environment contains these secrets:

| Secret | Scope |
| --- | --- |
| `DIGITALOCEAN_TOKEN` | Dedicated team; account/region/size/image reads, Droplet create/read/update/delete, action reads, managed SSH-key create/read/delete, and tag create/read/update needed by the runner. No team, billing, registry, Kubernetes, database, volume, snapshot, or Spaces administration. |
| `DIGITALOCEAN_SWEEPER_TOKEN` | Same dedicated team; resource/tag/SSH-key list, read, and delete only. It must not create Droplets. |
| `SCW_ACCESS_KEY`, `SCW_SECRET_KEY` | Dedicated IAM application in the live-smoke project; Instance, image/catalog, network-read, and managed SSH-key operations needed by the runner. No organization, billing, IAM administration, object storage, Kubernetes, database, or DNS permissions. |
| `SCW_SWEEPER_ACCESS_KEY`, `SCW_SWEEPER_SECRET_KEY` | Separate IAM application in the same project; Instance and managed SSH-key list/read/delete only. It must not create Instances. |

Project/team IDs, regions, zones, images, sizes, TTLs, and budget values are
environment variables rather than secrets. They remain fixed policy inputs and
must not be workflow-dispatch text fields.

Credentials are never shared with production, developer machines, other
environments, or fork workflows. Rotate them at least every 90 days, immediately
after suspected exposure or unexpected use, and whenever an operator with
access leaves the maintainer group. Rotation replaces one credential at a time,
runs read-only inventory with the replacement, verifies cleanup with the
sweeper credential, and revokes the old credential only after inventory is
empty. GitHub environment access and provider audit logs are reviewed monthly.

## Failure And Cancellation Semantics

Every provider row emits exactly one terminal classification:

| Classification | Workflow treatment |
| --- | --- |
| `live_smoke_passed` | Success only when create, readiness, command, inventory, targeted delete, and final absence all passed. |
| `quota_blocked` or `environment_blocked` | Inconclusive warning. It does not count as live proof and does not trigger an automatic mutation retry. Repeated occurrences are an operational failure to investigate. |
| `validation_failed` | Failure. The adapter or evidence contract did not behave as expected. Cleanup still runs. |
| `cleanup_failed` | Failure and the highest-severity result, even if command execution succeeded. Run the sweeper immediately and inspect provider inventory. |
| `budget_blocked` | Safe skip before mutation. It is not proof; the schedule stays disabled or the monthly window must reset before another run. |

Workflow concurrency never cancels an in-progress run. A normal step failure or
job timeout proceeds to targeted cleanup. A user cancellation may prevent the
separate cleanup job, so the scheduled sweeper is the cancellation backstop. A
force cancellation is never recorded as clean merely because the mutation job
stopped.

Each run writes a sanitized GitHub job summary containing the source commit,
provider, fixed size and region/zone, run ID and attempt, lifecycle stage
timings, terminal classification, cleanup result, and a link to the sweeper run
that removed an orphan when applicable. Logs and short-retention artifacts may
contain provider resource IDs and slugs, but not tokens, SSH private keys,
public IP addresses, rendered SSH commands, or unredacted provider responses.
Only `live_smoke_passed` is reported as evidence that the provider lifecycle is
currently healthy.

The sweeper reports the count and sanitized identities it examined, deleted, or
refused. Finding and deleting an orphan makes the sweeper itself succeed at
reconciliation but raises a workflow error annotation and opens an operational
investigation into the original run. A refused identity mismatch fails the
sweeper without deleting anything.

## Implementation Requirements

The future workflow and sweeper implementation must land together with shape
tests that assert the trigger allowlist, main-only guards, literal provider
allowlist, environment name, read-only GitHub permissions, SHA-pinned actions,
bounded timeouts, sequential matrix, non-cancelling concurrency, no free-form
ref/provider inputs, and no secret use outside jobs bound to `live-smoke`.

The dedicated provider runners must accept the fixed run identity and TTL,
reject any size outside the policy, record cleanup identity before subsequent
steps, redact evidence, and return the terminal classifications above. The
sweeper needs unit fixtures covering absent tags, partial tags, wrong project,
future and malformed timestamps, `keep=true`, ambiguous matches, missing SSH
keys, already-deleted resources, and provider API failures. Destructive canary
proof must show that it deletes a complete expired fixture and refuses a
lookalike.

## Activation Checklist

The maintainer must complete every item below before adding or enabling the
hosted workflow:

- [ ] Approve the exact `digitalocean` and `scaleway` allowlist, USD 0.25
  per-provider run cap, USD 0.50 scheduled-run cap, and USD 20 monthly cap.
- [ ] Name one operational owner and one backup who receive cleanup, budget,
  credential, and repeated-inconclusive alerts.
- [ ] Create a dedicated DigitalOcean team and Scaleway project used by no
  other workload; configure one-compute-resource quotas and remove unneeded
  billable-product entitlements.
- [ ] Configure a hard USD 10 monthly provider cap for each project, or keep
  that provider's scheduled row disabled until a reviewed fail-closed
  equivalent exists; do not substitute a budget email.
- [ ] Create the four least-privilege provisioning/sweeper credential sets
  described above and verify the sweeper credentials cannot create resources.
- [ ] Create the GitHub `live-smoke` environment, restrict deployments to
  `main`, disallow administrator bypass where supported, and decide whether
  required reviewers will intentionally turn schedules and sweeps into approval
  queues.
- [ ] Add only the six named credential secrets to `live-smoke`; add fixed
  project/team IDs, regions/zones, images, `s-1vcpu-1gb`, `DEV1-S`, TTLs, and
  budget values as protected environment variables.
- [ ] Implement provider-native run ownership metadata, targeted cleanup, and
  separately credentialed DigitalOcean and Scaleway sweepers; prove each
  sweeper deletes one expired canary and refuses one lookalike.
- [ ] Add the schedule/dispatch workflow and its shape tests in one PR, with no
  pull-request-capable trigger and no active provider outside the allowlist.
- [ ] Dispatch each provider once from `main`; attach the sanitized run URLs to
  https://github.com/openclaw/crabbox/issues/944 and verify provider inventory
  and managed SSH-key inventory are empty afterward.
- [ ] Dispatch the sweeper once with empty inventory, then enable the nightly
  smoke and 15-minute sweep schedules.
- [ ] Record the credential rotation date, monthly audit date, alert routing,
  and emergency procedure: disable new runs, revoke provisioning credentials,
  retain sweeper credentials, reconcile to empty, then revoke or rotate the
  sweepers.
