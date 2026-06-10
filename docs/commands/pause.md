# pause

`crabbox pause` pauses a single lease, freeing the remote compute while
preserving the sandbox's state, so it can be brought back later with
[`resume`](resume.md). It is provider-dependent: only providers whose backend
implements the pausable capability accept it; others return
`provider=<name> does not support pause`.

```sh
crabbox pause swift-crab
crabbox pause --id isb_crabbox-repo-0a1b2c
crabbox pause --provider islo swift-crab
```

## Identifying the lease

Pass the lease as a positional argument or with `--id`; both accept the
canonical ID or an active friendly slug (see
[Identifiers](../features/identifiers.md)). Supplying both `--id` and a
positional argument, or more than one positional argument, is an error. The
local claim is kept so the lease can be resumed.

## Provider support

- `islo` — snapshots the sandbox to disk and frees its CPU/memory via the Islo
  pause API; resume restores it. Accepts an `isb_...` ID, a Crabbox-created
  sandbox name, or a local slug.

Run `crabbox providers` for the providers available in your build.

## See also

- [`resume`](resume.md) — resume a paused lease.
- [`stop`](stop.md) — tear a lease down entirely.
