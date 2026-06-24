# Sealos DevBox Provider

`provider: sealos-devbox` provisions or reuses Sealos DevBox resources as
direct Linux SSH leases. Crabbox owns the DevBox lifecycle through the
`devbox.sealos.io/v1alpha2` Kubernetes CRD, waits for the Sealos SSH route,
then uses the normal SSH sync, run, status, stop, and cleanup paths.

Sealos DevBox is direct-only. It never routes through the Crabbox coordinator.
The operator's local Kubernetes credentials, RBAC, Sealos SSHGateway, quota, and
DevBox templates remain under Sealos and cluster control.

## Requirements

- `kubectl` installed and on `PATH`, or configured with
  `sealosDevbox.kubectl`.
- A Kubernetes context that can read the configured namespace and the
  `devboxes.devbox.sealos.io` CRD.
- RBAC to get/list DevBoxes, Secrets, Pods, and Events.
- RBAC to create, update, and delete DevBoxes when running `warmup`, `run`,
  `stop`, or non-dry-run `cleanup`.
- Either `sealosDevbox.image` or `sealosDevbox.templateID`.
- A Sealos DevBox image or template with OpenSSH, `git`, `rsync`, and `tar`.
- For `network: SSHGate`, a reachable Sealos SSHGateway host and port.
- For `network: NodePort`, `sealosDevbox.nodeHost` and a DevBox status shape
  that exposes an SSH NodePort.

Run the read-only preflight before creating resources:

```sh
crabbox doctor --provider sealos-devbox
crabbox doctor --provider sealos-devbox --json
```

Doctor checks local `kubectl`, context, namespace, CRD availability, read RBAC,
mutating RBAC through `kubectl auth can-i`, and route configuration. It does
not create, patch, stop, delete, or read Secret data.

## Config

```yaml
provider: sealos-devbox
target: linux
sealosDevbox:
  kubectl: kubectl
  kubeconfig: ""
  context: sealos
  namespace: default
  image: ubuntu:24.04
  templateID: ""
  cpu: "2"
  memory: 4Gi
  storageLimit: 20Gi
  network: SSHGate
  sshGatewayHost: ssh.sealos.example.com
  sshGatewayPort: "2222"
  sshUser: devbox
  workRoot: /home/devbox/project
  nodeHost: ""
  deleteOnRelease: false
```

Environment overrides:

```text
CRABBOX_SEALOS_DEVBOX_KUBECTL
CRABBOX_SEALOS_DEVBOX_KUBECONFIG
CRABBOX_SEALOS_DEVBOX_CONTEXT
CRABBOX_SEALOS_DEVBOX_NAMESPACE
CRABBOX_SEALOS_DEVBOX_IMAGE
CRABBOX_SEALOS_DEVBOX_TEMPLATE_ID
CRABBOX_SEALOS_DEVBOX_CPU
CRABBOX_SEALOS_DEVBOX_MEMORY
CRABBOX_SEALOS_DEVBOX_STORAGE_LIMIT
CRABBOX_SEALOS_DEVBOX_NETWORK
CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST
CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_PORT
CRABBOX_SEALOS_DEVBOX_SSH_USER
CRABBOX_SEALOS_DEVBOX_WORK_ROOT
CRABBOX_SEALOS_DEVBOX_NODE_HOST
CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE
```

Flags use the `--sealos-devbox-*` prefix with the same field names, for
example `--sealos-devbox-context`, `--sealos-devbox-template-id`, and
`--sealos-devbox-delete-on-release`.

Local path expansion applies to host-side path fields such as `kubectl` and
`kubeconfig`. `workRoot` is a guest path and is not shell-expanded.

## Lifecycle

`warmup` and one-shot `run` create a Crabbox-owned DevBox CR in the configured
namespace. The generated manifest sets `spec.state: Running`, resource size,
image or template ID, storage limit, network mode, SSH user, workdir, and an SSH
port entry. Crabbox adds deterministic labels and annotations for the provider,
lease ID, slug, namespace, route scope, TTL, idle timeout, timestamps, and
release policy.

```sh
crabbox warmup --provider sealos-devbox --slug sealos-smoke --keep
crabbox run --provider sealos-devbox --id sealos-smoke -- go test ./...
crabbox status --provider sealos-devbox --id sealos-smoke --json
crabbox ssh --provider sealos-devbox --id sealos-smoke
crabbox stop --provider sealos-devbox sealos-smoke
crabbox cleanup --provider sealos-devbox --dry-run
```

Crabbox can resolve a Sealos lease by Crabbox lease ID, slug, or DevBox name
when it uniquely matches the current kubeconfig/context/namespace/route scope.
The same slug can exist safely in different Sealos environments because local
claims include that scope.

