# Crew

Read when:

- you want to lease several boxes that act as one logical group;
- you want `crabbox list` to show only the leases that belong to a group;
- you want peers in a group to reach each other by name on the tailnet.

A **crew** is a named set of Crabbox leases that should discover each other.
The name is stored on each lease as a reserved provider label (`crew=<name>`) at
provision time.
There is no separate top-level crew object: a crew exists for as long as at
least one active lease carries the label, and disappears when the last member
is released. The primitive stays emergent and observable through the
provider-label index the coordinator and direct backends already use.

## Selector

A reserved label key `crew` on every lease record.

```sh
crabbox warmup --crew alpha --slug db
crabbox warmup --crew alpha --slug web
crabbox warmup --crew alpha --slug worker
```

Each command tags its new lease with `crew=alpha` alongside the existing
`slug`, `provider`, `class`, and `state` labels. The label is sanitized the
same way as other provider labels and is bounded to 41 characters before
sanitization so the same name fits inside hostname-derived identifiers
(`<slug>.cbx` peer entries).

```sh
crabbox list --crew alpha
crabbox list --crew alpha --json
```

The crew label is opt-in. Leases created without `--crew` carry no crew label
and are unaffected.

## Peer discovery on the tailnet

When `--crew` is combined with `--tailscale` on a Tailscale-capable provider
(Hetzner, Azure, GCP managed Linux), the CLI advertises one extra ACL tag
when the box joins the tailnet:

```
tag:cbx-crew-<owner>-<crew>
```

The `<owner>` segment is derived from the operator's git email (local-part,
truncated for tag length). The mint happens entirely in user (CLI) context —
the broker never sees a Tailscale credential.

Each crew member writes `/etc/hosts.cbx` from its own `tailscale status
--json` output, filtered by the crew tag. The same systemd timer also
maintains a Crabbox-owned block in `/etc/hosts`, so normal system resolution
can find peers as `<slug>.cbx`:

```sh
curl http://db.cbx:5432/
ssh worker.cbx
```

`<slug>` is the suffix of the `crabbox-<slug>` hostname template every
Tailscale-capable provider already uses, so it doubles as the role name when
slugs are role-shaped (`db`, `web`, `worker`).

For providers without `FeatureTailscale` (E2B, Modal, Cloudflare, Railway,
Islo, Tensorlake, Blacksmith, exe.dev, SSH, Proxmox, Sprites, Daytona,
namespace-devbox), the crew label still sticks for `list --crew`, but the
networking plane is unavailable. `crabbox doctor --crew <name>` flags this with
`skip crew provider=<name> does not support the Tailscale plane`.

## Example: two-lease web server demo

End-to-end smoke that proves a crew is wired up. Each terminal runs from the
same operator shell so `crabbox` shares the local claim store.

Terminal 1 — start the server:

```sh
crabbox warmup --crew demo --slug web --provider hetzner
crabbox ssh demo-web -- 'python3 -m http.server 8080'
```

Terminal 2 — hit it from a peer:

```sh
crabbox warmup --crew demo --slug client --provider hetzner
crabbox ssh demo-client -- 'curl --max-time 5 http://web.cbx:8080'
# expect HTTP 200 with the python directory listing
```

Cleanup:

```sh
crabbox release --crew demo
```

The `.cbx` peer name resolves through the managed `/etc/hosts` block that the
crew-hosts systemd timer maintains on every Tailscale-capable peer.

## Auto-bootstrap of the tailnet policy

When `TS_API_KEY` is exported in the operator shell, the CLI self-bootstraps
the `tag:cbx-crew-<owner>-<crew>` rows on the first `run` or `warmup` for a
new crew: it reads the live policy with an ETag, merges the missing
`tagOwners` and self-peering grant, and PUTs the result back with `If-Match`
so a concurrent edit fails fast. Subsequent leases hit a cached row and
no-op. `crabbox doctor --crew <name>` reports `auto-managed` in that mode.

