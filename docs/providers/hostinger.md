# Hostinger Provider

Read this when you:

- choose `provider: hostinger`;
- configure Hostinger VPS catalog ids, template ids, data center ids, or SSH
  defaults;
- need the purchase, release, cleanup, or billing contract for Hostinger leases.

Hostinger is a **direct-only Linux SSH-lease** provider. Crabbox talks to the
Hostinger API from the local CLI, purchases and sets up a VPS only after an
explicit opt-in, waits for public SSH, then uses the normal SSH sync/run/ssh
workflow. It never goes through the Crabbox coordinator.

Hostinger billing remains account-owned. `crabbox stop` stops the VPS and
retains Crabbox's local claim and SSH key for later reuse; it does not delete the
VPS, cancel a subscription, or guarantee that Hostinger stops billing.

## Prerequisites

- A Hostinger API token in the shell or a private user config.
- Hostinger VPS ids for the priced item, template, and data center you want to
  use, plus an active default payment method or explicit payment method id. The
  item id must be a purchasable priced id such as
  `hostingercom-vps-kvm2-usd-1m`, not only the parent catalog family id such as
  `hostingercom-vps-kvm2`.
- An Ubuntu or Debian template. Crabbox validates the selected template before
  purchase because its remote bootstrap uses `apt-get` to install required SSH
  workflow tools.
- Local `ssh`, `ssh-keygen`, and `rsync` for the SSH lease workflow.
- A funded Hostinger account when running commands that purchase a VPS.

Keep the API token out of repo config and command lines. Prefer an environment
variable:

```sh
export HOSTINGER_API_TOKEN="<hostinger-api-token>"
```

`CRABBOX_HOSTINGER_API_TOKEN` is also accepted and takes precedence over
`HOSTINGER_API_TOKEN`.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes (public SSH) |
| Crabbox sync (rsync over SSH) | Yes |
| Provider-managed sync | No |
| Desktop / browser / code | No |
| Actions hydration | Yes (Linux SSH lease) |
| Coordinator (broker) | No - direct only |
| Tailscale | No (rejected; public SSH only) |
| Advertised cleanup feature | Yes (conservative stop-only sweep) |

The live provider matrix reports `ssh`, `crabbox-sync`, and `cleanup`.
Hostinger cleanup is intentionally conservative: the command can stop
VPSs with matching local CloudID-bound claims, but it does not delete or cancel
them.

## Configuration

```yaml
provider: hostinger
target: linux
hostinger:
  apiUrl: https://developers.hostinger.com
  itemId: "<hostinger-priced-item-id>"
  paymentMethodId: "<hostinger-payment-method-id>"
  templateId: "<hostinger-template-id>"
  dataCenterId: "<hostinger-data-center-id>"
  hostnamePrefix: crabbox
  user: root
  workRoot: /work/crabbox
  allowPurchase: false
  releaseAction: stop
```

Defaults:

- `apiUrl`: `https://developers.hostinger.com`
- `hostnamePrefix`: `crabbox`
- `user`: `root`
- `workRoot`: `/home/<user>/crabbox`
- `allowPurchase`: `false`
- `releaseAction`: `stop`

Do not put `apiToken` in repo-local YAML. If you need a file-backed token, keep
it in a private user config or credential manager and make sure
`crabbox config show` reports only a configured/missing state, never the value.
Repo-local config cannot set `apiToken`, `apiUrl`, `itemId`,
`paymentMethodId`, `templateId`, `dataCenterId`, or enable `allowPurchase`.
This prevents a repository from selecting the Hostinger account, redirecting
credentials, or choosing billable inputs. Set those through flags, environment
variables, private user config, or an explicit `CRABBOX_CONFIG` file.

Provider flags:

```text
--hostinger-url
--hostinger-item-id
--hostinger-payment-method-id
--hostinger-template-id
--hostinger-data-center-id
--hostinger-hostname-prefix
--hostinger-user
--hostinger-work-root
--hostinger-allow-purchase
--hostinger-release-action
```

Environment overrides:

```text
HOSTINGER_API_TOKEN
CRABBOX_HOSTINGER_API_TOKEN
HOSTINGER_API_URL
CRABBOX_HOSTINGER_API_URL
CRABBOX_HOSTINGER_ITEM_ID
CRABBOX_HOSTINGER_PAYMENT_METHOD_ID
CRABBOX_HOSTINGER_TEMPLATE_ID
CRABBOX_HOSTINGER_DATA_CENTER_ID
CRABBOX_HOSTINGER_HOSTNAME_PREFIX
CRABBOX_HOSTINGER_USER
CRABBOX_HOSTINGER_WORK_ROOT
CRABBOX_HOSTINGER_ALLOW_PURCHASE
CRABBOX_HOSTINGER_RELEASE_ACTION
```

