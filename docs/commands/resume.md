# resume

`crabbox resume` resumes a lease previously paused with [`pause`](pause.md),
restoring it to a running state. It is provider-dependent: only providers whose
backend implements the pausable capability accept it; others return
`provider=<name> does not support resume`.

```sh
crabbox resume swift-crab
crabbox resume --id isb_crabbox-repo-0a1b2c
crabbox resume --provider islo swift-crab
```

## Identifying the lease

Pass the lease as a positional argument or with `--id`; both accept the
canonical ID or an active friendly slug (see
[Identifiers](../features/identifiers.md)). Supplying both `--id` and a
positional argument, or more than one positional argument, is an error.

## Provider support

- `islo` — restores a paused sandbox to running via the Islo resume API.

Run `crabbox providers` for the providers available in your build.

## See also

- [`pause`](pause.md) — pause a lease, freeing remote compute.
- [`status`](status.md) — check whether a lease is running again.
