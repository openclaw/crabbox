# Boxd

> **Experimental — known production limitation, 2026-08-27:** HTTPS creation accepted
> `isolated: true`, but independent inventory reported the created VM as
> `isolated: false`. The current console does not satisfy this provider's
> isolation contract. This integration is available experimentally with that
> limitation documented: `doctor` can succeed, but `warmup` and `run` reject
> non-isolated machines before guest access. The isolation check is not optional,
> and there is no plaintext transport fallback. Live validation proved rejection
> and cleanup, not successful guest execution or reuse. Wait for a compatible
> vendor deployment before relying on Boxd for workloads.

`provider: boxd` is intended to provision isolated Boxd KVM microVMs for Crabbox's
normal SSH sync/run path. The implementation uses the HTTPS console API at
`https://app.boxd.sh` and requires inventory to prove isolation before guest
access. Its intended bootstrap path uses the authenticated WSS terminal,
followed by a dedicated guest SSH daemon and a per-lease key.
No Boxd CLI, SDK, device enrollment, credential-store access, or coordinator is
required. Crabbox does not use the vendor's plaintext gRPC control plane.

**Public ingress:** Boxd exposes guest port **8000** through an unauthenticated
public HTTPS proxy. Anything listening there can be reached from the internet,
including in isolated mode. Do not put secrets or private development services
on that port. Isolated mode is not an ingress firewall. The intended dedicated
SSH daemon would also have a public TCP forward, accepting only the lease key.

## Authentication and configuration

Supply an **interactive console session** through `BOXD_TOKEN`, or
`CRABBOX_BOXD_TOKEN` to override it. Tokens are never accepted in configuration
or flags. Boxd console mutations require an interactive session: API-key and
in-VM sessions are read-only, even when inventory and `whoami` succeed. Raw
`bxd_` API keys are rejected before any request. There is no API-key exchange,
automatic credential fallback, gRPC, vendor CLI, or credential-store lookup.

Crabbox authenticates the session through HTTPS `whoami` and uses the returned
`user_id` for ownership checks; it never trusts locally decoded JWT claims.
Sessions are not automatically renewed. If a session expires or is refused,
obtain a new interactive session before retrying. API redirects are refused,
writes are never automatically retried, and response bodies and terminal error
text are withheld from diagnostics. A successful `doctor` proves reads work,
not that a supplied session has permission to create machines.

### Obtain a session without the vendor CLI

From a Crabbox source checkout, run the provider-owned helper in a **private
interactive terminal**. Node.js 20.11 or newer is needed only to obtain or
renew the session; the Go provider runtime does not need Node.js.

```sh
mkdir -p "$HOME/.config/crabbox"
node scripts/boxd-login.mjs --output "$HOME/.config/crabbox/boxd-session"
# Keep shell tracing off when loading a token into the environment.
set +x
export BOXD_TOKEN="$(cat "$HOME/.config/crabbox/boxd-session")"
crabbox doctor --provider boxd
```

The helper starts `POST /api/v1/auth/device` over HTTPS with an empty JSON
object, then shows one private approval URL. Open that URL in your signed-in
console browser to authorize the device. It never launches a browser or signs
up automatically. The URL contains a session secret: do not share it, record
the terminal, or run the helper in public logs. Returned approval URLs must
point to `/link` on the same HTTPS console origin and match the device session.

The helper polls `/api/v1/auth/device/poll` until authorization or expiry, with
a ten-minute hard limit and bounded requests. It checks the returned token
against `whoami`, then writes only the token to the explicit output path with
mode `0600`, never stdout. Existing files and symlinks are refused; use a new
private path when renewing. Failed or canceled logins remove their empty output
file. Keep session files outside repositories. `--api-url <https-origin>` or
`CRABBOX_BOXD_API_URL` selects a different trusted console for the helper;
configure the provider to use the same origin. `doctor` never invokes login.

```yaml
provider: boxd
boxd:
  apiUrl: https://app.boxd.sh
  org: "" # fixed personal account context; set an explicit organization if needed
  workRoot: /home/boxd/crabbox
  deleteOnRelease: true
```

`apiUrl` must be an HTTPS origin without user information, path, query, or
fragment. `apiUrl` and `org` are accepted only from trusted config, explicit
flags, or environment variables, so a repository cannot redirect credentials
or billing. An empty org always means the personal account context; it does
not inherit an active organization from another tool.

| Setting | Flag | Environment |
| --- | --- | --- |
| HTTPS origin | `--boxd-api-url` | `CRABBOX_BOXD_API_URL` |
| Organization | `--boxd-org` | `CRABBOX_BOXD_ORG` |
| Work directory | `--boxd-work-root` | `CRABBOX_BOXD_WORK_ROOT` |
| Destroy on release | `--boxd-delete-on-release` | `CRABBOX_BOXD_DELETE_ON_RELEASE` |

Only Linux and public networking are supported. `--class`, `--type`, and
Tailscale enrollment are not supported; machine sizing follows the Boxd account
quota. The guest must contain `sudo`, OpenSSH server, an existing Ed25519 SSH
host key, bash, git, rsync, and tar. The terminal account must be able to run
`sudo -n bash`. Bootstrap does not install packages or replace guest host keys.

## Ownership and SSH trust