If you cannot expose `TS_API_KEY` to the CLI (e.g. shared tailnet,
locked-down policy editing), fall back to the manual snippet below.

## One-time tailnet setup (only if `TS_API_KEY` is not available)

The crew plane needs a `tag:cbx-crew-<owner>-<crew>` entry in your tailnet
policy file (Tailscale admin console -> Access Controls) plus one access row
that opens peer-to-peer traffic for that tag. Tailscale's policy schema
requires every advertised tag to be declared in `tagOwners` by its concrete
name (no wildcards), so add one entry per `<crew>` you intend to ship:

```hujson
{
  "tagOwners": {
    "tag:cbx-crew-yossi-e-alpha": ["autogroup:admin"],
  },
  "grants": [
    { "src": ["tag:cbx-crew-yossi-e-alpha"],
      "dst": ["tag:cbx-crew-yossi-e-alpha"],
      "ip": ["*"] },
  ],
}
```

Tailnets still using legacy ACLs can express the same rule as:

```hujson
{
  "tagOwners": {
    "tag:cbx-crew-yossi-e-alpha": ["autogroup:admin"],
  },
  "acls": [
    { "action": "accept",
      "src": ["tag:cbx-crew-yossi-e-alpha"],
      "dst": ["tag:cbx-crew-yossi-e-alpha:*"] },
  ],
}
```

`<owner>` is the first seven characters of the operator's git email
local-part — `yossi.eliaz@incredibuild.com` becomes `yossi-e`. `<crew>` is
the normalized name you pass to `--crew`. The doctor check verifies the
concrete tag declaration and matching peer-to-peer grants or ACL row for the
crew you ask it to inspect.

The plane stays operator-owned: the broker is a leaf and never holds
Tailscale policy-edit credentials. When `TS_API_KEY` is set in the operator
shell, the CLI uses it to (a) self-bootstrap the row on the first lease in
each new crew and (b) verify it on `crabbox doctor --crew <name>`. Without
that env var the auto-bootstrap is skipped silently and the doctor check
falls back to a hint pointing at the manual snippet above. Plain `crabbox
doctor` does not call the Tailscale ACL API unless a crew is explicitly
selected.

```sh
export TS_API_KEY=tskey-api-XXXXXXXXXX
export TS_TAILNET=example.com   # optional; defaults to '-' (the API key's tailnet)
crabbox doctor --provider hetzner --crew alpha
```

## Self-hosted control plane (Headscale, others)

