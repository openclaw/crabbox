# pond

`crabbox pond` is the cross-provider peer-discovery surface. It lists locally
known members of the named pond with a transport hint (`tailnet` / `url` /
`ssh` / `pending` / `none`) and, when known, a canonical endpoint. That makes
scripts transport-aware without pretending every provider exposes the same
network shape.
See `docs/features/pond.md` for the full design.

```sh
crabbox pond peers --pond alpha
crabbox pond peers --pond alpha --json
crabbox pond peers --pond alpha --provider islo --share-port 8080
crabbox pond peers --pond alpha --share-port 8080 --share-ttl 1h --json
crabbox pond release alpha
crabbox doctor --pond alpha
```

## `pond release`

Stop every locally claimed lease in the named pond across all providers and
remove their claim sidecars. No `--provider` flag is needed — the command
iterates every local claim whose pond label matches. Individual stop failures are logged as
warnings and do not block the remaining peers.

```sh
crabbox pond release alpha
```

The command loads each provider backend from the claim sidecar, calls the
appropriate stop path (`DelegatedRunBackend.Stop` or
`SSHLeaseBackend.ReleaseLease`), and removes the local claim file on
success. Leases on providers without a stop-capable backend are skipped
with a warning.

## `pond peers`

List every locally known peer in the named pond, regardless of provider. When
`--provider` is omitted the command fans out across every provider represented
in local claims for the pond; passing it preserves the original single-provider
semantics. Provider labels and coordinator state remain authoritative for
`crabbox list --pond`; `pond peers` depends on local claim sidecars for bridge
metadata and may ignore leases claimed on another operator machine.

| Flag             | Default | Description                                            |
| ---------------- | ------- | ------------------------------------------------------ |
| `--pond <name>`  | —       | Required. The pond label to resolve.                   |
| `--provider`     | (all)   | Restrict to a single provider; defaults to every provider represented in the pond. |
| `--json`         | `false` | Emit machine-readable JSON instead of text.            |
| `--share-port`   | `0`     | If non-zero, publish a public URL for that port on each URL-transport peer. The call is idempotent: an existing share is reused. |
| `--share-ttl`    | `24h`   | TTL for shares created with `--share-port`. Islo clamps into the legal 60s..7d range. |

Without `--share-port`, the command lists existing endpoints — calls are
cheap and side-effect-free, suitable for use in scripts and CI doctor probes.

JSON output:

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

Transport semantics:

| Transport | Producers                                                                | Endpoint shape              |
| --------- | ------------------------------------------------------------------------ | --------------------------- |
| `tailnet` | Hetzner / Azure / GCP (managed Linux)                                    | tailnet IPv4 (or FQDN)      |
| `url`     | Islo / E2B / Railway today; Modal / Cloudflare / Tensorlake surface as `unsupported` until their providers expose a per-sandbox HTTPS ingress | per-sandbox public HTTPS URL |
| `ssh`     | AWS / Proxmox / Static SSH / exe.dev / RunPod / Daytona / Sprites / Namespace / Semaphore | `ssh://<host>:<port>`        |
| `pending` | tailnet- or SSH-capable provider whose lease record has no endpoint yet  | empty                        |
| `none`    | Blacksmith (owns connectivity), or any provider without a bridge adapter | empty (`note` explains why)  |

## Provider support

| Provider                                              | Transport class | Notes                                                                                                                              |
| ----------------------------------------------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Hetzner / Azure / GCP                                 | `tailnet`       | Endpoint = tailnet IPv4 from the local claim sidecar (`tailscaleIPv4` field). Empty endpoint surfaces as `pending`.                |
| AWS / Proxmox / Static SSH                            | `ssh`           | Endpoint = `ssh://<host>:<port>` from the local claim sidecar. Empty host surfaces as `pending`.                                   |
| exe.dev / RunPod / Daytona / Sprites / Namespace / Semaphore | `ssh`           | Endpoint = `ssh://<host>:<port>` from the local claim sidecar. Empty host surfaces as `pending`.                                   |
| Islo                                                  | `url`           | Uses the islo `POST /sandboxes/{name}/shares` API. Existing shares are reused so calls are idempotent.                             |
| E2B                                                   | `url`           | Synthesises the canonical `https://<port>-<sandboxID>.<domain>` preview URL directly from the existing E2B sandbox + config.       |
| Railway                                               | `url`           | Surfaces the deployment URL already populated by Railway on `LatestDeployment`. One URL per service (no per-port routing).         |
| Modal                                                 | `url`           | Sandbox lease record does not carry a tunnel URL today; the adapter returns an explicit `BridgeState=unsupported` signal.          |
| Cloudflare                                            | `url`           | Worker URL is auth-gated; the adapter returns `BridgeState=unsupported` so callers see the gap.                                    |
| Tensorlake                                            | `url`           | Serverless invocation model — no per-sandbox HTTPS endpoint; adapter returns `BridgeState=unsupported`.                            |
| Blacksmith                                            | `none`          | Owns its own connectivity; surfaced with note "blacksmith owns connectivity".                                                       |

Peers on `unsupported` URL adapters still appear in the output with
`BridgeState=unsupported` (JSON) / `bridge=unsupported` (text) so callers
see the gap rather than mistaking it for "no shares published yet".

## `pond connect`

`crabbox pond connect <name> [--export]` opens operator-side SSH `-L` port
forwards to every pond member that declared ports with `--expose` at warmup
time, and writes a per-pond hosts file and shell-export snippet under
`~/.crabbox/pond/<name>/`.

```
Usage:
  crabbox pond connect <name> [flags]

Flags:
  --export          print shell exports for eval and daemonize the forwards
  --provider <n>    restrict to a single provider (default: all SSH-mesh members)
  --json            emit machine-readable JSON instead of starting forwards
```

With `--export`, the command starts SSH tunnels as daemon processes, prints
`export CRABBOX_POND_<NAME>_<PORT>=127.0.0.1:<local>` lines to stdout,
and exits — the tunnels survive via the daemon runner. Use as:

```bash
eval $(crabbox pond connect mypond --export)
curl $CRABBOX_POND_WEB_8080
```

Without `--export`, the command blocks until Ctrl-C, keeping all tunnels
alive.

Each forward gets a local loopback port in the 51820–52819 range, probed
for availability. The hosts file (`~/.crabbox/pond/<name>/hosts`) maps each
operator-side forward to its local address; these entries are not lease-to-lease
DNS:

```
# crabbox pond SSH-mesh — operator-side forwards
127.0.0.1:51820  web.cbx (remote :8080)
127.0.0.1:51821  worker.cbx (remote :3000)
```

## `doctor --pond`

`crabbox doctor --pond <name>` runs the Tailscale ACL row check (when the
pond uses tailnet-capable providers) and, in the same invocation, prints
the cross-provider reachability matrix:

```
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

The matrix is intentionally asymmetric: `tailnet -> url` works (the tailnet
peer makes an outbound HTTPS request) while `url -> tailnet` does not (the
delegated peer has no route into the tailnet). SSH pairs are flagged WARN
because Crabbox does not run an SSH mesh today — operator-side bridging is
required.

## Scope reminder

The bridge plane is **HTTP-only** for URL-transport peers. Non-HTTP
protocols (raw TCP/UDP, SSH on a custom port, Postgres, Redis, …) are not
exposed by per-port HTTPS shares; use a tailnet-capable provider for those.
