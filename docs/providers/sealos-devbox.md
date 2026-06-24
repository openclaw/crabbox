# Sealos DevBox Provider

`provider: sealos-devbox` is the foundation for a direct Linux SSH-lease
provider backed by Sealos DevBox. This phase registers the provider, wires its
typed CLI/config surface, and adds a read-only doctor. Creating, resolving,
stopping, deleting, SSH target construction, and cleanup are intentionally
deferred to later implementation phases.

## Automation Surface Decision

Decision: `crd_first`.

The local Sealos source evidence exposes `devbox.sealos.io/v1alpha2` Devbox
CRDs with `Running`, `Paused`, `Stopped`, and `Shutdown` states, plus
`SSHGate` and `NodePort` network modes. The Sealos SSHGate source watches
Devbox Secrets and Pods labeled as DevBox resources and routes SSH by public
key. Current public docs emphasize dashboard and IDE/plugin flows, so mutating
lifecycle code must continue to validate the CRD contract before it creates or
updates live DevBoxes.

## Configuration

```yaml
provider: sealos-devbox
target: linux
sealosDevbox:
  kubectl: kubectl
  kubeconfig: ""
  context: sealos
  namespace: default
  image: ""
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

Environment overrides use `CRABBOX_SEALOS_DEVBOX_*`:

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

Flags use the `--sealos-devbox-*` prefix with the same field names. Local path
expansion applies only to host-side path fields such as `kubectl` and
`kubeconfig`; `workRoot` is a guest path and is not shell-expanded.

## Doctor

Run:

```sh
crabbox doctor --provider sealos-devbox
crabbox doctor --provider sealos-devbox --json
```

Doctor is read-only. It checks:

- local `kubectl` client availability;
- configured context and namespace readability;
- the Devbox CRD;
- read/list permissions for DevBoxes, Secrets, Pods, and Events;
- create/update/delete capability only through `kubectl auth can-i` dry
  permission checks;
- `SSHGate` host/port or `NodePort` node host configuration.

Doctor never creates, updates, patches, stops, deletes, or reads Secret data.
Kubeconfig contents, tokens, Secret data, and private keys must not be printed.

## Lifecycle Foundation

The CRD-first lifecycle path creates Crabbox-owned `devbox.sealos.io/v1alpha2`
Devbox resources with deterministic labels and annotations for the lease ID,
slug, provider, provider scope, namespace/name, timestamps, TTL, idle timeout,
and configured network route. Local claims are scoped by Kubernetes identity
and the selected route (`SSHGate` gateway host/port or `NodePort` node host), so
the same slug can be reused safely across different Sealos clusters,
namespaces, contexts, or routes.

`status` and `list` inspect Devbox resources and normalize Sealos lifecycle
states such as `Running`, `Pending`, `Paused`, `Stopped`, `Shutdown`, and
`Error` without starting, patching, deleting, or reading Secret key data.
Diagnostic text includes phase, conditions, and recent events when available,
with sensitive-looking values redacted.

When Sealos publishes the owned SSH Secret, Crabbox reads
`SEALOS_DEVBOX_PUBLIC_KEY` and `SEALOS_DEVBOX_PRIVATE_KEY`, writes the private
key to the normal per-lease local key store with restrictive permissions, and
keeps key material out of command arguments, logs, claim JSON, and status
output.

## SSH, Release, and Cleanup

Crabbox returns a normal Linux SSH lease for `sealos-devbox`, so existing
`crabbox run`, `crabbox ssh`, `crabbox status --json`, `crabbox stop`, and
`crabbox cleanup --dry-run` behavior stays provider-neutral after the adapter
has resolved a DevBox.

`SSHGate` is the default network mode. Crabbox connects to
`sealosDevbox.sshGatewayHost:sealosDevbox.sshGatewayPort` as
`sealosDevbox.sshUser` using the Sealos-managed DevBox private key stored in
Crabbox's local per-lease key path. This relies on SSHGate public-key routing;
username-encoded SSHGate routing is not the default. `NodePort` is available
as a fallback mode when `sealosDevbox.nodeHost` is configured and Sealos
reports an SSH NodePort in the DevBox status.

Before handing a lease to the normal SSH runner, Crabbox waits for SSH and
verifies `git`, `rsync`, and `tar` on the remote host. `status --json` may show
the configured route without probing SSH or reading Secret data.

`crabbox stop` retains a DevBox by patching its Sealos state to `Paused`,
clearing the live SSH endpoint from the local claim, and keeping the local
claim so the lease can be resolved later. When
`sealosDevbox.deleteOnRelease: true` or `--sealos-devbox-delete-on-release` is
set, release validates the DevBox identity, marks it for shutdown, deletes the
DevBox CR, and removes the matching local claim and generated key.

`crabbox cleanup --provider sealos-devbox --dry-run` lists only Crabbox-owned
DevBoxes in the active kubeconfig/context/namespace/route scope and prints the
candidate or skip reason without mutating resources. Non-dry-run cleanup
revalidates identity immediately before deleting an expired owned DevBox and
removes stale local claims only after a refreshed inventory proves the DevBox
is absent in the active scope.
