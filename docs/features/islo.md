# Islo

Read when:

- choosing `provider: islo`;
- configuring the Islo sandbox image, sizing, snapshot, or gateway profile;
- reviewing how Crabbox behaves on a delegated-run provider.

`provider: islo` is a delegated-run provider: Islo owns the sandbox and the
command transport, while Crabbox owns local config, repo claims, the sync
manifest and its guardrails, slugs, timing summaries, and normalized
`list`/`status` rendering. Crabbox uses the Islo Go SDK for auth and sandbox
lifecycle (create, list, status, pause, resume) and calls the HTTP API
directly for stop (an empty-body `DELETE`), archive upload, shares, and
command output — the last via a small SSE reader for the
`POST /sandboxes/{name}/exec/stream` endpoint, since the SDK's exec helper
coalesces streamed output.

Sandboxes are Linux-only. `crabbox ssh --provider islo` can print a direct SSH
command for a Crabbox-created sandbox at `<sandbox>.islo`, but Islo `run`
commands and sync still use Islo's streaming exec and archive APIs rather than
SSH/rsync.

## Auth

```sh
export ISLO_API_KEY=ak_...
```

`ISLO_BASE_URL` (or `islo.baseUrl`) overrides the default `https://api.islo.dev`.
Both keys also accept the `CRABBOX_`-prefixed forms `CRABBOX_ISLO_API_KEY` and
`CRABBOX_ISLO_BASE_URL`, which take precedence.

## Config

```yaml
provider: islo
target: linux
islo:
  baseUrl: https://api.islo.dev
  image: docker.io/library/ubuntu:26.04
  workdir: crabbox
  gatewayProfile: ""
  snapshotName: ""
  vcpus: 2
  memoryMB: 4096
  diskGB: 20
```

Defaults: `baseUrl` `https://api.islo.dev`, `workdir` `crabbox`, `vcpus` `2`,
`memoryMB` `4096`, `diskGB` `20`. Crabbox keeps the resolved image/capacity
defaults in config for display and override compatibility, but omits implicit
default `image`, `vcpus`, `memoryMB`, and `diskGB` values from sandbox creation
so Islo can use its tenant defaults. Explicit config, environment, and flag
values are still sent even when they equal the Crabbox defaults.

`islo.workdir` is a relative directory name under `/workspace`. Absolute paths
and `..` escapes are rejected before sandbox creation. Crabbox applies the value
to archive upload and command execution, so the working set still lands in
`/workspace/<islo.workdir>` without relying on create-time workdir support.

Each config key has an equivalent flag and `CRABBOX_ISLO_*` environment
variable:

| Config key       | Flag                     | Env var                        |
| ---------------- | ------------------------ | ------------------------------ |
| `baseUrl`        | `--islo-base-url`        | `CRABBOX_ISLO_BASE_URL`        |
| `image`          | `--islo-image`           | `CRABBOX_ISLO_IMAGE`           |
| `workdir`        | `--islo-workdir`         | `CRABBOX_ISLO_WORKDIR`         |
| `gatewayProfile` | `--islo-gateway-profile` | `CRABBOX_ISLO_GATEWAY_PROFILE` |
| `snapshotName`   | `--islo-snapshot-name`   | `CRABBOX_ISLO_SNAPSHOT_NAME`   |
| `vcpus`          | `--islo-vcpus`           | `CRABBOX_ISLO_VCPUS`           |
| `memoryMB`       | `--islo-memory-mb`       | `CRABBOX_ISLO_MEMORY_MB`       |
| `diskGB`         | `--islo-disk-gb`         | `CRABBOX_ISLO_DISK_GB`         |