Crabbox writes a local claim before creating a VM, requests `isolated: true`,
and records the create response's immutable `vm_id` immediately. Inventory
returns that identity as `id`; the create response does not contain `id`.
Every later inventory
lookup uses that ID, never the reusable machine name. Claims are bound to the
normalized HTTPS origin, explicit organization, and authenticated user ID;
the VM owner must match that user. A renamed VM remains the same lease, while
a same-name replacement is never adopted or deleted.

Before guest access, Crabbox requires isolation to be independently confirmed
from inventory. The observed non-isolated result is rejected without terminal
or expose calls. Only after isolation is verified would the bootstrap path
generate a per-lease SSH key and install its public key in a dedicated,
root-owned guest directory. The dedicated sshd listens on guest port 2222 and
allows only user `boxd` with public-key authentication; password, keyboard
interactive, and root login are disabled. Its existing host public key is
returned through WSS. Crabbox requires the terminal's explicit successful exit,
validates that key, and writes it to isolated lease `known_hosts` before the
first SSH connection. There is no trust-on-first-use or fallback SSH port.
The authenticated VM `public_ip` supplies the SSH host; the port-forward
response's arbitrary endpoint URL is never used.

The intended SSH path supports sync, scripts, fresh PR
checkouts, captures, downloads, Actions hydration, tunnels, desktop, and browser
commands when their guest prerequisites are present; these features have not
been validated in production for Boxd. Readiness checks bash,
git, rsync, and tar. `doctor` verifies HTTPS authentication and inventory without
creating a VM; it does not prove guest bootstrap or SSH readiness.

## Lifecycle and recovery

The following commands describe the implementation's intended lifecycle, not
a validated production workflow. The isolation limitation above still applies.

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
claimed immutable VM by default and verifies sustained inventory absence
before removing its claim. `boxd.deleteOnRelease: false` instead stops it and
retains both disk and claim; reuse restarts it. That release intent persists
across processes unless explicitly overridden. Reuse and ordinary heartbeat
preserve the stored idle timeout; an explicit heartbeat override changes it.
Local TTL and idle expiry require a later `crabbox cleanup`; there is no Boxd
server-side Crabbox janitor. Cleanup honors kept leases and supports dry-run.

Failed bootstrap rolls back under an independent bounded cleanup context,
even after caller cancellation. `--keep` and `--keep-on-failure` preserve their
normal Crabbox semantics. Cleanup failure retains the immutable ownership
claim for retry. If create receives a definite HTTP 400, 401, 403, or 422
rejection, Crabbox removes its unchanged pending intent and reports the rejection. Transport
failures, server errors, or success responses without a valid immutable ID
retain an ambiguous intent with the requested machine name. Inspect the
HTTPS console and local claim before manual recovery: Crabbox cannot safely
infer ownership from a matching name. It will not automatically delete such
an unbound resource or discard its recovery claim.

## Live smoke and current verification limits

The full smoke cannot pass the current production isolation check. Once the
vendor honors the isolation contract, run it with an interactive session in the environment:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=boxd scripts/live-smoke.sh
```

The smoke uses normal Crabbox doctor, warmup, status, inspect, run, stop, and
cleanup dry-run commands. It also runs a one-shot command that exits 37 and
checks that the failure code survives release. A separate Python HTTPS client
records inventory before the run and checks for new remaining Crabbox machines
after success and on failure. It never deletes resources independently. Run
this smoke without concurrent Boxd acquisitions in the same account/org, since
those would correctly be reported as new inventory residue. Routing comes
only from explicit environment variables for both clients; no login or signup
is attempted.

Local TLS/WSS tests cover the adapter and device helper. Live investigation
confirmed HTTPS device authorization, `whoami`, inventory arrays and machine
identity fields, and the rejection of API-key JWTs for console mutations.
Interactive creation returned `{name,public_ip,status,url,vm_id}`, but inventory
reported `isolated: false` despite the requested `isolated: true`. The actual
Crabbox run rejected that VM before guest commands, key installation, port
exposure, or sync. With `--keep=false`, automatic rollback deleted only its
immutable VM ID; independent inventory confirmed absence, preservation of the
preexisting VM, and no remaining local claim.

Additional HTTPS fork and snapshot-restore probes also returned non-isolated
VMs despite `isolated: true`. The vendor's current protobuf and SDK contracts
support requesting isolation when creating, forking, or restoring a VM, but
not changing an existing VM in place. None of those tested HTTPS creation
paths supplied a working alternative. All task-created VMs and the test
snapshot were removed, with preexisting resources preserved.

The public CLI, SDKs, and example repositories also did not establish a secure
production gRPC alternative: the documented endpoint uses plaintext port 9443,
and credential-free TLS probes did not find a working gRPC TLS listener at the
tested production endpoints. This adapter does not downgrade to that transport.
The shipped console bundle supplies the intended raw-port and terminal framing
contracts. Production behavior remains unverified for isolated creation, org ownership
semantics, raw forwarding, guest sudo/sshd prerequisites, and terminal completion
after shell `exec`. Do not treat local fake-server tests as live
provider proof. See the [vendor command documentation](https://docs.boxd.sh/cli/commands)
and [protobuf reference](https://docs.boxd.sh/reference/grpc-proto) for vendor context;
the latter is not the transport used by this integration.
