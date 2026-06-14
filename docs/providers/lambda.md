# Lambda Provider

Read this when you are:

- choosing `provider: lambda`;
- validating a direct Lambda Cloud GPU SSH lease;
- changing `internal/providers/lambda` or the guarded live smoke.

Lambda is a Linux-only **SSH lease** provider for Lambda Cloud on-demand
instances. Crabbox creates a Lambda instance, injects a per-lease SSH key and
cloud-init user data, records Crabbox ownership metadata in the local lease
claim, waits for SSH/bootstrap readiness, and then uses the normal Crabbox SSH
sync/run/stop/cleanup path.

Lambda is **direct-only** in this release. It does not run through the
coordinator, so the local CLI must have a Lambda API key and direct cleanup
remains the operator's responsibility. Lambda instances are billable while they
are running; stopping a Crabbox lease terminates the instance rather than
pausing it.

## When To Use It

Use Lambda when you need a direct Linux GPU instance and local Lambda credentials
are acceptable. Prefer AWS, Azure, GCP, or Hetzner when you need a brokered team
path, coordinator-side credentials, or cloud-specific cost accounting. Prefer a
non-GPU direct provider such as DigitalOcean or Linode when a plain CPU Linux
box is enough.

## Commands

```sh
crabbox doctor --provider lambda
crabbox warmup --provider lambda --class standard
crabbox run --provider lambda --type gpu_1x_a10 -- go test ./...
crabbox ssh --provider lambda --id my-app
crabbox stop --provider lambda my-app
crabbox cleanup --provider lambda --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
Lambda instance id. `--type` is the exact Lambda instance type name; there is no
separate Lambda size flag.

## Configuration

```yaml
provider: lambda
target: linux
class: standard
lambda:
  region: us-west-1
  type: gpu_1x_a10
  imageFamily: lambda-stack-24-04
  image: ""
  firewallRuleset: ""
  sshCIDRs: []
  filesystemNames: []
  filesystemMounts: []
```

Config keys under `lambda:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.Lambda.Region` | `us-west-1` | Lambda region name. |
| `type` | `cfg.Lambda.Type` | `gpu_1x_a10` | Lambda instance type name. |
| `imageFamily` | `cfg.Lambda.ImageFamily` | `lambda-stack-24-04` | Lambda image family used when `image` is empty. |
| `image` | `cfg.Lambda.Image` | empty | Exact Lambda image id. Mutually exclusive with `imageFamily`. |
| `firewallRuleset` | `cfg.Lambda.FirewallRuleset` | empty | Optional existing Lambda firewall ruleset name. |
| `sshCIDRs` | `cfg.Lambda.SSHCIDRs` | empty | Reserved for firewall-aware follow-up work; this phase does not create firewall rules. |
| `filesystemNames` | `cfg.Lambda.FilesystemNames` | empty | Optional existing Lambda filesystem names to attach. |
| `filesystemMounts` | `cfg.Lambda.FilesystemMounts` | empty | Optional existing Lambda filesystems with mount paths, e.g. `cache:/mnt/cache`. |

The portable `--os ubuntu:24.04` selector maps to the default
`lambda-stack-24-04` image family. Other explicit portable OS selectors are
rejected unless `lambda.image`, `lambda.imageFamily`, `CRABBOX_LAMBDA_IMAGE`, or
`CRABBOX_LAMBDA_IMAGE_FAMILY` provides an explicit Lambda image choice.

Lambda leases default to SSH user `ubuntu` on port `22`. Explicit generic
`ssh.user` and `ssh.port` values remain authoritative. The effective values
appear in `crabbox config show --provider lambda`.

Environment overrides:

```text
LAMBDA_API_KEY                         Lambda API key for direct mode
CRABBOX_LAMBDA_REGION                  Override the region name
CRABBOX_LAMBDA_TYPE                    Override the instance type name
CRABBOX_LAMBDA_IMAGE                   Override with an exact image id
CRABBOX_LAMBDA_IMAGE_FAMILY            Override with an image family
CRABBOX_LAMBDA_FIREWALL_RULESET        Optional existing firewall ruleset name
CRABBOX_LAMBDA_SSH_CIDRS               Comma-separated SSH CIDRs, reserved for follow-up
CRABBOX_LAMBDA_FILESYSTEM_NAMES        Comma-separated existing filesystem names
CRABBOX_LAMBDA_FILESYSTEM_MOUNTS       Comma-separated name[:mountPath] entries
```

Do not pass the Lambda API key as a command-line argument. Keep it in the
environment or in a local secret manager.

## Token Scope

The provider uses Lambda account identity, regions, instance types, images,
instances, SSH keys, optional filesystems, and optional firewall rulesets.
Crabbox sends the API key only in the `Authorization: Bearer ...` header.

`crabbox doctor --provider lambda` is non-mutating. It validates auth, region,
capacity for the configured type, image or image family, optional filesystem
references, optional firewall ruleset, and current inventory. Missing API keys,
inactive accounts, invalid billing, quota, and capacity failures are reported as
classed doctor output rather than hidden behind generic errors.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Ensure a Lambda SSH key exists for the lease.
3. Launch one Lambda instance with `region`, `type`, `image` or `imageFamily`,
   cloud-init `user_data`, optional existing `firewallRuleset`, optional
   existing filesystems, and `quantity=1`.
4. Wait for an instance id, public IP address, and Crabbox SSH bootstrap
   readiness.
5. Claim the lease locally with Lambda instance and SSH-key metadata.
6. Run normal Crabbox sync/run/ssh workflows over SSH.
7. Terminate the Lambda instance and owned Lambda SSH key on `stop`; `cleanup`
   deletes resources backed by a complete local Crabbox Lambda claim, and can
   also reclaim complete Crabbox-tagged Lambda instances.

If SSH-key creation, instance launch, or post-launch readiness becomes
indeterminate, Crabbox records a local recovery claim with enough metadata to
retry `crabbox stop --provider lambda <lease-or-slug>`. Empty provider inventory
is not treated as proof that creation failed while the outcome remains
indeterminate: Crabbox retains the recovery claim so a late-created billable
instance can still be reconciled.

## Ownership And Cleanup

Crabbox encodes owned Lambda leases in local claims with flat labels such as:

```text
crabbox=true
created_by=crabbox
provider=lambda
lease=cbx_abcdef123456
slug=my-app
target=linux
expires_at=<unix-seconds>
ttl_secs=<seconds>
```

Release and cleanup require a complete ownership predicate: Crabbox marker,
provider marker, lease id, slug, and Linux target. Lambda instances backed only
by partial, foreign, or malformed Crabbox-like metadata are skipped or refused.

Lambda has no safe tag-update path in this phase, and the launch path does not
depend on provider-side tag persistence. Local Crabbox claims carry fresh touch
and idle-timeout state. Complete Crabbox-tagged Lambda instances are still
understood by `list`, `stop`, and `cleanup` for compatibility with manually
seeded or future provider metadata.
Direct mode has no coordinator alarm. Use:

```sh
crabbox list --provider lambda --json
crabbox cleanup --provider lambda --dry-run
crabbox cleanup --provider lambda
```

## Firewall And Filesystem Scope

Crabbox does not create or mutate Lambda firewall rulesets in this phase. If
`lambda.firewallRuleset` or `CRABBOX_LAMBDA_FIREWALL_RULESET` is set, Crabbox
validates that the named ruleset exists for the configured region and passes its
name to the launch request. Operators own the ruleset policy, including SSH
source restrictions. `lambda.sshCIDRs` and `CRABBOX_LAMBDA_SSH_CIDRS` are
reserved for follow-up firewall work and do not create ingress rules.

Crabbox can attach existing Lambda filesystems by name, optionally with mount
paths. It validates configured filesystem names through `doctor` and passes the
names to Lambda during launch. Crabbox does not create, delete, resize, or
otherwise manage Lambda filesystems.

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=lambda scripts/live-lambda-smoke.sh
```

