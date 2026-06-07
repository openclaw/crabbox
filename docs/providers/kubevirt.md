# KubeVirt Provider

Use `provider: kubevirt` for generic Linux virtual machines managed by a
Kubernetes cluster with KubeVirt installed. The adapter uses only `kubectl`,
`virtctl`, and standard KubeVirt resources. It contains no organization-specific
API, naming, authentication, or network policy.

Crabbox applies a `VirtualMachine` manifest, starts and stops it with `virtctl`,
and reaches guest SSH through:

```text
virtctl port-forward --stdio=true vm/<name>/<namespace> %p
```

That ProxyCommand is used by normal SSH, rsync, command execution, and native
VNC forwarding. The guest does not need a public IP or Kubernetes `Service`.

## Prerequisites

- a Kubernetes context with KubeVirt access;
- `kubectl` and a compatible `virtctl`;
- a KubeVirt release compatible with the Kubernetes minor version running in the
  target cluster;
- permission to create, list, start, stop, and delete `VirtualMachine`
  resources in the configured namespace;
- permission to get `VirtualMachineInstance` resources and read namespace
  events. Crabbox uses these while booting so KubeVirt scheduling or launcher
  failures are reported before the SSH wait starts;
- a Linux guest image with OpenSSH, `git`, `rsync`, and `tar`.

## Configuration

```yaml
provider: kubevirt
target: linux
kubevirt:
  kubectl: kubectl
  virtctl: virtctl
  kubeconfig: ""
  context: my-cluster
  namespace: default
  template: ./kubevirt-vm.yaml
  sshUser: crabbox
  sshKey: ""
  sshPublicKey: ""
  sshPort: "22"
  workRoot: /home/crabbox/crabbox
  deleteOnRelease: true
```

When `sshKey` is empty, Crabbox generates a per-lease key. When it is set,
`sshPublicKey` may contain the matching public key text or a public-key file
path; otherwise Crabbox reads `<sshKey>.pub`.

Provider flags use the `--kubevirt-*` prefix. Environment overrides use the
`CRABBOX_KUBEVIRT_*` prefix.

Local path fields expand `~` from config files, environment overrides, and
flags. This applies to `kubectl`, `virtctl`, `kubeconfig`, `template`,
`sshKey`, and file-form `sshPublicKey`. `workRoot` is a guest path and is not
shell-expanded.

Local lease claims are scoped by the same kubeconfig, context, and namespace
tuple that Crabbox passes to `kubectl` and `virtctl`. `kubevirt.context` is
required so claims cannot drift when a kubeconfig's current context changes.
When `kubevirt.kubeconfig` is empty, the scope uses the inherited `KUBECONFIG`
value; when both are empty it uses kubectl's default kubeconfig path. This
allows the same slug to exist in different namespaces or clusters without
`status`, `run`, `ssh`, or `stop` resolving the wrong VM. Generated `stop` and
failure-retry commands preserve that inherited `KUBECONFIG` value as an
environment assignment so cleanup uses the same cluster later, including
multi-file kubeconfig lists.

Claims written by older Crabbox builds without a scope are treated as legacy
state. New slug allocation checks the live VMs in the current namespace before
reusing a slug, so old claims attached to still-running VMs continue to prevent
duplicates. Once the VM is deleted by `stop` or `cleanup`, Crabbox removes the
legacy claim.

## VM template

The template must contain exactly one `kubevirt.io/v1` `VirtualMachine` and
must use `runStrategy: Manual`. Crabbox overwrites `metadata.name` and
`metadata.namespace`, adds lease labels and cleanup annotations, and replaces these string
placeholders anywhere in the YAML:

```text
{{NAME}}
{{NAMESPACE}}
{{LEASE_ID}}
{{SLUG}}
{{SSH_PUBLIC_KEY}}
```

Minimal SSH-ready example:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: replaced-by-crabbox
spec:
  runStrategy: Manual
  template:
    spec:
      domain:
        cpu:
          cores: 4
        resources:
          requests:
            memory: 8Gi
        devices:
          disks:
            - name: root
              disk:
                bus: virtio
            - name: cloudinit
              disk:
                bus: virtio
          interfaces:
            - name: default
              masquerade: {}
              ports:
                - name: ssh
                  port: 22
                  protocol: TCP
      networks:
        - name: default
          pod: {}
      volumes:
        - name: root
          containerDisk:
            image: quay.io/containerdisks/ubuntu:22.04
        - name: cloudinit
          cloudInitNoCloud:
            userData: |
              #cloud-config
              ssh_pwauth: false
              users:
                - name: crabbox
                  sudo: ALL=(ALL) NOPASSWD:ALL
                  shell: /bin/bash
                  ssh_authorized_keys:
                    - {{SSH_PUBLIC_KEY}}
              package_update: false
              packages:
                - git
                - openssh-server
                - rsync
                - tar
              runcmd:
                - systemctl enable --now ssh || systemctl enable --now sshd
                - mkdir -p /home/crabbox/crabbox
                - chown -R crabbox:crabbox /home/crabbox
