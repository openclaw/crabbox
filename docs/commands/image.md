# image

`crabbox image` contains trusted operator controls for runner images on AWS
and Hetzner Cloud.

```sh
crabbox image create --id cbx_... --name openclaw-crabbox-20260501-1246 --wait
crabbox image promote ami-...
crabbox image promote 382206402
```

Image commands require a configured coordinator and shared-token admin auth.
They are intentionally not available to normal GitHub browser-login users.

## create

Create an AMI (AWS) or snapshot (Hetzner) from an active lease. The provider
is inferred from the lease record.

Flags:

```text
--id <cbx_id>        source lease (AWS or Hetzner)
--name <name>        image name (AWS) or snapshot description (Hetzner)
--wait               poll until the image is available
--wait-timeout <d>   default 45m
--no-reboot          AWS only; default true
--json               print JSON
```

For AWS the Worker calls EC2 `CreateImage`. For Hetzner the Worker calls the
`create_image` server action with `type: "snapshot"`.

Hetzner snapshots taken from a running server include only data that has been
flushed to disk. Run `sync; sync` (or `fsfreeze -f /` followed by `fsfreeze
-u /` for non-ext4 filesystems) over SSH on the lease before creating the
image, otherwise recently written files may be missing or corrupt in the
restored snapshot.

## promote

Promote an available image as the coordinator's default for its provider:

```sh
crabbox image promote ami-1234567890abcdef0      # AWS
crabbox image promote 382206402                  # Hetzner snapshot id
```

Future brokered leases use the promoted image when the request omits an
explicit `awsAMI` / `image` (and `CRABBOX_AWS_AMI` is unset for AWS).
Promotion stores coordinator metadata only; it does not copy or modify the
underlying image.

Related docs:

- [Infrastructure](../infrastructure.md)
- [Runner bootstrap](../features/runner-bootstrap.md)