`gatewayProfile` accepts an Islo gateway profile name or id and is passed
opaquely in the sandbox create request. Gateway profiles are created and
managed on the Islo side and configure Islo's own egress gateway for the
sandbox — the setting is unrelated to the Crabbox coordinator. When unset, the
field is omitted from the create request so Islo applies its own default.

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:26.04
crabbox run --provider islo -- pnpm test
crabbox status --provider islo --id blue-lobster
crabbox pause --provider islo blue-lobster
crabbox resume --provider islo blue-lobster
crabbox stop --provider islo blue-lobster
```

## Behavior

- **warmup** creates a `crabbox-...` Islo sandbox and records a local lease ID of
  the form `isb_<sandbox-name>` plus a Crabbox slug.
- **run** creates or reuses a sandbox, validates `islo.workdir`, builds the
  Crabbox sync manifest, uploads it as a gzipped archive into
  `/workspace/<islo.workdir>`, streams stdout/stderr from Islo's SSE exec
  endpoint, and returns the remote exit code. A stream is only treated as
  successful once an exit event arrives.
- **list** and **status** go through the Islo SDK; **stop** issues a direct
  `DELETE`. All three act only on Crabbox-created sandboxes. Identifiers may be a
  Crabbox slug, an `isb_...` lease ID, or a Crabbox-created sandbox name;
  non-Crabbox sandboxes are rejected.
- **pause** snapshots the sandbox and releases its active compute while
  preserving the local lease claim; **resume** restores the sandbox to running.
- The sandbox is deleted on release unless kept. `--keep-on-failure` keeps a
  newly created failed sandbox until an explicit `stop` or provider-side expiry.

## URL bridge (per-port shares)

Islo declares the `url-bridge` capability. Crabbox publishes a per-port public
HTTPS share for an exposed sandbox port via Islo's
`POST /sandboxes/{name}/shares` API and reuses an existing share for the same
port when one is present. This is how delegated providers surface a reachable
URL in place of an SSH-tunneled bridge.

Requested share TTLs are clamped into Islo's legal 60s–7d range, matching the
`pond peers --share-ttl` contract. Reuse skips a share that expires within the
next 30 seconds, so a nearly-expired share is replaced with a fresh one rather
than handed out.

## Tailscale (userspace tailnet)

Islo advertises `FeatureTailscale` in addition to `url-bridge`. Because Islo is a
delegated-run provider with no Crabbox-managed SSH lease, Crabbox cannot reuse
the SSH runner-bootstrap that VM providers (Hetzner/Azure/GCP) use to join the
tailnet. Instead, when a lease is created with `--tailscale`, Crabbox brings the
sandbox onto the tailnet **through the Islo exec stream** — no Islo-side changes
are required:

1. it downloads the pinned static Tailscale build into the sandbox (the image
   ships `wget`, not `curl`, and has no systemd to run the packaged unit);
2. it starts `tailscaled` in **userspace-networking** mode. This is deliberate:
   kernel mode rewrites the sandbox routing table, which severs the Islo exec
   transport mid-run. Userspace mode never touches host routing, so the node
   joins the tailnet and the exec channel survives;
3. it runs `tailscale up` with the pond-scoped advertise tags, `TS_CONTROL_URL`
   as `--login-server` when set, and any configured exit-node flags;
4. it records the assigned tailnet IPv4 on the lease claim for health and ACL
   checks. `pond peers` keeps the URL bridge as the member's dialable transport
   and notes that Tailscale is available for outbound proxy traffic only.

```sh
export CRABBOX_TAILSCALE_AUTH_KEY=tskey-auth-...     # reusable, ephemeral, tagged node auth key
crabbox warmup --pond mesh --slug node-a --provider islo --tailscale
crabbox warmup --pond mesh --slug node-b --provider islo --tailscale
crabbox pond peers --pond mesh --json                # URL transport plus outbound-proxy note
```

The static build and its architecture-specific SHA-256 digests are pinned
together in Crabbox.
The direct auth key must be both reusable and ephemeral. Reusable is required
because memory-only identity must re-enroll after daemon loss; ephemeral keeps
those replacement device records from accumulating after sandboxes disappear.
Tailscale auth keys are opaque, so Crabbox cannot inspect these properties and
treats the supplied key as an operator contract.
The Islo path runs Tailscale in userspace mode, so it does not install a kernel
TUN route. For enrolled leases, Crabbox supplies workload commands with local
proxy defaults (`ALL_PROXY=socks5://127.0.0.2:1055`,
`HTTP_PROXY=http://127.0.0.2:1055`, and
`HTTPS_PROXY=http://127.0.0.2:1055`) and their lowercase equivalents; explicit
command environment values in either case override those defaults. An explicit
`ALL_PROXY`/`all_proxy` also suppresses the protocol-specific defaults. Other
processes must opt into those proxies or another userspace Tailscale surface.
The proxy uses `127.0.0.2`, separate from userspace Tailscale's inbound loopback
mapping. Crabbox runs `tailscaled` as `root` with its binaries and control
socket in a root-only directory, while repository sync and workload commands
run as Islo's non-root `islo` user. Node identity stays in memory and the auth
key is passed through stdin, so an Islo filesystem snapshot cannot clone either
credential. The control socket is revalidated before lease reuse, status
reporting, and `pond peers`; after daemon loss, recovery requires a usable auth
key. If recovery fails, stale tailnet claim metadata is removed and the lease
remains visible through its URL bridge for status and discovery. `run` fails
closed instead of executing an enrolled workload with ordinary direct egress.
Read-only `status` and `pond peers` checks do not run the long repair path;
lease reuse through `run` performs re-enrollment when needed.
Unproxied process traffic still uses the sandbox's normal network namespace.
Exit-node settings are passed through to `tailscale up`, but only traffic sent
through the userspace Tailscale path uses them.
Inbound tailnet connections are blocked with shields-up; Islo's
`FeatureTailscale` contract is outbound proxy access, not a forwarded loopback
service surface.

