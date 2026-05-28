# Pond

A preview way to group related Crabbox leases, discover their reachability
metadata, and release them together. A pond is not a central cluster object: it
is an emergent set of active leases tagged with the reserved `pond=<name>`
metadata plus local claim sidecars for providers that do not own cloud labels.

Some pond members can reach each other directly by name, but only on the
transport plane that supports that. Tailscale gives true peer-to-peer
`<slug>.cbx` reachability; URL bridge gives HTTP(S) endpoints; SSH-mesh gives
operator-side `ssh -L` forwards.

A `--pond` of one is the default; existing single-box flows are unchanged.

## Usage

```
crabbox warmup --pond NAME --slug ROLE --provider PROVIDER ...
crabbox run    --id LEASE_ID -- COMMAND
crabbox list   --pond NAME
crabbox doctor --pond NAME
crabbox pond peers   --pond NAME
crabbox pond connect NAME [--export]
crabbox pond disconnect NAME
crabbox pond release NAME
```

Use `--slug` as the stable role name for discovery. Whether that slug is
directly dialable from another lease depends on the transport plane below.

The plugin surface does not add first-class pond tools in this PR. Agents can
still create pond-tagged leases through existing argv-forwarding tools;
`pond peers`, `pond connect`, `pond disconnect`, `pond release`, and Tailscale
policy bootstrap remain CLI-led.

## Three transport planes

Each provider self-declares which planes it supports via `Spec().Features`
(`FeatureTailscale`, `FeatureSSH`, `FeatureURLBridge`). Some providers declare
more than one: a direct Hetzner box advertises both Tailscale and SSH, so
Tailscale is the preferred peer mesh and `pond connect` can still build
operator-side SSH forwards for declared ports. URL-only sandboxes, such as Islo
and E2B, do not join the peer mesh; they surface HTTP(S) endpoints instead.

Auto-generated `tag:cbx-pond-*` Tailscale tags are direct-provider only in this
preview. Brokered coordinators keep using their configured
`CRABBOX_TAILSCALE_TAGS` allowlist; they do not receive dynamic per-pond tags
until the Worker owns an explicit pond tag policy.

| Plane     | Feature flag       | Providers (today)                                                                                                                                                    | What you get                                                  |
| --------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| Tailscale | `FeatureTailscale` | Hetzner, Azure, GCP                                                                                                                                                  | true peer-to-peer mesh, `<slug>.cbx` DNS                      |
| Bridge    | `FeatureURLBridge` | Islo, E2B, Railway (live adapters); Modal, Cloudflare, Tensorlake (report `unsupported` until adapters ship)                                                         | provider-native HTTP(S) endpoints for discovery and sharing   |
| SSH-mesh  | `FeatureSSH`       | **any provider advertising SSH**: Hetzner, Azure, GCP, AWS, Proxmox, static SSH, RunPod, exe-dev, Daytona, Sprites, Namespace, Semaphore, local-container, Parallels | operator-side `ssh -L` tunnels via `pond connect [--export]` |
| (gap)     | —                  | macOS sandboxes, Windows                                                                                                                                             | not yet covered                                               |

`crabbox pond peers` returns locally known peers with *both* a primary `transport` hint and the
full `transports` list per member:

```jsonc
{ "slug": "api",  "provider": "hetzner",
  "transport":  "tailnet",            // primary / recommended
  "transports": ["tailnet", "ssh"],   // every plane this provider supports
  "endpoint":   "100.64.1.3" }
```

So `pond connect` works against any provider that includes `ssh` in its
`transports` list — including Hetzner / Azure / GCP / AWS, not just the
old SSH-only class. Tailscale stays the recommended path when it's also
available; SSH-mesh is an explicit operator-side plane for providers that
advertise SSH. `pond connect --export` daemon mode is macOS/Linux-only in this
preview because cleanup validates local daemon process commands before stopping
them; Windows operators can use foreground `pond connect` until a Windows
process validator ships.

## Three simple use cases

1. **Per-PR isolated E2E env.** Every PR gets its own staging; pond dies with PR:
   ```
   crabbox warmup --pond pr-$PR --slug api/web/db --provider hetzner --tailscale
   # ... E2E ...
   crabbox pond release pr-$PR
   ```

2. **API + GPU + DB integration test.** Vendor-mix in 4 lines — CPU on Hetzner,
   GPU on a delegated provider, DB on Hetzner. Use Tailscale names for
   tailnet members, URL bridge for HTTP(S) peers, and `pond connect` for
   operator-side TCP forwards:
   ```
   crabbox warmup --pond it-$SHA --slug api --provider hetzner --tailscale
   crabbox warmup --pond it-$SHA --slug ml  --provider modal   --class a10g
   crabbox warmup --pond it-$SHA --slug db  --provider hetzner --tailscale
   crabbox run --id it-$SHA-api -- "DB_HOST=db.cbx go test ./..."
   ```

3. **Per-PR build farm.** 30-helper delegated pond per PR, dies with the PR —
   useful for latency-tolerant work queues. This is not an MPI/NCCL-style
   low-latency fabric:
   ```
   crabbox warmup --pond build-$PR --slug coord --provider hetzner
   for i in $(seq 1 30); do
     crabbox warmup --pond build-$PR --slug helper-$i --provider islo &
   done; wait
   ```

## When not to use it

- **Tightly-coupled HPC** (MPI / NCCL) across providers — public-internet latency is too high. Keep tightly-coupled jobs on one provider + region.
- **macOS / Windows peer reachability** — gap, tracked separately.
- **Untrusted multi-tenant** — default-allow within a pond. For agent-isolation cases, see `--isolation per-slug` (future).

## Pool vs pond

`pool` is inventory: "what runners exist?" It is exposed through `crabbox list`
and `/v1/pool`.

`pond` is grouping: "which leases belong to this test environment?" It is
created by tagging leases with `--pond <name>` and operated through
`crabbox pond ...`.

## Security

Tailscale ACL bootstrap is opt-in. To let `crabbox run --pond ... --tailscale`
add the concrete `tag:cbx-pond-<owner>-<pond>` policy rows, set both
`TS_API_KEY` and `CRABBOX_POND_ACL_BOOTSTRAP=1`. Without the opt-in, Crabbox
prints/verifies the required policy through `doctor --pond` but does not edit
the tailnet policy. The broker never sees Tailscale credentials.

## API stability

`pond` is **preview** for v0.x. The reserved `pond=` label key is intended to
stay, but metadata shape and command flags may evolve before v1.0.
