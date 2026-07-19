# unshare

`crabbox unshare` removes sharing rules from a coordinator-backed lease. It is
the inverse of [`crabbox share`](share.md): use it to revoke access you granted
to individual users, to the lease's org, or to clear every rule at once.

Sharing lives on the broker, so `unshare` requires a configured coordinator. You
must be the lease owner, hold a `manage` share on the lease, or use an admin
session to change its sharing.

## Usage

```sh
crabbox unshare --id swift-crab --user github:12345
crabbox unshare --id swift-crab --org
crabbox unshare --id swift-crab --all
crabbox unshare swift-crab --all --json
```

The lease is identified by `--id` (its `cbx_…` id or slug). As a convenience the
first positional argument is treated as the id, so `crabbox unshare swift-crab`
is equivalent to `--id swift-crab`.

You must pass at least one of `--user`, `--org`, or `--all`; otherwise the
command exits with a usage error.

## What each flag removes

- `--user <owner>` revokes one owner's access. Repeat the flag to remove several
  users in a single call. Use `github:<numeric-id>` for GitHub users; values are
  matched case-insensitively.
- `--org` removes org-wide access, so the lease is no longer shared with everyone
  in the owning org.
- `--all` clears every sharing rule (all users and any org grant) in one step.

When `--all` is set the other selectors are ignored. Otherwise `--user` and
`--org` can be combined to remove specific users and the org grant together.

## Output

`unshare` prints the resulting sharing state after the change. The text form
lists the org grant followed by each remaining user:

```text
org=off
user=github:67890 role=use
```

`role` is `use` (run on the lease) or `manage` (run plus change sharing).
`org=off` means no org-wide grant remains, and `users=none` is printed when no
individual shares are left. Pass `--json` to emit the same state as a
`{"share": …}` object for scripting.

## Flags

```text
--id <lease-id-or-slug>   Lease to modify (or pass as the first positional arg).
--user <owner>            Owner identity to remove; repeatable.
--org                     Remove the org-wide grant.
--all                     Remove every sharing rule.
--json                    Print the resulting share state as JSON.
```

## Related docs

- [share](share.md)
- [Auth and admin](../features/auth-admin.md)
- [Browser portal](../features/portal.md)
