# pond

`crabbox pond` is the cross-provider peer-discovery and lifecycle surface for a
**pond** — an emergent group of leases that share a `--pond <name>` label. There
is no top-level pond object: a pond exists for as long as at least one active
lease carries the label. See [docs/features/pond.md](../features/pond.md) for the
full design.

The command has four subcommands:

| Subcommand          | Purpose                                                                 |
| ------------------- | ----------------------------------------------------------------------- |
| `pond peers`        | List every peer in a pond with a transport hint and endpoint.           |
| `pond connect`      | Open operator-side SSH `-L` forwards to members' `--expose` ports.      |
| `pond disconnect`   | Stop daemonized SSH-mesh forwards started by `pond connect --export`.   |
| `pond release`      | Stop every lease in the pond and remove its local claims.               |

```sh
crabbox pond peers --pond alpha
crabbox pond peers --pond alpha --json
crabbox pond peers --pond alpha --provider islo --share-port 8080
crabbox pond peers --pond alpha --share-port 8080 --share-ttl 1h --json
crabbox pond connect alpha --export
crabbox pond disconnect alpha
crabbox pond release alpha
crabbox doctor --pond alpha
```

Pond discovery reads **local claim sidecars** (the per-repo lease records this
machine wrote), not the coordinator. Leases claimed on another operator machine
do not appear. For coordinator-authoritative lease listings filtered by pond,
use [`crabbox list --pond <name>`](list.md).

## `pond peers`

List every locally known peer in the named pond, regardless of provider. With
no `--provider`, the command fans out across every provider represented in the
pond's local claims and concatenates the results; passing `--provider` restricts
to a single provider.

| Flag             | Default | Description                                                                                          |
| ---------------- | ------- | ---------------------------------------------------------------------------------------------------- |
| `--pond <name>`  | —       | Required. Pond label to resolve.                                                                     |
| `--provider`     | (all)   | Restrict to a single provider.                                                                       |
| `--json`         | `false` | Emit machine-readable JSON instead of text.                                                          |
| `--share-port`   | `0`     | If set (1–65535), publish a public URL for that port on each URL-transport peer. Idempotent — an existing share is reused. |
| `--share-ttl`    | `24h`   | TTL for shares created with `--share-port`. Islo clamps this into its legal 60s–7d range.            |

Without `--share-port`, the command lists existing endpoints. These calls are
cheap and side-effect-free — for URL-transport peers, the bridge backend is only
queried when a peer has no recorded endpoint yet — so `pond peers` is safe in
scripts and CI doctor probes.

JSON output wraps the rows in a `members` array. Each member carries its
primary `transport`, the full set of `transports` its provider supports, and an
optional `note`:

```json
{
  "members": [
    { "slug": "web",  "provider": "hetzner",    "transport": "tailnet", "endpoint": "100.64.1.3",                  "labels": {"role": "web"} },
    { "slug": "api",  "provider": "islo",       "transport": "url",     "endpoint": "https://abc.share.islo.dev",  "labels": {"role": "api"} },
    { "slug": "db",   "provider": "runpod",     "transport": "ssh",     "endpoint": "ssh://1.2.3.4:22",            "labels": {"role": "db"} },
    { "slug": "what", "provider": "blacksmith", "transport": "none",    "endpoint": "",                            "labels": {"role": "isolated"}, "note": "blacksmith owns connectivity" }
  ]
}
```

### Transports

A peer's `transport` is its provider's *primary* (recommended) plane; the full
list of planes the provider supports is on `transports`. A provider opts into a
plane by declaring the matching feature (`tailscale`, `ssh`, `url-bridge`), so
one provider can advertise several — managed-Linux providers offer both the
tailnet peer mesh and the operator-side SSH mesh.

