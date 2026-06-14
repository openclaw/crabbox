# Bring Your Own Infrastructure

Read this when an organization already has a VM, container, devbox, lab, or
sandbox control plane and wants to use Crabbox without adding organization-
specific code to the public project.

The composition has two independent choices:

1. integrate the private control plane as a direct Crabbox provider;
2. optionally register those direct leases with a coordinator.

The provider remains responsible for provisioning and deletion. Crabbox owns
lease naming, local routing, SSH, repository sync, command execution, and
desktop tunnels. In registered mode, the coordinator adds inventory, sharing,
and outbound browser bridges without receiving provider credentials. Direct
provider runs are not recorded by the coordinator in registered mode.

## Choose an integration

| Existing control plane | Recommended Crabbox integration |
| --- | --- |
| Deterministic CLI create/delete commands and SSH names | declarative `external.lifecycle` |
| Arbitrary logic or structured provider metadata | external JSON protocol executable |
| Slurm allocation that publishes an SSH endpoint from a scheduled job | external JSON protocol executable |
| KubeVirt cluster | built-in `kubevirt` provider |
| Existing SSH hosts | built-in `ssh` provider |
| New reusable public provider | implement a normal provider adapter |

Start with configuration. Add code only when the provider contract cannot be
expressed safely through deterministic commands or the versioned protocol.

## Ownership boundary

```text
operator machine                     private control plane
----------------                     ---------------------
Crabbox CLI ---- lifecycle argv ---> create / inspect / delete
     |
     +---------- SSH + rsync ------> leased machine
     |
     +-- optional HTTPS/WS --------> coordinator
```

The private tool owns its authentication and resource API. Do not place its
tokens in Crabbox repository config. Crabbox may pass explicitly named secrets
through an operation environment, but it never needs to understand them.

## Declarative external provider

Use declarative lifecycle configuration when resource names and SSH routing
are predictable:

```yaml
provider: external
target: linux

external:
  lifecycle:
    doctor:
      argv: [devboxctl, list, --format, json]
    acquire:
      steps:
        - [devboxctl, new, "{{resourceName}}", --size, "{{config.size}}"]
        - [devboxctl, configure, "{{resourceName}}"]
      rollbackOnFailure: true
    list:
      argv: [devboxctl, list, --format, json]
      output: json-name-array
      namePrefix: cbx-
    release:
      argv: [devboxctl, rm, --yes, "{{resourceName}}"]

  connection:
    resourceName: "{{leaseIdSlug}}"
    cloudId: "devboxes/{{resourceName}}"
    ssh:
      host: "{{resourceName}}"
      user: developer
      port: "22"
      sshConfigProxy: true
      readyCheck: command -v git && command -v rsync && command -v tar

  config:
    size: cpu16
  workRoot: /home/developer/crabbox
```

Lifecycle entries are argv arrays, not shell strings. Pipes, substitutions,
redirections, and shell expansion are not evaluated. Multi-step operations run
in order and stop on the first error. `rollbackOnFailure` invokes `release`
after a later acquire step fails, unless the caller explicitly keeps the
resource for diagnosis.

Use operation `env:` entries for credentials:

```yaml
external:
  lifecycle:
    acquire:
      argv: [devboxctl, new, "{{resourceName}}"]
      env:
        DEVBOX_TOKEN: "{{env.DEVBOX_TOKEN}}"
```

Secret environment values are rejected in argv by default because process
arguments can be visible to other local users. Keep that protection enabled.

## External protocol executable

Use the versioned JSON protocol when the adapter must perform arbitrary logic,
discover non-deterministic connection details, or return richer diagnostics.
Crabbox sends one JSON request on stdin and expects one JSON response on stdout.
Diagnostics belong on stderr.

The executable supports `doctor`, `acquire`, `resolve`, `list`, `release`,
`touch`, and `cleanup`. It receives arbitrary non-secret `external.config`, the
desired Crabbox lease identity, and repository metadata. It returns a lease
record containing its own `cloudId` plus SSH host, user, port, key or proxy
route, and readiness check.

Keep the executable separately versioned when it is proprietary. Public
Crabbox needs only the protocol contract documented in
[External Provider](../providers/external.md).