Status and list operations are read-only. They normalize Sealos states such as
`Running`, `Pending`, `Paused`, `Stopped`, `Shutdown`, and `Error`, include
recent diagnostics when available, and do not read Secret private-key data.
Commands that need SSH, such as `run` and `ssh`, resolve the live DevBox,
refresh the route and key, wait for SSH, and then enter the normal Crabbox SSH
runner.

## SSH

`SSHGate` is the default and recommended network mode when the Sealos
deployment exposes it:

```text
Host: sealosDevbox.sshGatewayHost
Port: sealosDevbox.sshGatewayPort
User: sealosDevbox.sshUser
IdentityFile: Crabbox local per-lease key path
```

Crabbox uses Sealos' public-key SSHGateway routing. It does not document or rely
on username-encoded SSHGateway routing as complete. That route remains a
separate proof gate for a future change.

`NodePort` is available as a fallback when `sealosDevbox.nodeHost` is
configured and the DevBox status exposes an SSH NodePort. Tailnet is visible in
Sealos source material but is not implemented or documented as a Crabbox
`sealos-devbox` route.

The ready check verifies `git`, `rsync`, and `tar` on the remote host before
Crabbox syncs or runs project commands.

## Secrets

Crabbox never accepts Sealos tokens on argv and should not store kubeconfig
contents, tokens, Secret data, or private keys in repository config.

When Sealos publishes the DevBox Secret, Crabbox reads only the key fields it
needs, writes the private key to the normal per-lease local key store with
restrictive permissions, and keeps key material out of command arguments, logs,
claims, and status output. `status --json` may show the local key path category
for the current lease, but it does not print key contents.

## Release And Cleanup

By default, `crabbox stop` retains the DevBox by patching the Sealos state to
`Paused`, clearing the live SSH endpoint from the local claim, and keeping the
claim so the lease can be resolved later.

Set `sealosDevbox.deleteOnRelease: true` or pass
`--sealos-devbox-delete-on-release` when the DevBox should be disposable. In
that mode, release validates the live DevBox identity, marks it for shutdown,
deletes the DevBox CR, and removes the matching local claim and generated key.

Cleanup is scope-safe:

- dry-run cleanup prints only Crabbox-owned candidates in the active
  kubeconfig/context/namespace/route scope;
- resources outside the active provider scope are skipped with a reason;
- non-dry-run cleanup revalidates identity immediately before deleting;
- stale local claims are removed only after a refreshed inventory proves the
  DevBox is absent in the active scope.

## Live Smoke

Run live smoke only in a Sealos environment where it is safe to create and stop
a short-lived DevBox:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=sealos-devbox \
CRABBOX_SEALOS_DEVBOX_CONTEXT=sealos \
CRABBOX_SEALOS_DEVBOX_NAMESPACE=default \
CRABBOX_SEALOS_DEVBOX_IMAGE=ubuntu:24.04 \
CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST=ssh.sealos.example.com \
scripts/live-smoke.sh
```

For `NodePort`, set `CRABBOX_SEALOS_DEVBOX_NETWORK=NodePort` and
`CRABBOX_SEALOS_DEVBOX_NODE_HOST=<node-host>` instead of the SSHGateway host.

The shared smoke refuses to mutate Sealos resources until local credentials,
context, namespace, image or template, RBAC, and route configuration are
present. Missing setup is classified as `environment_blocked`, for example
`missing_context`, `missing_image_or_template`, `missing_ssh_gateway_host`,
`doctor_failed`, or `missing_rbac_create_devboxes`.

When the prerequisites are present, the smoke runs `doctor`, dry-run cleanup,
`warmup --keep`, `status --json`, `run`, rendered SSH command proof, `stop`,
post-stop `status --json`, and final dry-run cleanup. Interactive shell proof
for a PR should be collected separately in a `tmux` session by running
`crabbox ssh --provider sealos-devbox --id <slug>` and recording a redacted pane
log that reaches the shell and exits cleanly.

## Troubleshooting

- `sealos-devbox context is required`: set `sealosDevbox.context` or
  `CRABBOX_SEALOS_DEVBOX_CONTEXT`; claims intentionally do not follow a
  kubeconfig's mutable current context.
- `sealos-devbox requires image or templateID`: set a DevBox image or template
  ID before `warmup` or `run`.
- `Sealos DevBox ... has no SSH NodePort in status.network`: use `SSHGate` or
  confirm the Sealos deployment publishes SSH NodePort status for the DevBox.
- SSH waits time out: run `doctor --provider sealos-devbox --json`, then inspect
  DevBox phase, related Pod events, the route mode, and whether the selected
  image/template includes OpenSSH, `git`, `rsync`, and `tar`.
- Cleanup skips a DevBox as outside scope: rerun cleanup with the same
  kubeconfig/context/namespace/network route that created the lease, or delete
  the provider resource manually after verifying ownership.
