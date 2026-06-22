# share

`crabbox share` grants other people access to an existing lease through the
coordinator. It manages who may see a lease and use its portal bridges; it does
not move SSH private keys between machines.

Sharing requires a configured coordinator. Without one, the command exits with
`share requires a configured coordinator`.

Direct-provider leases become shareable when `broker.mode: registered` is set.
The coordinator shares portal capabilities such as WebVNC; it does not gain
provider lifecycle ownership or the local SSH key.

## Usage

```sh
# Share with a specific user (defaults to role "use")
crabbox share --id swift-crab --user alice@example.com

# Grant manage access
crabbox share --id swift-crab --user alice@example.com --role manage

# Share with everyone in the lease's org
crabbox share --id swift-crab --org
crabbox share --id swift-crab --org --role manage

# Show the current sharing for a lease
crabbox share --id swift-crab --list
crabbox share swift-crab --list --json
```

The lease can be addressed by its canonical id (`cbx_…`) or its slug, either via
`--id` or as the first positional argument.

When you pass neither `--user` nor `--org` (or pass `--list`), the command prints
the current sharing instead of changing it.

Only the lease owner, an admin, or a user with `manage` access can list or change
sharing. A user with `use` access can see the lease but cannot enumerate its
sharing roster.

## Roles

```text
use     see the lease and use visible portal bridges such as WebVNC and code
manage  use access plus changing sharing and stopping the lease
```

A role applies to every `--user` and to `--org` named in the same invocation.
`--role` defaults to `use`.

## Targets

- `--user <email>` is repeatable. Addresses are stored normalized to lowercase
  and trimmed; an empty value is rejected.
- `--org` shares with authenticated users whose org matches the lease's org.

## Output

Without `--json`, the resulting share state prints one line per scope:

```text
org=use
user=alice@example.com role=use
```

`org` is `off` when no org sharing is set, and `users=none` when no users are
shared. With `--json`, the share record is emitted under a `share` key.

## Flags

```text
--id <lease-id-or-slug>   lease to share (or first positional arg)
--user <email>            user email to share with; repeatable
--org                     share with the lease org
--role use|manage         role to grant (default: use)
--list                    print current sharing without changing it
--json                    print JSON
```

## Notes

Sharing grants coordinator and portal access only. SSH-based commands still
require a local private key the runner accepts; sharing does not copy SSH private
keys between people.

## Related docs

- [unshare](unshare.md)
- [Auth and admin](../features/auth-admin.md)
- [Browser portal](../features/portal.md)