## Lifecycle

### Doctor

`crabbox doctor --provider hostinger` is read-only. It requires a token, lists
VPS inventory, priced VPS catalog entries, payment methods, templates, and data
centers through the Hostinger API, and reports
`purchase=explicit release=stop`. It never purchases, starts, stops, deletes, or
cancels a VPS.

```sh
crabbox doctor --provider hostinger
crabbox doctor --provider hostinger --json
```

The `purchase-options` check fails when a configured id is unavailable, the
payment method is inactive or ambiguous, or the template is not Ubuntu/Debian.
Before required selectors are configured, it reports a non-failing warning and
still returns the available ids for discovery.
It still includes the configured ids plus bounded `priced_items`,
`payment_methods`, `templates`, and `data_centers` summaries so you can correct
the configuration without a mutating command.
Payment-method output contains only the Hostinger id, method name, and state; it
does not expose card identifiers:

```sh
crabbox doctor --provider hostinger --json |
  jq '.checks[] | select(.check == "purchase-options")'
```

Catalog prices are reported in Hostinger's integer minor units. For example,
`899USD/1month` means USD 8.99 for the first monthly period. Review the current
renewal price and subscription terms in Hostinger before purchasing.

### Acquire

`warmup` and `run` can create billable Hostinger resources. They refuse to
purchase unless one of these is set:

```sh
crabbox warmup --provider hostinger --hostinger-allow-purchase
```

```yaml
# Private user config, or a file explicitly selected with CRABBOX_CONFIG.
hostinger:
  allowPurchase: true
```

The provider also requires `itemId`, `templateId`, and `dataCenterId`. Use a
priced Hostinger item id such as `hostingercom-vps-kvm2-usd-1m`; Hostinger
rejects the parent catalog family id, for example `hostingercom-vps-kvm2`, with
an item-price error. The template must identify Ubuntu or Debian. If
`paymentMethodId` is unset, Crabbox uses exactly one active default payment
method. It refuses to purchase if any configured id is unavailable, the
template is unsupported, no active default exists, or the default choice is
ambiguous. During acquire it:

1. validates the priced item, payment method, template, and data center against
   the current read-only Hostinger catalog;
2. lists existing VPSs to allocate a safe Crabbox slug;
3. creates a per-lease SSH key;
4. stores a private local recovery record beside that key;
5. sends the public key inside the atomic Hostinger setup request;
6. purchases and sets up a VPS named
   `crabbox-<slug>-<lease-suffix>` unless `hostnamePrefix` changes that prefix;
7. records the paid VPS id in a local claim as soon as Hostinger returns it;
8. removes the recovery record after the claim is durable;
9. waits for Hostinger to expose a public IP;
10. prepares Crabbox's configured work root and Linux readiness checks over SSH;
11. waits for SSH readiness and runs the normal SSH transport.

If the purchase response is interrupted, Crabbox looks for exactly one VPS with
the generated hostname. If the VPS is not visible before the recovery window
expires, Crabbox retains a pending local claim and SSH key. A later resolve by
lease id or slug searches for that exact hostname, binds the Hostinger VPS id to
the stable claim, and clears the pending marker. If the paid-resource claim
write itself fails, the recovery record preserves the original lease-to-key
mapping for a later resolve by VPS id or hostname. If a later setup step fails,
Crabbox keeps the local claim and SSH key for recovery and stops the VPS unless
`--keep` was set. The subscription remains account-owned and may continue
billing in every case.

Hostinger may reject an API purchase with HTTP `402` even when `doctor` reports
an active default payment method. This means the payment or order requires
interactive checkout, card authentication, or another billing action in hPanel.
Crabbox reports the failure without purchase recovery because Hostinger did not
accept the order. `doctor` proves that the payment method is discoverable; it
cannot guarantee that Hostinger will permit a non-interactive charge.

Example:

```sh
crabbox run \
  --provider hostinger \
  --hostinger-item-id "<hostinger-priced-item-id>" \
  --hostinger-payment-method-id "<hostinger-payment-method-id>" \
  --hostinger-template-id "<hostinger-template-id>" \
  --hostinger-data-center-id "<hostinger-data-center-id>" \
  --hostinger-allow-purchase \
  -- go test ./...
```

### Resolve, List, And SSH

