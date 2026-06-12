# Pond

A pond is a lightweight way to group related leases, discover how to reach each
one, and release them together. It is not a central cluster object: a pond is an
emergent set of active leases that share the reserved `pond=<name>` provider
label, plus local claim sidecars for providers that do not own cloud labels. A
pond exists for as long as at least one active lease carries the label.

Reachability between pond members depends on the transport plane each member's
provider supports. Tailscale gives tailnet membership and, on managed Linux VM
providers, OS-routed peer-to-peer `<slug>.cbx` names; the URL bridge gives
provider-native HTTP(S) endpoints; the SSH-mesh gives operator-side `ssh -L`
forwards. A pond can mix providers and planes.

A `--pond` of one is the default — single-box flows are unchanged.

> **Preview.** Pond is preview for v0.x. The reserved `pond=` label key is
> intended to stay, but metadata shape and command flags may evolve before v1.0.

## Quick start

```sh
# Lease a few members into one pond (slug = stable role name).
crabbox warmup --pond pr-42 --slug api --provider hetzner --tailscale
crabbox warmup --pond pr-42 --slug db  --provider hetzner --tailscale

# Discover peers, run work against a member, then tear the pond down.
crabbox pond peers   --pond pr-42
crabbox run --id api -- "DB_HOST=db.cbx go test ./..."
crabbox pond release pr-42
```

Use `--slug` as the stable role name; it is what shows up in discovery and in
`<slug>.cbx` names. Whether that slug is directly dialable from another member
depends on the transport plane (see below).

## Naming a pond

`--pond <name>` accepts any string and normalizes it: lowercased, characters
outside `[a-z0-9-]` collapsed to `-`, runs collapsed, leading/trailing dashes
trimmed. The normalized name must contain at least one letter or digit and be at
most 41 characters. The same name is reused everywhere the pond appears (the
label value, the Tailscale ACL tag, peer hostnames), so it stays in a regular
DNS-like identifier space.

## The three transport planes

Each provider self-declares which planes its leases can serve via its
`Spec().Features` (`FeatureTailscale`, `FeatureURLBridge`, `FeatureSSH`). A
single provider can advertise more than one — for example a direct Hetzner box
advertises both Tailscale and SSH, so Tailscale is the preferred peer mesh while
`pond connect` can still build operator-side SSH forwards. Islo is dual-plane: by
default it surfaces HTTP(S) endpoints, but warmed with `--tailscale` it joins the
tailnet through userspace `tailscaled` driven over the exec stream. E2B and other
URL-only sandboxes do not join the tailnet; they surface HTTP(S) endpoints
instead.

| Plane     | Feature flag       | Providers that advertise it (today)                                                                                       | What you get                                                |
| --------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| Tailscale | `FeatureTailscale` | Hetzner, Azure, GCP, Islo (userspace `tailscaled` via exec, opt-in with `--tailscale`)                                    | tailnet membership; OS-routed peer mesh on managed Linux, userspace proxy path on Islo |
| Bridge    | `FeatureURLBridge` | Islo, E2B, Railway                                                                                                      | provider-native HTTP(S) endpoints for discovery and sharing |
| SSH-mesh  | `FeatureSSH`       | any provider advertising SSH: Hetzner, Azure, GCP, AWS, Proxmox, static `ssh`, RunPod, exe-dev, Daytona, Sprites, Namespace, Semaphore, local-container, Parallels | operator-side `ssh -L` tunnels via `pond connect`           |

macOS and Windows peer reachability are not covered by any plane yet.

Capabilities are derived from each provider's `FeatureSet`, not from a static
table — a provider opts into a plane by declaring the feature. See
[providers.md](providers.md) for the full capability matrix and
[tailscale.md](tailscale.md) for the Tailscale transport.

## Commands

```sh
crabbox warmup --pond NAME --slug ROLE --provider PROVIDER [--tailscale] [--expose PORT]...
crabbox run    --id LEASE_OR_SLUG -- COMMAND
crabbox list   --pond NAME
crabbox doctor --pond NAME

crabbox pond peers      --pond NAME [--provider P] [--share-port PORT] [--share-ttl D] [--json]
crabbox pond connect    NAME [--provider P] [--export] [--json]
crabbox pond disconnect NAME
crabbox pond release    NAME
```

### `pond peers`

Lists every member of the pond, regardless of provider. `--pond` is required.
With no `--provider`, the resolver fans out across every provider represented in
the pond and concatenates the result; pass `--provider P` to restrict to one.

Each row carries a primary `transport` hint plus the full `transports` list of
every plane that member's provider supports:

