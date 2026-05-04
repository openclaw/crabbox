# admin

`crabbox admin` contains trusted operator controls for coordinator-backed leases.

```sh
crabbox admin leases
crabbox admin leases --state active --json
crabbox admin release blue-lobster
crabbox admin release blue-lobster --delete
crabbox admin delete cbx_... --force
```

Release/delete accept a canonical `cbx_...` ID or an active slug; use the canonical ID when an admin slug lookup is ambiguous. Add `--json` to print the updated lease record.

Admin commands require a configured coordinator and a separate admin bearer token
stored as `broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN`. The shared
operator token is not enough for admin routes.

## leases

List coordinator lease records.

Flags:

```text
--state <state>     filter by active, released, expired, or failed
--owner <email>     filter by owner
--org <name>        filter by org
--limit <n>         default 100, maximum 500
--json              print JSON
```

## release

Mark a lease released. Add `--delete` to delete the backing server while releasing.

Flags:

```text
--id <lease-id-or-slug>
--delete
--json
```

## delete

Delete the backing server for an active lease and mark it released. Requires `--force`.

Flags:

```text
--id <lease-id-or-slug>
--force
--json
```

Related docs:

- [Operations](../operations.md)
- [Auth and admin](../features/auth-admin.md)