| Transport | Producers                                                                                                  | Endpoint shape                |
| --------- | ---------------------------------------------------------------------------------------------------------- | ----------------------------- |
| `tailnet` | Hetzner / Azure / GCP (managed Linux with Tailscale)                                                       | tailnet IPv4 (or FQDN)        |
| `ssh`     | AWS / Proxmox / static SSH / exe.dev / RunPod / Daytona / Sprites / Namespace / Semaphore                  | `ssh://<host>:<port>`         |
| `url`     | Islo / E2B / Railway                                                                                       | per-sandbox public HTTPS URL  |
| `pending` | a tailnet- or SSH-capable provider whose claim has no endpoint recorded yet                                | empty (note explains)         |
| `none`    | Blacksmith (owns its own connectivity), or any provider with no bridge adapter                             | empty (note explains)         |

URL-transport peers whose adapter cannot bridge are still listed, with
`bridgeState=unsupported` (JSON) / `bridge=unsupported` (text), so callers see
the gap rather than mistaking it for "no shares published yet".

### Provider notes

| Provider                                                     | Transport | Notes                                                                                                       |
| ------------------------------------------------------------ | --------- | ----------------------------------------------------------------------------------------------------------- |
| Hetzner / Azure / GCP                                        | `tailnet` | Endpoint = tailnet IPv4 (or FQDN) from the local claim sidecar. Empty endpoint surfaces as `pending`.       |
| AWS / Proxmox / static SSH                                   | `ssh`     | Endpoint = `ssh://<host>:<port>` from the claim. Empty host or port surfaces as `pending`.                  |
| exe.dev / RunPod / Daytona / Sprites / Namespace / Semaphore | `ssh`     | Endpoint = `ssh://<host>:<port>` from the claim. Empty host or port surfaces as `pending`.                  |
| Islo                                                         | `url`     | Uses the Islo shares API. Existing shares are reused, so the call is idempotent.                            |
| E2B                                                          | `url`     | Synthesizes the canonical per-port preview URL from the existing sandbox and config.                        |
| Railway                                                      | `url`     | Surfaces the deployment URL Railway already populates. One URL per service (no per-port routing).           |
| Modal / Cloudflare / Tensorlake                              | `none`    | No advertised pond transport today; surfaced with a `no advertised pond transport` note.                    |
| Blacksmith                                                   | `none`    | Owns its own connectivity; surfaced with the note `blacksmith owns connectivity`.                           |

## `pond connect`

```text
crabbox pond connect <name> [--provider <name>] [--export] [--json]
```

Reads pond members across every SSH-mesh-capable provider in the pond, computes
a unified forward table, opens operator-side `ssh -L` forwards to each member's
`--expose` ports, and writes a per-pond hosts file and env snippet under
`~/.crabbox/pond/<name>/`.

A provider is SSH-mesh-eligible when it advertises the `ssh` feature. That
includes managed-Linux providers and SSH-lease providers, so a single pond can
span both groups and still connect with one command. URL-only members (for
example Islo, E2B, or Railway) are skipped here with a warning but still show up
in `pond peers`.

| Flag                | Description                                                                              |
| ------------------- | ---------------------------------------------------------------------------------------- |
| `--provider <name>` | Restrict to a single provider (default: all SSH-mesh-capable members).                   |
| `--export`          | Daemonize the forwards and print `export …` lines for `eval`, then exit.                 |
| `--json`            | Print the forward table as JSON and exit without starting forwards.                      |

Each forward gets a free loopback port in the **51820–52819** range, probed for
availability against `127.0.0.1`.

### Default (foreground) mode

Without `--export`, the command starts the forwards, prints the export lines and
the paths of the rendered files, then blocks until interrupted (Ctrl-C), keeping
all tunnels alive:

```text
pond "alpha" SSH-mesh ready (2 forwards)
export CRABBOX_POND_WEB_8080=127.0.0.1:51820
export CRABBOX_POND_WORKER_3000=127.0.0.1:51821
wrote /Users/alice/.crabbox/pond/alpha/hosts
wrote /Users/alice/.crabbox/pond/alpha/env
```

### Export (daemon) mode

With `--export`, the command starts each forward as a daemon process that
survives the CLI exit, verifies none exit immediately (wrong key, host
unreachable, …), records the PIDs in `~/.crabbox/pond/<name>/daemon.json`, prints
the `export` lines to stdout, and exits. Daemon mode runs on macOS and Linux
operator hosts only; on Windows, run `pond connect` without `--export`.