```

The cloud-init user must match `kubevirt.sshUser`. If the template uses
`masquerade`, declare the SSH port under `interfaces[].ports`; otherwise
`virtctl port-forward` can reach the VM object while the guest port still
refuses connections.

## Lifecycle

1. Render and apply the configured manifest.
2. Run `virtctl start`.
3. Wait for the `VirtualMachineInstance` to exist and reach a state that can be
   SSH-probed. `Running` and `Ready` pass immediately; `Scheduled` also passes
   because some KubeVirt clusters can leave the VMI phase stale while the
   domain is already running and `virtctl port-forward` works.
4. Wait for SSH through the Kubernetes API server forwarding path.
   If SSH never becomes ready, Crabbox includes the latest VMI phase,
   conditions, and recent KubeVirt events in the error.
5. Use the standard Crabbox SSH lease flow.
6. On release, delete the VM by default. Set `deleteOnRelease: false` to run
   `virtctl stop` and retain its storage.

`crabbox cleanup --provider kubevirt` evaluates the persisted lease annotations
with Crabbox's normal TTL/idle policy and deletes eligible labeled VMs. Use
`--dry-run` to inspect decisions.

`crabbox status` and `crabbox inspect` are read-only. They report the VM's
KubeVirt printable status and do not start a stopped retained VM. Commands that
need an SSH target, such as `run`, `ssh`, and `code`, resolve the lease and
start a retained VM before waiting for SSH.

```sh
crabbox doctor --provider kubevirt
crabbox warmup --provider kubevirt --slug vm-smoke
crabbox run --provider kubevirt --id vm-smoke -- go test ./...
crabbox vnc --provider kubevirt --id vm-smoke --open
crabbox stop --provider kubevirt vm-smoke
```

For the repository live harness:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=kubevirt \
CRABBOX_LIVE_KUBEVIRT_TEMPLATE=./kubevirt-vm.yaml \
scripts/live-smoke.sh
```

The live harness also needs `jq` and `rg` on the operator machine. Use
`CRABBOX_LIVE_KUBEVIRT_CONTEXT` or `kubevirt.context` must name the target
Kubernetes context. Use `CRABBOX_LIVE_KUBEVIRT_NAMESPACE` when the configured
namespace is not the target namespace. `CRABBOX_LIVE_COMMAND` overrides the
remote smoke command; by default it accepts either a Go repo (`go.mod`) or a
Node repo (`package.json`).

## Troubleshooting

- `timed out waiting for KubeVirt VMI ... to be scheduled for SSH probing`:
  inspect the phase, Ready condition, and events printed by Crabbox. Common
  causes are image pull failures, unschedulable resource requests, missing
  KubeVirt permissions, and launcher/runtime problems.
- `KubeVirt SSH did not become ready ... KubeVirt VMI diagnostics`: the VM was
  schedulable, but SSH did not accept connections through `virtctl
  port-forward`. Check the appended VMI diagnostics plus guest boot and
  cloud-init logs.
- `virtctl port-forward ... connect: connection refused`: the VM object exists
  and the control-plane tunnel opened, but nothing is accepting on the target
  guest port. Check the cloud-init user data, OpenSSH service, and
  `interfaces[].ports` when using masquerade networking.
- VMI phase remains stale while `virtctl port-forward` never returns an SSH
  banner: check Kubernetes/KubeVirt version compatibility. Unsupported minor
  combinations can prevent KubeVirt from updating VMI status and from wiring
  port-forward correctly.
- A slug works in one namespace but not another: confirm the command uses the
  intended `--kubevirt-kubeconfig` or `KUBECONFIG`,
  `--kubevirt-context`, and `--kubevirt-namespace`. Local claims are scoped to
  that routing tuple; a claim from a different tuple is intentionally ignored
  and Crabbox falls back to the VM labels visible in the current namespace.
- Local macOS ARM clusters may not be suitable for KubeVirt smoke tests. Some
  kind/OrbStack setups cannot run x86_64 container disks and KubeVirt currently
  restricts arm64 CPU model selection. Use a Linux node with `/dev/kvm`, or set
  KubeVirt `useEmulation` only for slow CI diagnostics.
