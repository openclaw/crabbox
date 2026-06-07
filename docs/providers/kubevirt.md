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

That ProxyCommand is used by normal SSH, rsync, command execution, and WebVNC
tunnels. The guest does not need a public IP or Kubernetes `Service`.

## Prerequisites

- a Kubernetes context with KubeVirt access;
- `kubectl` and a compatible `virtctl`;
- permission to create, list, start, stop, and delete `VirtualMachine`
  resources in the configured namespace;
- a Linux guest image with OpenSSH, `git`, `rsync`, and `tar`.

## Configuration

```yaml
provider: kubevirt
target: linux
kubevirt:
  kubectl: kubectl
  virtctl: virtctl
  kubeconfig: ""
  context: ""
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
`sshPublicKey` may contain the matching public key; otherwise Crabbox reads
`<sshKey>.pub`.

Provider flags use the `--kubevirt-*` prefix. Environment overrides use the
`CRABBOX_KUBEVIRT_*` prefix.

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

Minimal example:

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
              users:
                - name: crabbox
                  sudo: ALL=(ALL) NOPASSWD:ALL
                  shell: /bin/bash
                  ssh_authorized_keys:
                    - {{SSH_PUBLIC_KEY}}
              packages:
                - git
                - openssh-server
                - rsync
                - tar
```

The cloud-init user must match `kubevirt.sshUser`.

## Lifecycle

1. Render and apply the configured manifest.
2. Run `virtctl start`.
3. Wait for SSH through the Kubernetes API server forwarding path.
4. Use the standard Crabbox SSH lease flow.
5. On release, delete the VM by default. Set `deleteOnRelease: false` to run
   `virtctl stop` and retain its storage.

`crabbox cleanup --provider kubevirt` evaluates the persisted lease annotations
with Crabbox's normal TTL/idle policy and deletes eligible labeled VMs. Use
`--dry-run` to inspect decisions.

```sh
crabbox doctor --provider kubevirt
crabbox warmup --provider kubevirt --slug vm-smoke
crabbox run --provider kubevirt --id vm-smoke -- go test ./...
crabbox webvnc --provider kubevirt --id vm-smoke
crabbox stop --provider kubevirt vm-smoke
```
