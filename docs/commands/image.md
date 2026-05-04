# image

`crabbox image` contains trusted operator controls for AWS runner images.

```sh
crabbox image create --id cbx_... --name openclaw-crabbox-20260501-1246 --wait
crabbox image promote ami-...
crabbox image promote ami-... --json
```

Image commands require a configured coordinator and admin-token auth. Set
`broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN` locally; the Worker
checks `CRABBOX_ADMIN_TOKEN`.
They are intentionally not available to normal GitHub browser-login users.

## create

Create an AWS AMI from an active AWS lease.

Flags:

```text
--id <cbx_id>        source lease; must be a canonical AWS lease ID
--name <name>        AMI name
--wait               poll until the AMI is available
--wait-timeout <d>   default 45m
--no-reboot          default true
--json               print JSON
```

The source lease must still be active in the coordinator. The Worker calls AWS
`CreateImage` from the backing instance ID and tags the image as Crabbox-owned.

Recommended bake flow:

```sh
crabbox warmup --provider aws --class standard --ttl 2h --idle-timeout 30m
crabbox run --id <slug> --shell -- 'command -v ssh git rsync curl jq && test -d /work/crabbox'
crabbox image create --id <cbx_id> --name openclaw-crabbox-YYYYMMDD-HHMM --wait
```

Use a fresh, intentionally warmed lease as the source. Do not bake personal
workspace state, local secrets, repository checkouts, or one-off debugging
artifacts into the image.

Failure handling:

- If `--wait` times out, run `crabbox image create ... --json` or inspect the
  AWS AMI state before retrying. AWS image creation can continue after the CLI
  stops polling.
- If the AMI enters a failed state, leave the current promoted image in place
  and create a new image from a fresh lease.
- If the source lease disappears, create a new warm lease and restart the bake;
  image creation requires the backing AWS instance ID.
- If the baked image boots but never reaches `crabbox-ready`, do not promote it.
  Keep the previous promoted AMI and debug bootstrap on a normal lease first.
- Cleanup of stale candidate AMIs is an AWS operator task. Promotion does not
  delete old images or snapshots.

## promote

Promote an available AMI as the coordinator's default AWS image:

```sh
crabbox image promote ami-1234567890abcdef0
```

Add `--json` to print the promoted image record for automation.

Future brokered AWS leases use the promoted image when the request does not set
an explicit `awsAMI` or `CRABBOX_AWS_AMI` override. Promotion stores coordinator
metadata only; it does not copy or modify the AMI.

Promotion and rollback:

```sh
crabbox image promote ami-new
crabbox warmup --provider aws --class standard --ttl 20m --idle-timeout 6m
crabbox run --id <slug> --shell -- 'echo image-smoke-ok && uname -srm && test -d /work/crabbox'
crabbox stop <slug>
```

If the smoke fails, promote the previous known-good AMI again. The coordinator
stores only the selected AMI ID, so rollback is another `image promote` call.
Keep the previous AMI available until at least one brokered AWS smoke succeeds
on the new image.

Related docs:

- [Infrastructure](../infrastructure.md)
- [Runner bootstrap](../features/runner-bootstrap.md)