```bash
eval $(crabbox pond connect alpha --export)
curl $CRABBOX_POND_WEB_8080
crabbox pond disconnect alpha
```

The exported variable name is `CRABBOX_POND_<PEER>_<PORT>`, where `<PEER>` is the
peer's uppercased, shell-safe name. When two members would collide on a name,
the provider (then a short lease-ID suffix) is appended to disambiguate.

### Rendered files

The hosts file (`~/.crabbox/pond/<name>/hosts`) maps operator-side `<peer>.cbx`
aliases to loopback. Because `/etc/hosts` cannot encode a port, the local/remote
port mapping lives in trailing comments and in the `CRABBOX_POND_*` variables —
these aliases are operator-side conveniences, not lease-to-lease DNS:

```text
# crabbox pond SSH-mesh operator-side aliases
# Use CRABBOX_POND_<PEER>_<PORT> for the forwarded host:port.
127.0.0.1  web.cbx web-8080.cbx  # local=127.0.0.1:51820 remote=:8080
127.0.0.1  worker.cbx worker-3000.cbx  # local=127.0.0.1:51821 remote=:3000
```

A lease declares the ports it wants reachable over the SSH mesh at warmup time
with `--expose <port>` (repeatable, up to 10 distinct ports per lease).

## `pond disconnect`

```text
crabbox pond disconnect <name>
```

Stops the daemonized SSH-mesh forwards recorded by the last
`pond connect <name> --export` run. It reads only the per-pond `daemon.json`
state file and validates each recorded process (command line must still be the
matching `ssh -L` forward) before stopping it — it never scans unrelated SSH
processes. It reports how many daemons it stopped, or that none were recorded.
Because `--export` is macOS/Linux-only, `disconnect` is meaningful only on those
operator hosts.

## `pond release`

```text
crabbox pond release <name>
```

Stops every locally claimed lease in the named pond, across all providers, and
removes their claim sidecars on success. No `--provider` flag is needed — the
command iterates every local claim whose pond label matches. For each claim it
loads the provider backend and calls the appropriate stop path
(`DelegatedRunBackend.Stop` for delegated providers, `ReleaseLease` for SSH-lease
providers). Leases on providers without a stop-capable backend are skipped with a
warning. Individual stop failures are logged as warnings and do not block the
remaining peers; the first error is returned so callers can tell whether the
release was fully clean.

## `doctor --pond`

[`crabbox doctor --pond <name>`](doctor.md) runs the Tailscale ACL check (when the
pond includes tailnet-capable providers) and, in the same invocation, prints the
cross-provider reachability matrix:

```text
pond "alpha": 4 members
  transport breakdown: none=1 ssh=1 tailnet=1 url=1
  reachability:
    tailnet -> tailnet : OK
    tailnet -> url     : OK (via outbound HTTPS)
    tailnet -> ssh     : WARN (requires operator-side bridge — see SSH-mesh DRAFT PR)
    tailnet -> none    : NO (destination has no published endpoint)
    url     -> tailnet : NO (no public endpoint on tailnet members)
    url     -> url     : OK
    url     -> ssh     : WARN (requires operator-side bridge)
    url     -> none    : NO (destination has no published endpoint)
    ssh     -> tailnet : WARN (requires operator-side bridge)
    ssh     -> url     : OK (via outbound HTTPS)
    ssh     -> ssh     : WARN (requires operator-side bridge — peers do not share a mesh)
    ssh     -> none    : NO (destination has no published endpoint)
    none    -> *       : NO (source provider owns its own connectivity)
```

The matrix only emits rows and columns for transports actually present in the
pond, and it is intentionally asymmetric: `tailnet -> url` works (the tailnet
peer makes an outbound HTTPS request) while `url -> tailnet` does not (the
URL-only peer has no route into the tailnet).

## Scope

The bridge plane is **HTTP-only** for URL-transport peers. Non-HTTP protocols
(raw TCP/UDP, SSH on a custom port, Postgres, Redis, …) are not exposed by
per-port HTTPS shares — use a tailnet-capable provider, or the SSH mesh via
`pond connect`, for those.
</content>
</invoke>
