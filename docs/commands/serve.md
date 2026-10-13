# serve

Donate spare container capacity to the configured coordinator using the existing
broker login. See the [provider reference](../providers/serve.md) for owner-only
placement, security, sizing, cache volumes, and network requirements.

```sh
crabbox serve --cpus 24 --memory 48g
crabbox serve --runtime podman --cpus 8 --memory 16g --advertise-address 100.64.1.2
crabbox serve machines --json
crabbox serve status --json
crabbox serve cache prune
crabbox serve install --cpus 24 --memory 48g
crabbox serve uninstall
```

| Agent flag | Meaning |
| --- | --- |
| `--cpus` | Offered CPU count; defaults to host logical CPUs |
| `--memory` | Required offered memory; MiB integer, `m`, or `g` suffix |
| `--max-leases` | Maximum concurrent containers, default 8, maximum 32 |
| `--name` | Machine display name; default hostname |
| `--advertise-address` | Repeatable client-reachable IPv4 (max 4); replaces automatic tailnet and LAN candidates |
| `--no-lan-candidates` | Disable automatic private LAN candidates |
| `--ssh-port` | Agent SSH front-door port, 1–65535; default 2222 |
| `--runtime` | `docker` or `podman` |
| `--poll-interval` | Minimum poll and heartbeat-response spacing, 500ms–10s; default 2s |
| `--state-dir` | Private machine identity/status directory |
| `--no-cache-volumes` | Disable managed cache volumes |
| `--drain-on-exit` | Remove containers on orderly agent shutdown |

`CRABBOX_SERVE_MEMORY` supplies the memory default. `serve.advertiseAddresses`
or comma-separated `CRABBOX_SERVE_ADVERTISE_ADDRESSES` configures explicit
addresses; the singular `CRABBOX_SERVE_ADVERTISE_ADDRESS` remains a fallback.
`serve.noLANCandidates` / `CRABBOX_SERVE_NO_LAN_CANDIDATES` disables LAN discovery.
`serve.sshPort` and `CRABBOX_SERVE_SSH_PORT` configure the single SSH front-door
port; `--ssh-port` takes precedence. Allow that one port in the host firewall and
tailnet policy when using tailnet routes. LAN routes bypass tailnet policy and
expose the key-authenticated front door to the LAN. Containers expose SSH only on loopback. The ordinary broker URL/token
configuration still applies; credentials are never printed or written into a
service command.

Serve requires coordinator enablement through `CRABBOX_SERVE_ALLOWED_OWNERS`;
unset or empty disables it. Unlisted owners receive `serve_not_enabled`.

Serve prepares a reusable local image in the background after its first heartbeat
and reconciliation to avoid reinstalling bootstrap packages for every lease.
See [prepared container images](../providers/serve.md#prepared-container-images)
for details, fallback behavior, and image evidence.

Each poll starts at least `--poll-interval` after the prior poll start and heartbeat response.

`status` checks coordinator access and reads the local machine UUID, last successful coordinator contact,
active assignments, caps, and draining state. `machines` queries owner-visible
coordinator inventory. Both support `--json`.

`install` writes and loads a per-user LaunchAgent on macOS or a systemd user
unit on Linux, preserving the supplied flags. It uses the current executable
path and account's existing broker configuration. `uninstall` unloads and
removes that service. Linux user services run while the user manager is active;
configure systemd lingering separately if required. Existing service files are
never overwritten implicitly. Default shutdown leaves containers until their
last deadlines; install with `--drain-on-exit` for immediate removal on stop.

`cache prune` requires the same state directory and runtime used by the agent,
and exclusive access to its state lock. Stop the agent first. It removes only
the named cache volumes derived for that machine and images labeled
`crabbox.serve.prepared=true` that no running container uses. Prepared images
may be shared by several agents. Removal is never forced; a runtime refusal
is reported. An idle agent whose prepared image was pruned uses its configured
image with lease-time bootstrap and starts background re-preparation on its next
lease; no agent restart is required.