```jsonc
// crabbox pond peers --pond pr-42 --json  ->  { "members": [ ... ] }
{
  "slug": "api",
  "leaseID": "cbx_0a1b2c3d4e5f",
  "provider": "hetzner",
  "pond": "pr-42",
  "transport":  "tailnet",          // primary / recommended plane
  "transports": ["tailnet", "ssh"], // every plane this provider supports
  "endpoint":   "100.64.1.3"
}
```

The endpoint shape depends on the primary plane: a tailnet IPv4/FQDN for
Tailscale members, `ssh://host:port` for SSH-lease members, and a per-port
HTTPS URL for URL-bridge members. Members whose endpoint is not yet recorded
surface with `transport: "pending"` and an honest note; providers with no
networking adapter (e.g. Blacksmith) surface with `transport: "none"`.

The bridge plane is HTTP-only by design. For URL-bridge providers you can
publish a per-peer public URL for a port:

```sh
crabbox pond peers --pond pr-42 --provider islo --share-port 8080 --share-ttl 12h --json
```

`--share-port` is idempotent (existing shares are reused), bounded to `1..65535`,
and `--share-ttl` defaults to 24h. Providers without a per-sandbox HTTPS ingress
report `unsupported` rather than pretending to bridge.

### `pond connect` / `pond disconnect`

`pond connect <name>` opens operator-side `ssh -L` forwards to every pond member
that declared `--expose` ports, across all SSH-mesh-capable providers in the
pond (not just one). `--provider P` narrows it to a single provider.

For each forwarded port it renders, under `~/.crabbox/pond/<name>/`:

- `hosts` — `<slug>.cbx` aliases pointing at `127.0.0.1`, with the assigned
  loopback port in a comment (a hosts file cannot encode `host:port`).
- `env` — shell exports of the form `CRABBOX_POND_<PEER>_<PORT>=127.0.0.1:<local>`,
  which is the supported way to reach a forwarded peer port.

Local forward ports are auto-allocated from `51820..52819` (probed against the
kernel so they do not collide with services you already run).

By default `pond connect` runs in the foreground and holds the tunnels open
until you press Ctrl-C. `--export` daemonizes the tunnels so they survive the
CLI exit and prints the exports for `eval`:

```sh
eval "$(crabbox pond connect pr-42 --export)"
curl "$CRABBOX_POND_API_8080"          # reach api's exposed :8080
crabbox pond disconnect pr-42          # stop the daemonized forwards
```

`--export` (and `pond disconnect`) are macOS/Linux only: cleanup validates the
local daemon process command before stopping it, and that validator is not yet
implemented for Windows operators. Use foreground `pond connect` on Windows.

`--json` prints the forward table and exits without opening any tunnels.

### `pond release`

Stops every lease in the named pond, iterating across all providers represented
in the pond — you do not pass `--provider`. Claims are removed for destroyed
resources and retained for reusable stopped resources. Individual failures are
logged as warnings and do not block the rest; the first error is returned so
scripts can tell whether the release was clean.

### `pool` vs `pond`

`pool` is inventory — "what runners exist?" — exposed through `crabbox list` and
the broker's `/v1/pool`. `pond` is grouping — "which leases belong to this
environment?" — created by tagging leases with `--pond <name>` and operated
through `crabbox pond ...`. `crabbox list --pond <name>` filters inventory down
to one pond.

## Exposing ports for the SSH-mesh

`--expose <port>` (repeatable, comma lists accepted) declares a TCP port a lease
wants reachable over the SSH-mesh plane. Ports are validated (`1..65535`),
deduplicated, and stored in the lease's provider label so `pond connect` can
discover them without a separate store. Up to 10 distinct ports per lease.

```sh
crabbox warmup --pond pr-42 --slug api --provider hetzner --expose 8080 --expose 5432
```

## Example use cases

**Per-PR isolated E2E environment.** Every PR gets its own staging; the pond
dies with the PR:

```sh
crabbox warmup --pond "pr-$PR" --slug api --provider hetzner --tailscale
crabbox warmup --pond "pr-$PR" --slug web --provider hetzner --tailscale
# ... run E2E against api.cbx / web.cbx ...
crabbox pond release "pr-$PR"
```

**Mixed-vendor integration test.** CPU on Hetzner, a delegated HTTP sandbox,
DB on Hetzner. Use Tailscale names for tailnet members and the URL bridge for
providers that advertise it:

```sh
crabbox warmup --pond "it-$SHA" --slug api --provider hetzner --tailscale
crabbox warmup --pond "it-$SHA" --slug web --provider e2b --e2b-template base
crabbox warmup --pond "it-$SHA" --slug db  --provider hetzner --tailscale
crabbox run --id api -- "DB_HOST=db.cbx go test ./..."
```