Crew's network plane uses the Tailscale client (installed by cloud-init); it
does not depend on who runs the control server. To point a crew at
[Headscale](https://github.com/juanfont/headscale) or another self-hosted
control plane, set both the client and admin endpoints before running
`crabbox`:

```sh
export TS_CONTROL_URL=https://headscale.example.com   # client login server
export TS_API_URL=https://headscale.example.com       # admin API base (used by crabbox)
export TS_API_KEY=<your control-server admin token>
crabbox warmup --crew alpha --provider hetzner --tailscale
```

`TS_CONTROL_URL` is forwarded into cloud-init and passed to `tailscale up`
as `--login-server`, so the lease registers against the self-hosted control
plane. `CRABBOX_TS_API_URL` is honored first if both are set, which lets you
keep `TS_API_URL` pointed elsewhere for other tooling.

Auto-bootstrap of the `tag:cbx-crew-*` policy row targets the Tailscale-shaped
`/api/v2/tailnet/-/acl` route. Headscale's policy endpoint
(`/api/v1/policy`) is not byte-compatible with that shape — it wraps the
policy as a quoted string field and does not return an ETag — so the CLI
detects the missing route (HTTP 404 or no `ETag` header on the response) and
falls back gracefully. `crabbox doctor --crew <name>` reports a `skip` with
the manual snippet pointer when this happens. For Headscale, apply the
policy block from the section above via:

```sh
headscale policy set --file ./policy.hujson
```

The client-side plane (advertise-tags, `/etc/hosts.cbx` synthesis from
`tailscale status --json`) works identically against either control plane.

For delegated providers (E2B, Modal, Cloudflare, Railway, Islo, Tensorlake,
Blacksmith), the label is honored as metadata but the Tailscale plane is not
applicable. The **bridge plane** (see next section) gives delegated providers
HTTP-only peer discovery on top of the provider's own ingress primitive.

## Bridge plane (cross-provider peer discovery)

`crabbox crew peers --crew <name>` is the single command that lists every
member of a crew with a transport hint, regardless of how that member is
hosted. It folds three planes into one view:

- **Tailscale plane** for managed Linux providers (AWS / Azure / GCP /
  Hetzner / Proxmox / Static SSH). Endpoint is the peer's tailnet IPv4
  (or FQDN when only that is recorded). Reachability is direct on the
  tailnet.
- **URL plane** for delegated providers (Islo / E2B / Railway today;
  Modal / Cloudflare / Tensorlake surface as `unsupported` until their
  providers expose a per-sandbox HTTPS ingress). Endpoint is a
  per-sandbox public HTTPS URL. The plane is deliberately HTTP-only — it
  is not a general-purpose VPN.
- **SSH plane** for SSH-lease providers (exe.dev / RunPod / Daytona /
  Sprites / Namespace / Semaphore). Endpoint is `ssh://<host>:<port>`.

Two more states cover the gaps honestly:

- `pending` — the provider class is known but the lease record has not
  yet captured an endpoint (tailnet not joined, share not minted, SSH
  host not yet known).
- `none` — the provider owns its own connectivity (Blacksmith) or
  Crabbox has no bridge adapter for it. The peer is listed with a note
  explaining the gap so the doctor command can report it without
  pretending the peer is reachable.

For URL-transport peers the bridge calls the provider's native ingress
API (islo `share`, E2B preview, Modal web endpoint, …) through the
`core.BridgeProvider` interface (`PublishPeer` / `ListPeerTargets`). For
tailnet and SSH peers the unified view does not invoke a bridge adapter
at all — it reads the endpoint straight off the local claim sidecar.

### How it works on islo

Islo exposes `POST /sandboxes/{name}/shares` to create a public HTTPS URL
that routes to a chosen sandbox port. Crabbox's bridge calls that endpoint
when the user asks for peer URLs, caches the results inside the islo
share registry (so calls are idempotent), and surfaces them as
`BridgePeerTarget` rows.

```sh
crabbox warmup --provider islo --crew bridge-demo --slug bridge-demo-web
crabbox warmup --provider islo --crew bridge-demo --slug bridge-demo-client

# Publish a public URL for port 8080 on every member of the crew.
crabbox crew peers --crew bridge-demo --share-port 8080 --json
```

The JSON output contains one `BridgePeer` per lease, each with a list of
`Targets` (`{port, url, shareID, expiresAt}`). A client that wants to dial
the web peer from inside another sandbox uses the URL directly:

```sh
curl --silent --show-error https://abc123.share.islo.dev/
```

Without `--share-port`, the command lists *existing* shares rather than
minting new ones, so the call is cheap and side-effect-free. That is also
what the doctor probe runs (HEAD against the first target with a 3s budget
per peer).

### Honest scope

- HTTP/HTTPS only. The bridge plane is built on islo `share`, which is an
  HTTPS endpoint; raw TCP (Postgres, Redis, SSH) is out of scope.
- One target per share. Each sandbox port needs its own share; the bridge
  plane is per-port discovery, not a wildcard tunnel.
- TTL: islo shares default to 24h, max 7d (the bridge clamps any user
  override into that range). Renewal is the application's responsibility.
- No name resolution. The bridge does not write `<slug>.cbx` aliases — peer
  URLs are random subdomains under `share.islo.dev`. Applications consume
  the JSON output and dial by URL; they do not assume a stable hostname.
- Delegated provider only. The Tailscale plane is still the right answer
  for managed Linux providers; the bridge plane is purpose-built for
  backends that cannot join a tailnet.

### Per-provider posture

| Provider                                                | Transport       | Notes                                                                                                       |
| ------------------------------------------------------- | --------------- | ----------------------------------------------------------------------------------------------------------- |
| AWS / Azure / GCP / Hetzner / Proxmox / Static SSH      | `tailnet`       | Endpoint = tailnet IPv4 from the lease record. Empty endpoint surfaces as `pending`.                        |
| exe.dev / RunPod / Daytona / Sprites / Namespace / Semaphore | `ssh`           | Endpoint = `ssh://<host>:<port>` from the lease record. Empty host surfaces as `pending`.                   |
| Islo                                                    | `url`           | Implemented via the islo `share` API (per-port public HTTPS URL, idempotent).                               |
| E2B                                                     | `url`           | URLs synthesised from the native `https://<port>-<sandboxID>.<domain>` preview convention.                  |
| Railway                                                 | `url`           | Surfaces the existing deployment URL on `railwayDeployment.URL` (one URL per service, no per-port routing). |
| Modal                                                   | `url` (unsupported) | Sandbox lease record carries no tunnel URL; the adapter returns an explicit `BridgeState=unsupported`.       |
| Cloudflare                                              | `url` (unsupported) | Worker URL is auth-gated; the adapter returns `BridgeState=unsupported`.                                     |
| Tensorlake                                              | `url` (unsupported) | Serverless invocation model — no per-sandbox HTTPS endpoint; the adapter returns `BridgeState=unsupported`.  |
| Blacksmith                                              | `none`          | Owns its own connectivity; surfaced with the documented note.                                                 |

### Doctor reachability matrix

`crabbox doctor --crew <name>` runs the existing Tailscale ACL check for
the named crew and, in the same invocation, prints the per-transport
reachability matrix derived from the unified peer list:

```
crew "alpha": 4 members
  transport breakdown: none=1 ssh=1 tailnet=1 url=1
  reachability:
    tailnet -> tailnet : OK
    tailnet -> url     : OK (via outbound HTTPS)
    tailnet -> ssh     : WARN (requires operator-side bridge — see SSH-mesh DRAFT PR)
    url     -> tailnet : NO (no public endpoint on tailnet members)
    url     -> url     : OK
    url     -> ssh     : WARN (requires operator-side bridge)
    ssh     -> tailnet : WARN (requires operator-side bridge)
    ssh     -> url     : OK (via outbound HTTPS)
    ssh     -> ssh     : WARN (requires operator-side bridge — peers do not share a mesh)
```

The matrix is asymmetric on purpose. A tailnet peer can dial a URL peer
over outbound HTTPS, but a URL peer cannot dial a tailnet peer — the
tailnet member has no public endpoint. SSH pairs are flagged WARN
because Crabbox does not currently mesh SSH leases.

## Why a label, not a new object

Crabbox's labels already drive cleanup, the portal lease list, broker
filters, and machine identity. Putting the crew name in the same place makes
the primitive observable, queryable, and removable through the same paths.
The maintainer's recent PR #118 rewrite of exe.dev — from a custom transport
into a normal SSH lease provider — set the rule the design follows: bend new
features into existing abstractions; do not grow parallel verb trees.

## Provider posture

| Provider                                                            | `--crew` tagged | Peer DNS (`<slug>.cbx`)              | Tailscale ACL doctor check |
| ------------------------------------------------------------------- | --------------- | ------------------------------------ | -------------------------- |
| Hetzner / Azure / GCP managed Linux                                 | yes             | yes (`/etc/hosts` managed block)     | yes, with `doctor --crew`  |
| AWS Linux / AWS Windows / AWS Mac                                   | yes             | follow-up                            | n/a (no `FeatureTailscale`)|
| Proxmox / SSH / Daytona / Sprites / exe.dev / namespace-devbox      | yes             | n/a (non-managed tailnet)            | skip with `doctor --crew`  |
| E2B / Modal / Cloudflare / Railway / Islo / Tensorlake / Blacksmith | yes             | n/a                                  | skip with `doctor --crew`  |
