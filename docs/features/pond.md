# Pond

A small group of crabbox boxes connected on a private network, addressable by name, that live and die together. Same idea as a tidepool — a contained body of water where a few creatures coexist for a while, then it dries up.

A `--pond` of one is the default; existing single-box flows are unchanged.

## Usage

```
crabbox warmup --pond NAME --slug ROLE --provider PROVIDER ...
crabbox run    --id LEASE_ID -- COMMAND
crabbox list   --pond NAME
crabbox doctor --pond NAME
crabbox pond peers   --pond NAME
crabbox pond connect NAME [--export]
crabbox pond release NAME
```

Each lease in a pond is addressable by its `--slug` from any other member of the same pond.

## Three transport planes

Each provider self-declares which planes it supports via `Spec().Features`
(`FeatureTailscale`, `FeatureSSH`, `FeatureURLBridge`). Most providers
declare more than one — a Hetzner box advertises both Tailscale and SSH,
so it is reachable via either the peer mesh or `pond connect`; an Islo
sandbox advertises URL Bridge, etc. The CLI verbs opportunistically use
whichever plane best fits the call site.

| Plane     | Feature flag       | Providers (today)                                                                                                                                                    | What you get                                                  |
| --------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| Tailscale | `FeatureTailscale` | Hetzner, Azure, GCP                                                                                                                                                  | true peer-to-peer mesh, `<slug>.cbx` DNS                      |
| Bridge    | `FeatureURLBridge` | Islo, E2B, Railway (live adapters); Modal, Cloudflare, Tensorlake (report `unsupported` until adapters ship)                                                         | HTTPS endpoints between pond members                          |
| SSH-mesh  | `FeatureSSH`       | **any provider advertising SSH**: Hetzner, Azure, GCP, AWS, Proxmox, static SSH, RunPod, exe-dev, Daytona, Sprites, Namespace, Semaphore, local-container, Parallels | operator-side `ssh -L` tunnels via `pond connect [--export]` |
| (gap)     | —                  | macOS sandboxes, Windows                                                                                                                                             | not yet covered                                               |

`crabbox pond peers` returns *both* a primary `transport` hint and the
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
available; SSH-mesh becomes a universal fallback that works wherever the
provider exposes SSH.

## Three simple use cases

1. **Per-PR isolated E2E env.** Every PR gets its own staging; pond dies with PR:
   ```
   crabbox warmup --pond pr-$PR --slug api/web/db --provider hetzner --tailscale
   # ... E2E ...
   crabbox pond release pr-$PR
   ```

2. **API + GPU + DB integration test.** Vendor-mix in 4 lines — CPU on Hetzner, GPU on Modal, DB on Hetzner — talking by name:
   ```
   crabbox warmup --pond it-$SHA --slug api --provider hetzner --tailscale
   crabbox warmup --pond it-$SHA --slug ml  --provider modal   --class a10g
   crabbox warmup --pond it-$SHA --slug db  --provider hetzner --tailscale
   crabbox run --id it-$SHA-api -- "ML_HOST=ml.cbx DB_HOST=db.cbx go test ./..."
   ```

3. **Per-PR build farm.** 30-helper Islo pond per PR, dies with the PR — useful for slow C++/Bazel/Rust builds:
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

## Security

Setting `TS_API_KEY` in your shell empowers `crabbox run` to mutate your operator's Tailscale ACL policy (the auto-bootstrap path). The broker never sees Tailscale credentials.

## API stability

`pond` is **preview** for v0.x. The reserved `pond=` label key stays; the flag shape may evolve. Stable from v1.0.
