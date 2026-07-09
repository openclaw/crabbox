# image

`crabbox image` holds the trusted-operator controls for provider base images:
creating runner images, promoting an AWS AMI as the brokered default, inspecting
Fast Snapshot Restore, and deleting stale images. Use it for shared base images
and explicit image cleanup, not for per-scenario state. To save one prepared
lease and fork that exact scenario later, use
[`crabbox checkpoint`](checkpoint.md).

```sh
crabbox image create --id cbx_... --name my-runner-20260501-1246 --wait
crabbox image promote ami-1234567890abcdef0
crabbox image promote ami-1234567890abcdef0 --target macos --region us-east-1 --type mac2.metal
crabbox image fsr-status ami-1234567890abcdef0 --region us-west-2 --fsr-az us-west-2a
crabbox image delete ami-1234567890abcdef0 --region eu-west-1
crabbox image delete my-managed-image --provider azure --region westeurope
crabbox image delete my-machine-image --provider gcp --region europe-west1-b --project example-project
```

Every `image` subcommand requires a configured coordinator (broker) **and**
admin-token auth. Set `broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN`
locally; the Worker validates it against `CRABBOX_ADMIN_TOKEN`. Without an admin
token the command exits early with `admin command requires broker.adminToken or
CRABBOX_COORDINATOR_ADMIN_TOKEN`. These commands are intentionally unavailable
to normal GitHub browser-login users.

Image bytes live in the provider account, never in git or coordinator durable
state. AWS images are AMIs backed by EBS snapshots; Azure images are managed
images; GCP images are Compute Engine machine images. Crabbox stores promoted
AWS AMI metadata per target, architecture, and region so future brokered AWS
leases can resolve a matching default. Hetzner snapshots/images live in the
Hetzner project and are selected through `image`/`CRABBOX_HETZNER_IMAGE`;
Crabbox has no Hetzner create/promote lifecycle commands yet.

## create

Create a provider image from an active brokered lease.

```sh
crabbox image create --id cbx_abc123def456 --name my-runner-20260501-1246 --wait
```

The source lease must still be active in the coordinator. The Worker calls the
provider image API against the backing cloud VM and tags the image as
Crabbox-owned. AWS uses the `CreateImage` API; the same flow applies to other
brokered providers that support image capture. Note that Azure managed-image
capture requires a stopped, generalized source VM, so for active Azure leases
prefer [`crabbox checkpoint`](checkpoint.md) disk snapshots instead.

Flags:

```text
--id <cbx_id>        source lease (canonical lease ID, required)
--name <name>        provider image name (required)
--wait               poll until the provider image is available (default false)
--wait-timeout <d>   maximum wait duration (default 45m)
--no-reboot          AWS CreateImage NoReboot (default true)
--json               print the image record as JSON
```

Without `--json`, output is a single line:

```text
image=ami-... name=my-runner-20260501-1246 state=available region=eu-west-1
```

Recommended bake flow — warm a fresh lease, smoke-test it, then capture:

```sh
crabbox warmup --provider aws --class standard --ttl 2h --idle-timeout 30m
crabbox run --id <slug> --shell -- 'command -v ssh git rsync curl jq && test -d /work/crabbox'
crabbox image create --id cbx_... --name my-runner-YYYYMMDD-HHMM --wait
```

Use a fresh, intentionally warmed lease as the source. Do not bake personal
workspace state, local secrets, repository checkouts, or one-off debugging
artifacts into the image. For desktop/browser images, follow the full
[Image bake runbook](../features/image-bake-runbook.md) rather than relying on
the short smoke above.

Failure handling:

- If `--wait` times out, re-run `crabbox image create ... --json` or inspect the
  provider image state before retrying. Provider image creation can continue
  after the CLI stops polling.
- If the image enters a failed state, leave the current promoted image in place
  and create a new image from a fresh lease.
- If the source lease disappears, create a new warm lease and restart the bake;
  image creation requires the backing cloud VM.
- If the baked image boots but never reaches `crabbox-ready`, do not promote it.
  Keep the previous promoted AMI and debug bootstrap on a normal lease first.
- Promotion does not delete old images or snapshots. Cleanup of stale candidate
  images is an operator task; use `crabbox image delete`.

## promote

Promote an available AMI as the coordinator's default AWS image. Promotion is
AWS-only.

```sh
crabbox image promote ami-1234567890abcdef0
```

Flags:

```text
--target <name>          linux, macos, or windows
--os <selector>          portable Linux OS selector for Linux AMIs (ubuntu:26.04 | ubuntu:24.04)
--region <name>          AWS region containing the AMI
--type <instance-type>   instance type the AMI boots on, for example mac2.metal
--server-type <type>     alias for --type
--architecture <arch>    AWS AMI architecture, for example x86_64_mac or arm64_mac
--os-version <version>   numeric OS version present in the image
--sdk <name=version>     SDK present in the image (repeatable)
--runtime <name=version> runtime present in the image (repeatable)
--browser                image includes browser support
--webview2               image includes Microsoft WebView2
--desktop                image includes desktop support
--fast-snapshot-restore  enable AWS Fast Snapshot Restore for the backing snapshots
--fsr-az <az>            availability zone for Fast Snapshot Restore (repeatable)
--json                   print the promoted image record as JSON
```