The script builds `bin/crabbox`, reads `LAMBDA_API_KEY` from the environment,
requires an empty Crabbox-owned Lambda inventory, creates a kept
`gpu_1x_a10` instance by default, waits for ready status, runs `echo ok`,
verifies `list --json`, stops the lease, runs dry-run cleanup, and verifies the
Crabbox-owned inventory is empty afterward.

Optional live-smoke overrides:

```text
CRABBOX_LIVE_LAMBDA_TYPE       Instance type for the smoke, default gpu_1x_a10
CRABBOX_LIVE_LAMBDA_REGION     Region for the smoke, default us-west-1
```

Final classifications include:

```text
classification=live_lambda_smoke_passed
classification=environment_blocked
classification=billing_blocked
classification=quota_blocked
classification=capacity_blocked
classification=validation_failed
classification=cleanup_failed
```

External blockers such as missing credentials, inactive billing, quota, or
capacity are reported as classified blocked outcomes. If cleanup fails, use the
reported slug and local Crabbox claim to inspect the instance with
`crabbox list --provider lambda --json` or the Lambda console. The smoke script
redacts `LAMBDA_API_KEY`, user data, private key material, and Jupyter token/URL
fields from diagnostic output.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: yes through the standard Linux cloud-init path when a direct
  Tailscale auth key is configured.
- **Desktop / browser / code**: not advertised in this phase.
- **Cleanup**: yes, local-claim-owned or complete-tag-owned Lambda instances and
  owned Lambda SSH keys only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- Lambda is direct-only. Coordinator secrets, team scheduling, and cost
  accounting do not cover these instances.
- Running Lambda instances are billable. Use short TTLs, dry-run cleanup, and
  explicit live-smoke gates.
- `--type` must be a valid Lambda instance type such as `gpu_1x_a10`.
- `lambda.image` and `lambda.imageFamily` are mutually exclusive.
- Crabbox can pass an existing firewall ruleset and existing filesystems during
  launch, but it does not create or mutate those resources.
- Phase 1 does not advertise Jupyter, desktop, VNC, browser, or code-server
  surfaces even if a selected Lambda image includes provider-specific tooling.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
- [Operations](../operations.md)
