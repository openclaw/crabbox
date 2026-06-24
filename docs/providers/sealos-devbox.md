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

## Current Limits

`warmup`, `run`, `ssh`, `status`, `stop`, and `cleanup` are not implemented for
`sealos-devbox` in this foundation phase. They return explicit deferred errors
until the later lifecycle and SSH phases land.