Hostinger leases can be resolved by Crabbox lease id, local slug, Hostinger VM
id, or a Crabbox-created Hostinger hostname. Listing defaults to VPSs with
local CloudID-bound Crabbox claims; `--all` includes every VPS returned by the
account. A matching hostname alone is not ownership proof for stop or cleanup.

When an SSH command resolves a stopped Hostinger VPS, Crabbox starts it, waits
for a public SSH endpoint, then reuses the stored per-lease key. Read-only
`status` does not start a stopped VPS. `run` may prepare the configured work root
and readiness checks before command execution. Root or passwordless-sudo SSH
users can also install the optional system readiness helper; non-root users keep
to the configured work root and existing SSH tools.

The lease claim stores the effective Hostinger SSH user and work root. Values
supplied only on the original acquire command therefore remain attached to that
VPS and do not need to be repeated on later `run`, `ssh`, `status`, or `stop`
commands.

```sh
crabbox list --provider hostinger
crabbox list --provider hostinger --all
crabbox ssh --provider hostinger --id "<lease-or-slug>"
```

### Release

`crabbox stop` and the compatibility alias `crabbox release` call the Hostinger
stop action, wait until Hostinger reports the VPS as stopped, preserve the local
CloudID-bound claim for reuse, and print a provider-specific message with
`billing=still-owned`.

```sh
crabbox stop --provider hostinger "<lease-or-slug>"
```

Only `hostinger.releaseAction: stop` is supported. Any other value is rejected.
If stop confirmation times out or fails, the local claim and SSH key remain
available for recovery. After confirmed stop, Crabbox marks the claim stopped
and retains the stored per-lease private key so the VPS can be restarted and
reused by the original lease id or slug.
Stopping a VPS is not the same as deleting it or cancelling a Hostinger plan.
Check the Hostinger dashboard after release if cost or subscription state
matters.

### Cleanup

Hostinger cleanup is stop-only and dry-run-first. It lists VPSs, skips anything
that is not positively identified as Crabbox-owned, and prints `stop` lines for
matching VPSs. With `--dry-run`, it prints the same decisions without making
provider calls.

```sh
crabbox cleanup --provider hostinger --dry-run
crabbox cleanup --provider hostinger
```

After a successful stop, cleanup marks the matching local claim stopped and
skips already stopped VPSs on later sweeps. Cleanup does not delete VPSs, cancel
subscriptions, or claim full lifecycle garbage collection. Use it only as a
direct-mode safety net for VPSs whose names and local claims match Crabbox's
Hostinger ownership pattern.

## Verification

Deterministic local checks:

```sh
crabbox providers --json
crabbox doctor --provider hostinger
crabbox cleanup --provider hostinger --dry-run
```

`providers --json` does not contact Hostinger. `doctor` and cleanup dry-run
contact the Hostinger API but must not mutate state.

For live smoke, load the token without printing it, then run a read-only doctor
before any billable command:

```sh
set -a
. ./.hostinger.env
set +a
crabbox doctor --provider hostinger
```

The env file should contain variable assignments such as
`HOSTINGER_API_TOKEN=<hostinger-api-token>` and should not be committed. Do not
use `set -x` around token loading.

A mutating smoke must opt in explicitly and must be followed by `stop` plus a
Hostinger dashboard check for billing/subscription state:

```sh
crabbox warmup \
  --provider hostinger \
  --hostinger-item-id "<hostinger-priced-item-id>" \
  --hostinger-payment-method-id "<hostinger-payment-method-id>" \
  --hostinger-template-id "<hostinger-template-id>" \
  --hostinger-data-center-id "<hostinger-data-center-id>" \
  --hostinger-allow-purchase

crabbox stop --provider hostinger "<lease-or-slug>"
```

## Gotchas

- No aliases are registered; use `hostinger` exactly.
- `--target` values other than `linux` are rejected.
- `--tailscale` is rejected; Hostinger leases expose public SSH.
- `--hostinger-allow-purchase` is required for any billable acquire.
- `--hostinger-item-id` must be the priced item id, for example
  `hostingercom-vps-kvm2-usd-1m`.
- A sole active default payment method is selected automatically; otherwise set
  `--hostinger-payment-method-id` explicitly.
- A discovered payment method may still require interactive hPanel checkout.
  HTTP `402` is a deterministic payment failure, not an ambiguous VPS creation.
- `crabbox stop` stops the VPS but does not delete or cancel it.
- Cleanup is stop-only; the shared `cleanup` capability does not imply deletion
  or subscription cancellation.

Related docs:

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Configuration](../features/configuration.md)
- [Lifecycle and cleanup](../features/lifecycle-cleanup.md)