Add `--target` and `--region` when promoting an AMI that was not created through
`crabbox image create`; images created by Crabbox inherit target and region
metadata from their source lease. For external macOS AMIs, Crabbox reads the
architecture from AWS but also accepts `--type` or `--architecture` to pin the
promotion metadata explicitly. Use `--os` to record the portable Linux selector
for a promoted Linux AMI.

Capability declarations make the AMI eligible for capability-aware selection.
Versions use numeric dot notation, for example `--os-version 15.5`,
`--sdk xcode=16.4`, or `--runtime node=24.2`. Omitted capabilities remain
unknown and do not satisfy an explicit image requirement.

Add `--fast-snapshot-restore` plus one or more `--fsr-az` values when the
promoted image backs hot lanes that need immediate EBS snapshot reads:

```sh
crabbox image promote \
  --target windows \
  --region us-west-2 \
  --fast-snapshot-restore \
  --fsr-az us-west-2a \
  --fsr-az us-west-2b \
  ami-1234567890abcdef0
```

Fast Snapshot Restore is provider-billed per snapshot and availability zone. Use
it for known hot zones, not every candidate bake.

Future brokered AWS leases use the promoted image when the request does not set
an explicit `awsAMI`/`CRABBOX_AWS_AMI` override. Promotion stores coordinator
metadata only; it does not copy or modify the AMI. A macOS promotion is scoped
to matching macOS leases and never becomes the Linux or Windows default.
Crabbox retains a scoped catalog of promoted AMIs so a lease can select the
newest image satisfying every requested image capability, not only the last
promoted default.

Promote, smoke-test, and roll back if needed:

```sh
crabbox image promote ami-new
crabbox warmup --provider aws --class standard --ttl 20m --idle-timeout 6m
crabbox run --id <slug> --shell -- 'echo image-smoke-ok && uname -srm && test -d /work/crabbox'
crabbox stop <slug>
```

For macOS:

```sh
crabbox image promote ami-new --target macos --region us-east-1 --type mac2.metal
crabbox warmup --provider aws --target macos --type mac2.metal --market on-demand --ttl 30m
crabbox run --id <slug> --shell -- 'echo image-smoke-ok && sw_vers && test -d "$HOME/crabbox"'
crabbox stop <slug>
```

If the smoke fails, promote the previous known-good AMI again. The coordinator
updates the scoped default and retains each promotion in the capability catalog,
so rollback is another `image promote` for the same target and region. Keep the
previous AMI available until at least one brokered AWS smoke succeeds on the new
image.

## fsr-status

Show the live AWS Fast Snapshot Restore state for an image's backing snapshots.
This subcommand is AWS-only.

```sh
crabbox image fsr-status ami-1234567890abcdef0 --region us-west-2 --fsr-az us-west-2a
```

Flags:

```text
--provider <name>   image provider; aws only (default aws)
--region <name>     AWS region containing the AMI or snapshot
--fsr-az <az>       availability zone to report; repeatable
--json              print the image record as JSON
```

The argument may be an AMI ID or a snapshot ID. Omit `--fsr-az` to return every
Fast Snapshot Restore record AWS reports for the image snapshots. Output lists a
summary line plus one line per snapshot/AZ pair (or `fsr none` when there are no
records):

```text
image=ami-... state=available region=us-west-2 fsr=2
fsr snapshot=snap-... az=us-west-2a state=enabled reason=-
```

## delete

Delete a Crabbox-created provider image.

```sh
crabbox image delete ami-1234567890abcdef0 --region eu-west-1
crabbox image delete my-managed-image --provider azure --region westeurope
crabbox image delete my-machine-image --provider gcp --region europe-west1-b --project example-project
```

Flags:

```text
--provider <name>   image provider: aws, azure, or gcp (default aws)
--region <name>     region, location, or zone containing the image
--project <name>    GCP project containing the image
```

AWS deletion deregisters the AMI and then deletes the EBS snapshots referenced by
its block-device mappings. Azure deletion removes the managed image or disk
snapshot. GCP deletion removes the machine image or disk snapshot. Any other
`--provider` value is rejected.

Deletion fails closed unless the coordinator has stored Crabbox-created image
metadata for the target. This protects unrelated provider images and snapshots
that happen to live in the same cloud account, resource group, or project.

## Related docs

- [Image bake runbook](../features/image-bake-runbook.md)
- [Prebaked runner images](../features/prebaked-images.md)
- [Infrastructure](../infrastructure.md)
- [Runner bootstrap](../features/runner-bootstrap.md)
