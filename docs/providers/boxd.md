# Boxd

> **Experimental.** The control plane and isolated creation are verified live
> (2026-09-01) over the vendor's TLS gRPC API; the guest execution path has
> narrower production mileage than the mature providers. The isolation check
> is not optional, and there is no plaintext transport fallback.

`provider: boxd` provisions isolated Boxd KVM microVMs for Crabbox's normal
SSH sync/run path. The implementation talks to the public gRPC API at
`boxd.sh:9443` over TLS, authenticating every call with a short-lived JWT
derived from a `bxd_` API key. Inventory must independently prove isolation
before guest access. Guest bootstrap uses the API's authenticated exec stream,
followed by a dedicated guest SSH daemon and a per-lease key. No Boxd CLI,
SDK, device enrollment, credential-store access, or coordinator is required.

Earlier revisions of this integration used the HTTPS console because the gRPC
endpoint was plaintext and console creation did not honor `isolated: true`.
Both are fixed upstream: port 9443 serves TLS with the cluster certificate,
and gRPC `CreateVm` with `isolated: true` was verified live to create a
machine that inventory independently reports as isolated. Crabbox never dials
the endpoint without TLS and never falls back on a handshake failure.

**Public ingress:** Boxd exposes guest port **8000** through an unauthenticated
public HTTPS proxy. Anything listening there can be reached from the internet,
including in isolated mode. Do not put secrets or private development services
on that port. Isolated mode is not an ingress firewall. The dedicated
SSH daemon also has a public TCP forward, accepting only the lease key.

## Authentication and configuration

Supply a **Boxd API key** through `BOXD_API_KEY`, or `CRABBOX_BOXD_API_KEY`
to override it. Keys are never accepted in configuration files or flags. The
format is `bxd_` followed by 43 base62 characters; anything else is rejected
before any request. Create one in the Boxd console (API keys page), or with
the vendor CLI if you already use it: `boxd auth keys create crabbox`.

Crabbox exchanges the key over HTTPS at the console origin
(`POST /api/v1/auth/token`) for a JWT that lives about an hour, re-exchanges
before expiry, and sends only the JWT — as gRPC bearer metadata over TLS.
The raw key goes only to the validated exchange origin. Exchange redirects
are refused, writes are never automatically retried, and vendor response
bodies and stream error text are withheld from diagnostics. An API key is
fenced to one organization on the vendor side; a handful of interactive-only
RPCs (key minting among them) are outside this integration's surface, so a
leaked Crabbox key can never mint more keys.

Crabbox authenticates the session through the gRPC `Whoami` call and uses the
returned `user_id` for ownership checks; it never trusts locally decoded JWT
claims or the exchange response's identity fields. Interactive session tokens
from the retired device-login flow (`BOXD_TOKEN`) are no longer used and get
migration guidance instead of a silent fallback.

```yaml
provider: boxd
boxd:
  apiUrl: https://app.boxd.sh # HTTPS console origin, used only for the key exchange
  grpcUrl: boxd.sh:9443 # TLS gRPC endpoint, bare host:port
  org: "" # fixed personal account context; set an explicit organization if needed
  workRoot: /home/boxd/crabbox
  deleteOnRelease: true
```

`apiUrl` must be an HTTPS origin without user information, path, query, or
fragment; `grpcUrl` must be a bare `host:port`. Both, plus `org`, are
accepted only from trusted config, explicit flags, or environment variables,
so a repository cannot redirect credentials or billing. An empty org always
means the personal account context; it does not inherit an active
organization from another tool.

| Setting | Flag | Environment |
| --- | --- | --- |
| HTTPS exchange origin | `--boxd-api-url` | `CRABBOX_BOXD_API_URL` |
| TLS gRPC endpoint | `--boxd-grpc-url` | `CRABBOX_BOXD_GRPC_URL` |
| Organization | `--boxd-org` | `CRABBOX_BOXD_ORG` |
| Work directory | `--boxd-work-root` | `CRABBOX_BOXD_WORK_ROOT` |
| Destroy on release | `--boxd-delete-on-release` | `CRABBOX_BOXD_DELETE_ON_RELEASE` |

Only Linux and public networking are supported. `--class`, `--type`, and
Tailscale enrollment are not supported; machine sizing follows the Boxd account
quota. The guest must contain `sudo`, OpenSSH server, an existing Ed25519 SSH
host key, bash, git, rsync, and tar. The exec account must be able to run
`sudo -n bash`. Bootstrap does not install packages or replace guest host keys.

## Ownership and SSH trust

Crabbox writes a local claim before creating a VM, requests `isolated: true`,
and records the create response's immutable `vm_id` immediately. Every later
read addresses that ID directly (`GetVm`), never the reusable machine name.
Claims are bound to the normalized console origin, the gRPC endpoint, the
explicit organization, and the authenticated user ID. A renamed VM remains
the same lease, while a same-name replacement is never adopted or deleted.

