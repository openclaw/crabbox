# image

`crabbox image` contains trusted operator controls for provider images. Use it
for base runner images and explicit image cleanup, not per-scenario checkpoints.
If you want to save one prepared lease and fork that exact scenario later, use
[`crabbox checkpoint`](checkpoint.md).

```sh
crabbox image create --id cbx_... --name crabbox-runner-20260501-1246 --wait
crabbox image promote ami-...
crabbox image promote ami-... --target macos --region us-east-1 --type mac1.metal
crabbox image promote ami-... --json
crabbox image delete ami-... --region eu-west-1
crabbox image delete my-azure-image --provider azure --region westeurope
crabbox image delete my-gcp-image --provider gcp --region europe-west1-b --project example-project
```

Image commands require a configured coordinator and admin-token auth. Set
`broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN` locally; the Worker
checks `CRABBOX_ADMIN_TOKEN`.
They are intentionally not available to normal GitHub browser-login users.

Image bytes live in the provider account, not in git or coordinator durable
state. AWS images are AMIs backed by EBS snapshots. Azure images are managed
images. GCP images are Compute Engine machine images. Crabbox stores promoted
AWS AMI metadata per target, architecture, and region so future AWS leases can
resolve a matching default image. Hetzner snapshots/images should live in the
Hetzner project and be selected through `image`/`CRABBOX_HETZNER_IMAGE` until
Crabbox grows Hetzner create/promote lifecycle commands.

An AMI is AWS's bootable machine image format. EBS snapshots are the stored disk
snapshots that back the AMI. Deleting an AWS candidate image should remove both
the AMI registration and its EBS snapshots.

## create

Create a provider image from an active brokered AWS or GCP lease. Active Azure
leases use `crabbox checkpoint` disk snapshots; Azure managed-image capture
requires a stopped/generalized source VM.

Flags:

```text
--id <cbx_id>        source lease; must be a canonical lease ID
--name <name>        provider image name
--wait               poll until the provider image is available
--wait-timeout <d>   default 45m
--no-reboot          AWS AMI CreateImage NoReboot, default true
--json               print JSON
```

The source lease must still be active in the coordinator. The Worker calls the
provider image API from the backing cloud VM id and tags the image as
Crabbox-owned.

Recommended bake flow:

```sh
crabbox warmup --provider aws --class standard --ttl 2h --idle-timeout 30m
crabbox run --id <slug> --shell -- 'command -v ssh git rsync curl jq && test -d /work/crabbox'
crabbox image create --id <cbx_id> --name crabbox-runner-YYYYMMDD-HHMM --wait
```

Use a fresh, intentionally warmed lease as the source. Do not bake personal
workspace state, local secrets, repository checkouts, or one-off debugging
artifacts into the image.
For desktop/browser images, follow the full [Image bake runbook](../features/image-bake-runbook.md)
instead of relying only on the short smoke above.

Failure handling:

- If `--wait` times out, run `crabbox image create ... --json` or inspect the
  provider image state before retrying. Provider image creation can continue
  after the CLI stops polling.
- If the image enters a failed state, leave the current promoted AWS image in
  place and create a new image from a fresh lease.
- If the source lease disappears, create a new warm lease and restart the bake;
  image creation requires the backing cloud VM ID.
- If the baked image boots but never reaches `crabbox-ready`, do not promote it.
  Keep the previous promoted AMI and debug bootstrap on a normal lease first.
- Cleanup of stale candidate images is an operator task. Promotion does not
  delete old images or snapshots. Use `crabbox image delete` for explicit
  cleanup.
- If a timing report does not improve after promotion, treat that as a failed
  performance bake even if the AMI boots.

## promote

Promote an available AMI as the coordinator's default AWS image:

```sh
crabbox image promote ami-1234567890abcdef0
```

Flags:

```text
--target <name>          linux, macos, or windows when promoting an existing AMI
--region <name>          AWS region for AMI lookup when promoting an existing AMI
--type <instance-type>   instance type the AMI boots on, for example mac1.metal
--server-type <type>     alias for --type
--architecture <arch>    AWS AMI architecture, for example x86_64_mac or arm64_mac
--json                   print JSON
```

Add `--target` and `--region` when promoting an AMI that was not created through
`crabbox image create`; created images inherit target and region metadata from
their source lease. For external macOS AMIs, Crabbox reads the AMI architecture
from AWS and also accepts `--type` or `--architecture` when you want to pin the
promotion metadata explicitly. Add `--json` to print the promoted image record
for automation.

Future brokered AWS leases use the promoted image when the request does not set
an explicit `awsAMI` or `CRABBOX_AWS_AMI` override. Promotion stores coordinator
metadata only; it does not copy or modify the AMI. A macOS promotion is only
used by matching macOS leases and will not become the Linux or Windows default.

Promotion and rollback:

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
stores only scoped selected AMI IDs, so rollback is another `image promote`
call for the same target and region. Keep the previous AMI available until at
least one brokered AWS smoke succeeds on the new image.

## delete

Delete a provider image:

```sh
crabbox image delete ami-1234567890abcdef0 --region eu-west-1
crabbox image delete my-managed-image --provider azure --region westeurope
crabbox image delete my-machine-image --provider gcp --region europe-west1-b --project example-project
```

AWS deletion deregisters the AMI, then deletes the EBS snapshots referenced by
its block device mappings. Azure deletion removes the managed image. GCP
deletion removes the machine image. It requires admin-token auth.

Related docs:

- [Image bake runbook](../features/image-bake-runbook.md)
- [Prebaked runner images](../features/prebaked-images.md)
- [Infrastructure](../infrastructure.md)
- [Runner bootstrap](../features/runner-bootstrap.md)