Academic Slurm clusters usually fit this protocol path: the adapter submits an
`sbatch` job, waits for the allocation to publish a host/port/key or proxy
route, returns that as the external SSH target, and uses `scancel` on release.
See [Slurm academic sandboxes](slurm-academic-sandboxes.md) for the full
product and security contract, and
[examples/slurm-external-provider](../../examples/slurm-external-provider/README.md)
for a reference external adapter.

## Persisted routing

External leases keep two pieces of private local state on the operator machine:

- the lease claim stores the canonical lease, resolved cloud/resource identity,
  and resolved SSH target;
- the mode-`0600` routing file stores the provider command or lifecycle,
  non-secret config, connection templates, provider scope, and work root.

Generated retry, SSH, WebVNC, and stop commands use the claim and its opaque
routing reference instead of embedding private configuration on the command
line. Preserve both. For a nondeterministic protocol adapter, the routing file
alone may not contain enough resolved identity to release the resource.

Routing is scoped by the selected lifecycle or protocol command and config.
Two control planes may therefore use the same friendly slug without claiming
or deleting each other's resources. Ambiguous persisted routes fail closed.
Missing routing does not always fail closed: when the external provider is
explicitly selected, Crabbox can fall back to the current external config as an
ownership override. Before a destructive command in that state, verify the
current provider scope and resource identity or restore the original routing;
never assume a reused slug identifies the same control plane.

## Registered coordinator mode

Registration is optional and provider-neutral:

```yaml
broker:
  url: https://broker.example.com
  mode: registered
  autoWebVNC: true
  token: <user-or-operator-token>
```

For short-lived identity tokens, prefer the shell-free command environment:

```sh
export CRABBOX_COORDINATOR_TOKEN_COMMAND='["credential-helper","read","crabbox-user-token"]'
```

The command is executed directly without a shell. It must print exactly one
token line, complete within 15 seconds, and stay within the output limit. The
CLI reruns it for HTTP requests and reconnecting WebSockets, so an expiring
token does not need to be persisted. Its output must be a bearer the
coordinator accepts, such as its shared/admin token or a signed `cbxu_` user
token. An audience-scoped token from another identity system works only when an
upstream identity proxy validates it and injects the coordinator's configured
trusted-user header.

When a direct lease is claimed, the CLI idempotently registers generic
metadata:

- canonical lease ID and friendly slug;
- provider and target OS;
- desktop, browser, and code capabilities;
- provider resource identity and server type;
- SSH destination metadata, never the private key contents;
- work root, TTL, idle timeout, profile, class, and exposed ports.

Registration is best effort for ordinary direct operation. A temporary
coordinator outage must not prevent SSH, sync, run, or provider cleanup. Portal
and sharing features require the registration to exist and will report the
coordinator error explicitly.

## Lifecycle in registered mode

The normal sequence is:

1. provision through the direct provider;
2. wait for SSH and claim the lease locally;
3. register or refresh coordinator metadata;
4. heartbeat during active commands and, for kept desktops, through the optional
   persistent WebVNC bridge daemon;
5. use SSH and rsync directly for commands;
6. stop the bridge and delete or retain the provider resource;
7. release the coordinator registration.

Coordinator expiry or release ends the active registration but retains the
record as `expired` or `released` history. It cannot invoke the direct provider
and never deletes the underlying resource. Provider cleanup remains the CLI
adapter's responsibility.

A retained headless lease has no persistent background heartbeat after the
command exits. Its coordinator record may reach idle expiry while the direct
provider resource remains live. A kept desktop bridge keeps heartbeating while
its daemon is connected.

Registered records are excluded from managed provider cost totals, ready
pools, image operations, and access reconciliation. Registration does not
universally exempt the underlying resource from provider-account orphan
sweeps. If direct resources use tags or accounts visible to a coordinator
sweep, isolate their ownership scope, disable destructive sweep mode, or add a
provider-specific exclusion before production use.

## Outbound WebVNC

Managed and external desktop integrations should keep VNC on target loopback.
With registered mode, a kept desktop may start an outbound bridge daemon:

```text
guest VNC on loopback
    <- SSH tunnel -> local Crabbox daemon
    <- outbound WebSocket -> coordinator
    <- authenticated viewer WebSocket -> browser
```

In this path the guest does not expose VNC publicly. The coordinator uses
short-lived one-use tickets to pair an agent and viewer.

Static `ssh` hosts have an explicit direct-endpoint fallback: if loopback VNC
is unavailable but `<host>:5900` is reachable, the local daemon may connect to
that endpoint without an SSH tunnel. Use that mode only on a trusted private
network with firewall isolation and host-managed VNC authentication; it does
not provide SSH transport protection. Prefer loopback VNC when possible.

Use:

```sh
crabbox webvnc daemon status --id cbx_0123456789ab
crabbox webvnc status --id my-box
crabbox webvnc reset --id my-box
```

The daemon subcommand reads local PID/log state keyed by canonical `cbx_...`
lease ID and does not resolve a friendly slug. Obtain that ID from `inspect` or
`list`.

## Sharing

Registered leases use the normal coordinator share model:

```sh
crabbox share --id my-box --user alice@example.com
crabbox share --id my-box --org
crabbox share --id my-box --list
crabbox unshare --id my-box --user alice@example.com
```

`use` grants portal visibility and available bridges. `manage` also grants
sharing changes and coordinator-side stop of the registration. Sharing never
copies an SSH private key and never grants access to the private provider API.

## Failure behavior

| Failure | Expected result |
| --- | --- |
| Provider create fails before a resource exists | acquisition fails; no registration |
| Later acquire step fails | optional rollback invokes direct release |
| Provider response is lost after create | adapter-specific resolve/readiness may recover the named resource |
| Coordinator registration fails | warning; direct lease remains usable |
| Bridge disconnects | daemon reconnects; direct SSH remains usable |
| Coordinator release succeeds | record becomes inactive history; provider resource remains |
| Provider cleanup fails | local routing remains for retry |
| Persisted routing is ambiguous | destructive operation fails closed |
| Routing is missing and current external config is selected | operation may proceed as an ownership override; verify provider scope and resource identity first |

## Security checklist

- Keep private control-plane credentials in its normal credential store or
  operation environment, never repository YAML.
- Use argv arrays and leave shell evaluation disabled.
- Keep routing and user config files mode `0600`.
- Never put secret environment placeholders in lifecycle argv.
- Keep managed/external VNC loopback-only and use SSH plus the outbound bridge;
  separately secure any explicit static-host direct endpoint.
- Configure coordinator authentication independently from provider auth.
- Share portal capabilities only with authenticated identities.
- Verify stop removes the provider resource before deregistration is treated as
  complete.

## Validation checklist

Before adopting an integration, prove:

```sh
crabbox doctor --provider external
crabbox warmup --provider external --slug integration-smoke --desktop --keep
crabbox inspect --provider external --id integration-smoke --json
crabbox run --provider external --id integration-smoke --preflight -- uname -a
crabbox webvnc status --provider external --id integration-smoke
crabbox stop --provider external integration-smoke
```

Also verify a changed local config can still stop an existing lease through
persisted routing, a coordinator outage does not block direct cleanup, and
inventory no longer contains the resource after stop. To prove acquisition
rollback, use a disposable config whose second acquire step fails and run:

```sh
crabbox warmup --provider external --slug rollback-smoke --keep=false
```

Record provider inventory before the command, capture the generated
`{{resourceName}}` from lifecycle output, then verify the release operation ran,
that exact resource is absent, and inventory returned to its baseline. The
friendly slug is not necessarily the provider resource name. When the
coordinator can enumerate the same provider account, also prove its orphan
sweep cannot select a live registered direct resource.

## Related documentation

- [External Provider](../providers/external.md)
- [Slurm academic sandboxes](slurm-academic-sandboxes.md)
- [Reference Slurm external provider](../../examples/slurm-external-provider/README.md)
- [KubeVirt Provider](../providers/kubevirt.md)
- [Coordinator](coordinator.md)
- [Portable Coordinator](portable-coordinator.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Share command](../commands/share.md)
- [Lifecycle cleanup](lifecycle-cleanup.md)