A lease warmed **without** `--tailscale` is unchanged: no tailnet IP is recorded
and `pond peers` reports it on the URL bridge as before. The pond ACL tag and its
auto-bootstrap (`CRABBOX_POND_ACL_BOOTSTRAP=1` + `TS_API_KEY`) apply to Islo
exactly as they do for other direct Tailscale-capable providers.
Tailscale enrollment is creation-time only: a reused plain Islo lease must be
recreated with `--tailscale` rather than enrolled in place.

## Rejected options

Because Islo owns command transport and there is no Crabbox-managed SSH/rsync
target, these `run` options are rejected:

- `--sync-only`, `--checksum`, `--force-sync-large`, `--full-resync` — no
  Crabbox rsync target to drive.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures,
  `--capture-on-fail`, `--artifact-glob`, `--env-helper`, `--stop-after` —
  these require Crabbox-owned transport or execution. `--require-artifact` and
  `--download` support safe relative single files up to 64 KiB, retrieved
  through Islo exec after a successful command.

Large-sync guardrails still apply: the gzipped archive upload runs the same
size preflight as rsync providers, but because `--force-sync-large` is rejected
on Islo, an oversize sync cannot be forced through and fails the preflight
instead. `--shell` passes the raw shell string through to the remote shell.

## SSH access

Crabbox can resolve kept Islo sandboxes for direct SSH:

```sh
islo ssh --setup
crabbox ssh --provider islo --id blue-lobster
# ssh islo@crabbox-repo-abcdef.islo
```

Install and authenticate the [Islo CLI](https://docs.islo.dev/cli/sandbox-commands),
then run `islo ssh --setup` once to install its SSH proxy configuration and
short-lived certificate support. The Islo CLI must remain authenticated through
`islo login` or `ISLO_API_KEY`; `CRABBOX_ISLO_API_KEY` alone is only read by
Crabbox. By default the rendered target is `islo@<sandbox>.islo` on port `22`.
Explicit `ssh.user`, `ssh.port`, or `ssh.key` settings are honored. This is a
login helper only: `vnc`, `code`, Crabbox rsync, and Actions hydration are not
available on `provider: islo`. When you need a Crabbox-managed SSH box, use
Hetzner, AWS, static SSH, or Daytona instead.

## Related docs

- [Provider: Islo](../providers/islo.md)
- [Provider backends](../provider-backends.md)
