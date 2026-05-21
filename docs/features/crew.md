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