The gRPC read model carries no per-machine owner field, so the immutable-ID
binding is the primary ownership fence, corroborated on every observation:
the authenticated `Whoami` identity must match the claim, the machine must
not be shared with an organization (Crabbox never creates shared machines),
and its billing organization must match the configured `org` (empty meaning
the personal quota). A machine failing any check is refused, not adopted.

Before guest access, Crabbox requires isolation to be independently confirmed
from an ID-addressed inventory read; a non-isolated machine is rejected
without exec or expose calls and the created VM is rolled back by its
immutable ID. Only after isolation is verified does the bootstrap path
generate a per-lease SSH key and install its public key in a dedicated,
root-owned guest directory over the authenticated exec stream. The dedicated
sshd listens on guest port 2222 and allows only user `boxd` with public-key
authentication; password, keyboard-interactive, and root login are disabled.
Its existing Ed25519 host public key is returned through the exec stream's
stdout. Crabbox requires the stream to complete without a failing exit code,
validates that key, and writes it to isolated lease `known_hosts` before the
first SSH connection. There is no trust-on-first-use or fallback SSH port.
The authenticated VM `public_ip` supplies the SSH host, and the port-forward
response must name the same immutable VM ID; its arbitrary endpoint URL is
never used.

The SSH path supports sync, scripts, fresh PR checkouts, captures, downloads,
Actions hydration, tunnels, desktop, and browser commands when their guest
prerequisites are present. Readiness checks bash, git, rsync, and tar.
`doctor` verifies the API-key exchange, `Whoami`, and claim inventory without
creating a VM; it does not prove guest bootstrap or SSH readiness.

## Lifecycle and recovery

```sh
crabbox doctor --provider boxd
crabbox warmup --provider boxd --slug testbox
crabbox run --provider boxd --id testbox -- bash -lc 'git --version'
crabbox status --provider boxd --id testbox
crabbox heartbeat --provider boxd --id testbox --idle-timeout 30m
crabbox stop --provider boxd testbox
crabbox cleanup --provider boxd --dry-run
```

Use a local Crabbox lease ID or slug to resolve leases. Release destroys the
claimed immutable VM by default and requires deletion proof before removing
its claim: Boxd leaves a readable tombstone (`status: destroyed`) on the
immutable ID after destruction, which is accepted as definitive; plain
inventory absence must instead hold across a bounded grace period.
`boxd.deleteOnRelease: false` instead stops the VM and retains both disk and
claim; reuse restarts it. That release intent persists across processes
unless explicitly overridden. Reuse and ordinary heartbeat preserve the
stored idle timeout; an explicit heartbeat override changes it. Local TTL and
idle expiry require a later `crabbox cleanup`; there is no Boxd server-side
Crabbox janitor. Cleanup honors kept leases and supports dry-run.

Claims written by the earlier console-based provider revision carry a
pre-migration scope and remain visible to `list`, `status`, `stop`, and
`cleanup` under the same identity and immutable-ID fences, so retained or
failed-cleanup machines from before the migration can still be found and
destroyed. Guest access on such a claim is refused; acquire a new lease for
workloads. New acquisitions always write the current scope.

Failed bootstrap rolls back under an independent bounded cleanup context,
even after caller cancellation. `--keep` and `--keep-on-failure` preserve their
normal Crabbox semantics. Cleanup failure retains the immutable ownership
claim for retry. If create receives a definite rejection (invalid argument,
authentication or permission failure, name conflict, or quota exhaustion),
Crabbox removes its unchanged pending intent and reports the rejection.
Transport failures, server errors, or success responses without a valid
immutable ID retain an ambiguous intent with the requested machine name.
Inspect the Boxd console and local claim before manual recovery: Crabbox
cannot safely infer ownership from a matching name. It will not automatically
delete such an unbound resource or discard its recovery claim.

## Live smoke and verification

Run the live smoke with a `bxd_` API key in the environment:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=boxd BOXD_API_KEY=bxd_... scripts/live-smoke.sh
```

The smoke uses normal Crabbox doctor, warmup, status, inspect, run, stop, and
cleanup dry-run commands. It also runs a one-shot command that exits 37 and
checks that the failure code survives release. A separate Python HTTPS client
authenticates with the same API key over the console REST API — a different
transport than the provider under test — records inventory before the run,
and checks for new remaining Crabbox machines after success and on failure.
It never deletes resources independently. Run this smoke without concurrent
Boxd acquisitions in the same account/org, since those would correctly be
reported as new inventory residue. Routing comes only from explicit
environment variables for both clients; no login or signup is attempted.

Local TLS gRPC fake-server tests cover authentication, the API-key exchange,
ownership and isolation fences, exec bootstrap, pinned SSH trust, ambiguous
creation, cleanup, reuse, and keep/stop semantics. Do not treat local
fake-server tests as live provider proof. The vendored client subset of the
vendor's proto lives in `internal/providers/boxd/proto/api.proto`
(regenerate stubs with `scripts/gen-boxd-proto.sh`); see the
[vendor gRPC reference](https://docs.boxd.sh/reference/grpc-api) and
[protobuf reference](https://docs.boxd.sh/reference/grpc-proto) for the full
surface.
