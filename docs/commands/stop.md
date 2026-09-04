# stop

`crabbox stop` ends a single lease. For coordinator-backed and direct cloud
providers it releases or deletes the backing machine; for delegated runners it
tears down the underlying sandbox; for static `provider=ssh` hosts it attempts
connection cleanup and removes the local claim without stopping or deleting
the machine.

```sh
crabbox stop swift-crab
crabbox stop --id cbx_0a1b2c3d4e5f
crabbox stop --provider namespace-devbox swift-crab
crabbox stop --provider daytona swift-crab
crabbox stop --provider e2b swift-crab
crabbox stop --provider ssh --static-host mac-studio.local mac-studio.local
```

`crabbox release` is a compatibility alias for `crabbox stop`.

For coordinator-backed leases, the preliminary lookup has a ten-second budget.
If it stalls, ordinary stop warns and proceeds through the existing
provider-scoped release request. Provider identity mismatches still block
release; `--force` still requires successful inspection. Canceling the command
does not start a release fallback. Cleanup must still be confirmed before local
claim and SSH artifacts are removed.

If a fixed-ID create was admitted by the coordinator but never allocated a
machine, `stop` cancels that intent and confirms the cancellation even when
the preliminary lease lookup returns 404. This includes a create rejected by
a quota check. The owner, organization, and selected provider must match.
Delayed creates cannot allocate after this confirmation; a genuinely unknown
ID still fails, and an allocation already in progress must finish cleanup
before Stop reports success.

## Identifying the lease

Pass the lease as a positional argument or with `--id`; both accept the
canonical `cbx_...` ID or an active friendly slug (see
[Identifiers](../features/identifiers.md)). Supplying both `--id` and a
positional argument, or more than one positional argument, is an error.

Several providers also accept their own native identifiers in addition to the
Crabbox lease ID and local slug:

- `aws` — direct fixed-ID canonical stops can replay a retained terminal
  receipt after successful instance/key cleanup, with fresh account, configured
  region, identity, and inventory checks. Older compact tombstones lack the
  required binding and still fail closed after upgrading; missing inventory
  alone never acknowledges cleanup. This does not extend to slug, raw instance,
  or ordinary non-fixed lookups. See [AWS fixed-ID replay](../providers/aws.md#fixed-id-replay).
- `blacksmith-testbox` — accepts a Testbox ID or slug only with an exact local
  organization/API-scoped claim and matching native workflow identity. It stops
  the Testbox (also cancelling its backing Actions run) and removes the claim/key
  only after fresh native stdout confirms the exact ID in state `completed`.
  Failed native stops can reconcile through the same confirmation; ambiguous,
  failed or cancelled queries retain the original stop error and local state.
  Verification and local artifact cleanup failures remain visible alongside the
  native error and exit code. Failed artifact removal retains the exact claim;
  an already absent lease key directory is safe to finalize.
  Legacy or lost claims require independently verified native Blacksmith cleanup;
  raw IDs alone never authorize stop. See
  [Blacksmith Testbox](../features/blacksmith-testbox.md).

- `blaxel` — accepts a Crabbox lease ID (`blx_<sandbox-id>`) or local slug and
  deletes the Blaxel sandbox only when the local claim and remote ownership
  labels match. Missing sandboxes keep the local claim unless
  `--blaxel-forget-missing` is set.
- `namespace-devbox` — shuts down the Namespace Devbox by default and retains
  its exact local claim and SSH files for reuse. Set
  `namespace.deleteOnRelease` (or pass `--namespace-delete-on-release`) to
  delete the Devbox and local SSH files instead. Both operations reject
  missing or mismatched claims; `--force` is unsupported because Devbox
  inventory cannot independently prove lost-claim ownership.
- `namespace-instance` — accepts a lease ID, local slug, or Namespace instance
  ID and destroys the Compute instance only with an exact scoped local claim.
  `--force --id <exact-instance-id>` can recover a lost claim after verifying
  the instance's live Crabbox ownership labels and Namespace tenant.
- `morph` — requires an exact API-scoped local ownership claim and fresh
  matching instance metadata before pausing or deleting an instance. It pauses
  by default and retains the claim and SSH key for reuse; set
  `morph.deleteOnRelease` (or pass `--morph-delete-on-release`) to delete the
  instance and key instead. Failed provider operations preserve the claim.
- `exe-dev` — accepts a Crabbox lease ID, local slug, or exe.dev VM name only
  when an unchanged local claim binds the exact deterministic VM name,
  complete remote ownership tags, and current control route. Claimless or
  legacy unscoped tagged VMs require explicit `--reclaim` through a normal
  reuse command before stop; untagged VMs remain read-only inventory. Failed
  deletion keeps the claim.
- `semaphore` — requires an exact organization-host/project-scoped local claim
  and fresh live job ownership before stopping the Semaphore CI job. Failed
  stops preserve the claim and SSH key; verified legacy claims are safely
  upgraded before the fenced stop.
- `sprites` — requires an exact API-scoped local claim plus fresh matching
  provider ownership labels before deleting the sprite. Failed deletion keeps
  the claim; claimless sprites require explicit `--reclaim` reuse.
- `tenki` — requires an exact endpoint/workspace/project-scoped local claim plus
  fresh matching session ownership metadata before terminating the sandbox.
  Failed termination keeps the claim; claimless sessions require explicit
  `--reclaim` reuse.
- `daytona` — deletes the Daytona sandbox.
- `coder` — stops the Coder workspace by default and removes the local claim.
  Set `coder.deleteOnRelease` or pass `--coder-delete-on-release` to delete the
  workspace instead.
- `islo` — accepts an exactly claimed `isb_...` ID, Crabbox-created sandbox
  name, or local slug and deletes the Islo sandbox. Claimless canonical names
  must first be adopted through an explicit supported `--reclaim` reuse.
- `freestyle` — accepts an exactly claimed `fsb_...` ID, Crabbox-created VM
  name, or local slug and deletes the Freestyle VM. Claimless canonical names
  remain visible to status/list but cannot be deleted until explicit
  `--reclaim` reuse persists a claim.
- `runpod` — accepts a lease ID, pod ID, pod name, or local slug only when an
  exact local claim binds that RunPod id and provider-returned name. Unclaimed
  and legacy pods remain visible to status/list but require explicit
  `--reclaim` reuse before deletion.
- `e2b` — accepts a Crabbox lease ID, local slug, or E2B sandbox ID only when
  an exact local claim binds the sandbox and configured API endpoint. Claimless
  raw or `e2b_<sandboxID>` identifiers require explicit `--reclaim`; Crabbox
  re-reads canonical remote ownership metadata and persists the exact claim
  before deletion. Failed deletion retains the claim for an exact retry.
- `railway` — refuses unclaimed service IDs. Use `--reclaim` only after
  inspecting the configured API endpoint, project, environment, service, and
  current deployment; Crabbox persists that exact one-deployment binding before
  stopping it. Failed stops retain the claim for an exact retry, while successful
  stops remove it.
- `hetzner` — requires canonical remote ownership labels and an exact local
  claim bound to the server ID and lease ID. Unclaimed resources must first be
  explicitly reclaimed through a normal reuse command; failed deletion keeps
  the claim for an exact retry.
- `vercel-sandbox` — accepts a Crabbox-created local slug or `vsbx_...` lease
  ID, verifies ownership metadata, deletes the Vercel Sandbox, and removes the
  local claim. Missing remote sandboxes preserve the claim unless
  `--vercel-sandbox-forget-missing` is explicit.
- `cloudflare-dynamic-workers` — accepts a local claim, lifecycle run ID, or
  slug, deletes loader metadata for that run, and removes the local claim.
  Stable and explicit Worker cache IDs are not lifecycle IDs. If the loader
  already reports `not found`, Crabbox removes the stale local claim.
- `cloudflare-sandbox` — accepts a Crabbox-created local slug or `cfsbx_...`
  lease ID, verifies ownership metadata, deletes the Cloudflare Sandbox through
  the configured bridge, and removes the local claim. Missing remote sandboxes
  preserve the claim unless `--cloudflare-sandbox-forget-missing` is explicit.
- `docker-sandbox` — accepts only a Crabbox lease ID or local slug backed by a
  `provider=docker-sandbox` local claim, then removes the sandbox with
  `sbx rm --force`. This is destructive cleanup, not Docker Sandbox pause, and
  it remains the manual cleanup path for clone-mode Docker Sandbox runs that
  Crabbox keeps after a successful one-shot command.
- `hostinger` — stops the VPS and retains its local claim and SSH key for later
  reuse. Hostinger still owns the subscription and may continue billing it.
- `ssh` (static hosts) — attempts shared connection cleanup, then removes the
  local claim without stopping or deleting the host. See
  [Static SSH connection cleanup](../providers/ssh.md#connection-cleanup).
- `xcp-ng` — requires an exact pool/account-scoped local claim for the same
  Crabbox lease, slug, and VM UUID, then verifies fresh live ownership metadata
  before deleting the VM. Missing or mismatched claims never authorize
  deletion, and provider failures preserve the claim for a safe retry.

## Behavior by provider mode

The action `stop` takes depends on how the lease was created:

- **Coordinator-backed** (`aws`, `azure`, `daytona`, `gcp`, `hetzner` brokered
  through a configured broker) — releases the lease through the broker and prints
  `released lease=<id> server=<id>`. If the lease cannot be inspected first,
  `stop` warns and still attempts the release by ID.
- **Direct cloud and local providers** — usually delete the backing server and
  print `deleted lease=<id> server=<id> name=<name>`, but retain-capable
  providers such as `namespace-devbox`, `morph`, `kubevirt`, and `incus` stop
  or pause instead
  when their `*.deleteOnRelease` setting is `false` (some providers print a
  provider-specific release message instead, for example
  `stopped lease=<id> instance=<name> retained=true` for retained Incus
  instances). Hostinger is stop-only and prints `billing=still-owned`; it does
  not delete or cancel the subscription.
- **DigitalOcean, Linode, Vultr, and Scaleway** — require canonical live
  ownership tags plus an exact local claim for the same provider account or
  project and resource before reuse or deletion. Claimless resources remain
  visible in read-only inventory and require explicit supported `--reclaim`
  reuse before `stop` may delete them.
- **Delegated runners** — call the provider's own teardown for the resolved
  sandbox.

For `provider=docker-sandbox`, `crabbox stop` intentionally keeps Crabbox's
cross-provider cleanup meaning. Use [`ports`](ports.md) and [`cp`](cp.md) for
non-destructive post-create workflows on a running sandbox. The separate
[`pause`](pause.md) and [`resume`](resume.md) commands are provider-dependent
and are not supported by Docker Sandbox.

Coordinator-backed stops refresh guest connection state inside the release owner.
A confirmed deletion skips guest SSH cleanup and repeats only local connection
cleanup, without another provider release request. Retained machines and pending
or failed provider cleanup do not count as confirmed deletion.

For SSH leases, shared connection cleanup makes best-effort attempts to signal
[Actions hydration](../features/actions-hydration.md) shutdown, stop local
mediated-egress daemon state and supported remote egress clients, and log out
remote Tailscale when stored lease metadata marks it enabled. Providers can
gate remote cleanup behind their ownership checks. The ordered remote cleanup
chain has a 35-second budget, including coordinator guest network selection and
reserving five-second windows for later egress
and Tailscale cleanup. Responsive hydrated jobs keep their normal 20-second
stop-marker grace; cancellation or the phase deadline ends that wait early.
The local egress daemon stays alive through guest cleanup. Coordinator-backed
explicit stops share one five-minute cancellation budget from the first lease
inspection through claim acquisition, guest cleanup, release requests, and cleanup
observation; an earlier caller deadline wins. Phase limits cannot restart this
budget. Pending or failed provider cleanup still returns an error and preserves
the local claim and SSH artifacts for a later retry.

After confirmed coordinator-backed deletion, SSH masters created with canonical
lease credentials are explicitly closed and observed to exit before local
artifacts are removed. If that step fails, Stop reports that remote deletion is
confirmed but local cleanup remains pending;
the retained claim permits a local-only retry.

Local daemon lock waits also honor the operation context. Once provider deletion
is confirmed, a canceled local daemon cleanup warns without undoing that result.
Already-started local process teardown remains joined. Synchronous filesystem
operations and existing process-inspection and termination helpers are not
interrupted by this context, so this is not a strict wall-clock limit.
Direct and delegated providers retain their existing caller lifetime. Static SSH attempts cleanup
before local unclaiming, even without hydration state; remote failures warn
but do not block unclaiming. See the [static provider details](../providers/ssh.md#connection-cleanup)
for marker paths, Linux egress process-matching scope, and Tailscale limits.

## Flags

`stop` accepts the shared provider-selection and target flags. The most common:

```text
--provider <name>          provider to act against (see crabbox providers)
--id <lease-or-slug>        lease ID or slug (equivalent to the positional arg)
--reclaim                   explicitly adopt a provider resource when that provider supports safe stop adoption
--force                     recover one exact resource through verified provider adoption or an inspected coordinator lease
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>        static SSH host (provider=ssh)
--static-user <user>        static SSH user (provider=ssh)
--static-port <port>        static SSH port (provider=ssh)
--static-work-root <path>   static target work root (provider=ssh)
```

`--force` is a targeted recovery operation, not an ownership bypass. It always
requires both an explicit `--provider` and an exact `--id`; positional IDs,
coordinator slugs, `--reclaim`, and internal controller release-identity flags
cannot be combined with it. Providers with a
verified stop-adoption contract inspect the exact remote resource, validate its
provider scope and ownership metadata, persist a conflict-safe local claim,
and then perform their ordinary claim-fenced stop. Brokered providers inspect
the exact coordinator lease and verify its provider before release; inspection
failures never fall back to releasing an unverified ID. Providers without a
verified recovery contract reject `--force` and direct the operator to their
native provider CLI. `cleanup` does not support `--force`.

`--reclaim` remains the existing provider-specific adoption interface where
supported. `--force` is the consistent cross-provider recovery interface for
one exact resource: it reuses verified adoption for supported direct providers
and adds fresh, exact-lease inspection for coordinator-backed providers. Neither
flag bypasses provider ownership, scope, or claim-fencing requirements.

Each provider also registers its own flags; the ones relevant to `stop` include:

```text
--namespace-delete-on-release            delete the Namespace Devbox instead of shutting it down
--coder-delete-on-release                delete the Coder workspace instead of stopping it
--exe-dev-control-host <host>            exe.dev SSH API host
--sprites-api-url <url>                  Sprites API URL
--e2b-api-url <url>                      E2B API URL
--e2b-domain <domain>                    E2B sandbox domain
--hostinger-url <url>                    Hostinger API URL
--hostinger-release-action stop          Hostinger release action; only stop is supported
--azure-dynamic-sessions-endpoint <url>  Azure Container Apps Dynamic Sessions endpoint
--blaxel-forget-missing                  remove a Blaxel claim after confirming the sandbox is already gone
--cloudflare-dynamic-workers-url <url>   Cloudflare Dynamic Workers loader URL
--cloudflare-sandbox-url <url>           Cloudflare Sandbox bridge URL
--cloudflare-sandbox-forget-missing      forget a local claim when the bridge reports the sandbox missing
```

Run `crabbox stop --help` for the full, provider-aware flag list, and
`crabbox providers` for the providers available in your build.

Generated stop commands use each provider's routing hook, preserving its
endpoint, scope, and explicit release policy, including `false` overrides.
Aliases are printed as canonical provider names. Configured Azure
subscription/resource group/location, GCP project/zone, AWS region, and inherited
Kubernetes kubeconfig lists are carried as environment assignments where
appropriate; keep those assignments when copying the command. API credentials
are not included. External adapters continue to use private, digest-bound
routing files instead of printing arbitrary adapter config or arguments.

For legacy claim scopes that include sensitive URL bytes, a generated command
leaves the endpoint in your private configuration instead of printing it or
overriding it with a differently scoped URL. Keep that original configuration
available when replaying the command. This applies to Railway query/fragment
identities, opaque E2B/CubeSandbox, Proxmox, and Namespace Instance identities,
and Morph URL userinfo; existing claim keys are not rewritten.

## See also

- [`cleanup`](cleanup.md) — sweep expired direct-provider machines and stale
  local state.
- [`ports`](ports.md) / [`cp`](cp.md) — non-destructive Docker Sandbox follow-up
  operations.
- [`pond release`](pond.md) — stop every lease in a named pond at once.
- [`admin`](admin.md) — coordinator-side `release` and `delete` for operators.
- [Lifecycle & cleanup](../features/lifecycle-cleanup.md) — how leases expire
  and get reclaimed.