**Per-PR build farm.** A large delegated pond of latency-tolerant helpers that
dies with the PR. This is a work queue, not a low-latency fabric:

```sh
crabbox warmup --pond "build-$PR" --slug coord --provider hetzner
for i in $(seq 1 30); do
  crabbox warmup --pond "build-$PR" --slug "helper-$i" --provider islo &
done
wait
```

## When not to use it

- **Tightly-coupled HPC (MPI / NCCL) across providers** — public-internet
  latency is too high. Keep tightly-coupled jobs on one provider and region.
- **macOS / Windows peer reachability** — not yet covered by any plane.
- **Untrusted multi-tenant isolation** — connectivity inside a pond is
  default-allow. Do not put mutually untrusted workloads in one pond.

## Tailscale names and the `.cbx` hosts file

On managed Linux VM peers, bootstrap installs a systemd timer that rewrites
`/etc/hosts.cbx` and a managed `/etc/hosts` block every 30 seconds from the
box-local `tailscale status --json` output. Each peer renders as
`<tailnet-ipv4> <slug>.cbx`, so members resolve each other as `<slug>.cbx`
within roughly one refresh interval — and the broker never sees a Tailscale
credential. Discovery is gated by the per-pond ACL tag (below): only peers
carrying that tag are written.

Islo uses userspace Tailscale without systemd, OS routes, or the managed hosts
file. Discover its recorded tailnet IPv4 with `crabbox pond peers`, then reach
that address through the workload proxy environment.

## Tailscale ACL bootstrap

Every Tailscale pond peer is advertised under the ACL tag
`tag:cbx-pond-<owner>-<pond>`, so a single concrete policy row gates the whole
pond. `<owner>` is derived from your local git email's local-part (sanitized,
and hashed when it would overflow the tag length budget).

Auto-generated pond tags are **direct-provider only**: they are added to the
provider's Tailscale config when you use a direct (non-brokered) Tailscale-capable
provider. Brokered coordinators keep enforcing their own configured
`CRABBOX_TAILSCALE_TAGS` allowlist and do not receive dynamic per-pond tags.

Installing the tag's `tagOwners` and self-peering grant on your tailnet policy
is **opt-in**. To let lease creation auto-install the rows, set both:

- `TS_API_KEY` — a tailnet API key (optionally `TS_TAILNET` to target a specific
  tailnet; defaults to `-`, your default tailnet).
- `CRABBOX_POND_ACL_BOOTSTRAP=1` — explicit consent to edit the tailnet policy.
  `TS_API_KEY` alone is never treated as consent.

When enabled, Crabbox reads the policy with an ETag, merges in the missing
`tagOwners` entry and a self-peering rule (grants-shape if the policy already
uses grants, otherwise a legacy `acls` row), and writes it back with `If-Match`
so concurrent edits fail fast. If the control plane does not expose a
Tailscale-compatible policy API (e.g. Headscale), lease creation is not blocked.

Without the opt-in, `crabbox doctor --pond <name>` verifies the required policy
row (using `TS_API_KEY` read-only) and points you at this document if it is
missing. The broker never sees Tailscale credentials.

Point the CLI at a self-hosted control plane with `CRABBOX_TS_API_URL` (it wins
over `TS_API_URL`); the default is `https://api.tailscale.com`.

### Manual policy snippet

If you prefer to manage the policy yourself, add the tag owner and a
self-peering rule for your pond's tag (replace `cbx-pond-alice-pr-42` with the
tag `doctor --pond` reports). Grants shape:

```jsonc
{
  "tagOwners": {
    "tag:cbx-pond-alice-pr-42": ["autogroup:admin"]
  },
  "grants": [
    { "src": ["tag:cbx-pond-alice-pr-42"], "dst": ["tag:cbx-pond-alice-pr-42"], "ip": ["*"] }
  ]
}
```

Legacy `acls` shape:

```jsonc
{
  "tagOwners": {
    "tag:cbx-pond-alice-pr-42": ["autogroup:admin"]
  },
  "acls": [
    { "action": "accept", "src": ["tag:cbx-pond-alice-pr-42"], "dst": ["tag:cbx-pond-alice-pr-42:*"] }
  ]
}
```

## See also

- [providers.md](providers.md) — provider capability matrix.
- [tailscale.md](tailscale.md) — the Tailscale transport and tags.
- [doctor.md](doctor.md) — readiness checks, including the pond ACL check.
- [identifiers.md](identifiers.md) — lease IDs and slugs.
