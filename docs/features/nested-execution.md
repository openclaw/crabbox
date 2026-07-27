# Nested Execution

Read this when you need WSL2 or a container engine inside a Crabbox target, want
to run microVMs on a prepared KVM host, or are comparing those shapes with a
local sandbox.

“Nested box” can describe several different systems. They do not have the same
requirements or security boundary, so Crabbox should not expose them as one
universal promise.

## What Works Today

| Shape | Current Crabbox Path | Boundary |
| --- | --- | --- |
| Linux tooling inside a Windows VM | Managed AWS or Azure with `--target windows --windows-mode wsl2` | WSL2 on compatible nested-virtualization capacity |
| A container engine available inside the workload | Prepare it on an SSH/VM image, or opt into Local Container host socket pass-through | Depends on the engine; the socket controls the host engine and is often host-root-equivalent with a rootful daemon |
| A full VM or microVM on a prepared KVM host | Run Crabbox on that host with `--provider firecracker`; reaching a remote outer host does not add inner-VM lifecycle | Provider, instance family, host kernel, and policy dependent |
| A sandbox or VM on the developer machine | Use a local provider such as Local Container, Apple VM, Multipass, Hyper-V, Parallels, Windows Sandbox, or Docker Sandbox | Runs beside the CLI; not necessarily nested inside another Crabbox lease |

There is no provider-neutral `--nested` flag today. WSL2 is a first-class target
contract. Generic nested KVM, Hyper-V, or container-in-container capability is
not yet represented as one guaranteed provider feature.

Crabbox also does not expose a parent/child lease API. A runner cannot ask the
coordinator for bounded child Crabbox leases; that would be control-plane
delegation, not nested virtualization.

## WSL2 Inside a Managed Windows Box

AWS and Azure can provision Windows instances on VM families that expose the
virtualization support WSL2 needs:

```sh
crabbox warmup --provider aws \
  --target windows \
  --windows-mode wsl2

crabbox run --id blue-lobster -- uname -a
```

Crabbox enables the required Windows features, updates WSL, verifies the pinned
Ubuntu root filesystem checksum, imports it, reboots as needed, and proves the
POSIX contract by running the Linux-side readiness setup.

This is not available on every instance type or architecture. Defaults use the
documented WSL2 candidate lists, and AWS rejects unsupported explicit instance
types. An operator-selected exact Azure VM size remains operator-validated;
warmup fails if the selected size does not expose nested virtualization. The
first warmup is slower than a normal Linux lease because Windows features,
updates, and imports can require multiple reboots.

See [Windows VNC and WSL2](vnc-windows.md), [AWS](../providers/aws.md), and
[Azure](../providers/azure.md) for the supported families and current
limitations.

## Containers Inside a Box

There are three common patterns:

1. **Engine installed in a full VM.** Bake Docker, containerd, Podman, or another
   engine into the image and use a normal SSH lease. The inner containers share
   the leased VM's kernel.
2. **Host socket pass-through.** Local Container can mount the host Docker
   socket with `--local-container-docker-socket`. This is convenient for build
   and integration tests, but access to the socket is effectively control of
   the host engine. Do not present it as a hostile-code isolation boundary.
3. **Container-in-container.** A privileged inner daemon may work on a prepared
   host, but it needs kernel features, storage, network, and security policy that
   Crabbox does not currently standardize across providers.

Use a dedicated VM or microVM boundary when the inner workload must not control
the host container engine.

## KVM or MicroVMs Inside a Box

A Linux guest can launch KVM, Firecracker, QEMU/KVM, or another hardware-backed
runtime only when all outer layers expose the required virtualization
extensions. At minimum, verify:

```sh
test -r /dev/kvm
test -w /dev/kvm
```

The cloud instance family must support nested virtualization; the account and
image must allow it; the guest kernel must expose `/dev/kvm`; and the workload
still needs networking, images, storage, cleanup, and an isolation policy.

Crabbox's `firecracker` provider is currently a direct provider for a Linux KVM
host running the CLI. It does not automatically turn an arbitrary cloud lease
into a nested Firecracker host. Run Crabbox itself on the prepared Linux KVM
host with `--provider firecracker`. The `ssh` and `external` providers can reach
a prepared outer host, but inner virtualization must then be invoked and
managed by the workload; Crabbox has no generic remote nested-VM lifecycle.

## Local VM and Sandbox Options

Local providers solve a related problem without nesting everything inside a
remote lease:

- **Local Container** — Docker-compatible Linux containers with normal Crabbox
  SSH and sync.
- **Apple VM** — full ARM64 Linux VM through Apple's
  `Virtualization.framework` on Apple Silicon.
- **Multipass** — Canonical-managed local Ubuntu VM.
- **Hyper-V** — local native Windows VM on a compatible Windows host.
- **Parallels** — local Linux, macOS, native Windows, and preconfigured
  Windows/WSL2 templates with checkpoint and fork support.
- **Windows Sandbox, Docker Sandbox, Apple Container, and local policy
  runtimes** — delegated local execution with provider-specific boundaries.

Use:

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
```

to inspect the current local choices.

## Capability Boundary

Nested support is conditional on the target OS, architecture, instance type,
region, image, and outer-host policy. A provider-wide “nested virtualization”
claim would overstate AWS, Azure, or GCP support.

Today, request WSL2 explicitly and let warmup prove its POSIX readiness. For KVM
or Firecracker, run Crabbox on the prepared host and verify `/dev/kvm` there.
Crabbox has no generic nested-capability resolver or provider-neutral preflight.

## Security Notes

- Nesting does not automatically improve isolation. Every additional daemon,
  hypervisor, socket, and management API adds a boundary that must be patched
  and configured.
- Passing a host container socket into a box weakens the boundary even when the
  outer container looks disposable.
- A VM family advertising nested virtualization says that the CPU feature is
  exposed; it does not certify the inner runtime, network policy, or cleanup.
- Do not place provider credentials inside a leaf runner when the coordinator
  or an external lifecycle service can own them.

## External References

- [AWS EC2 nested virtualization](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html)
- [Google Compute Engine nested virtualization](https://cloud.google.com/compute/docs/instances/nested-virtualization/overview)
- [Microsoft nested Hyper-V](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/nested-virtualization)
- [Microsoft WSL FAQ](https://learn.microsoft.com/en-us/windows/wsl/faq)
- [Docker rootless mode](https://docs.docker.com/engine/security/rootless/)
- [Firecracker production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)

## Related Docs

- [Use Cases](../use-cases.md)
- [Provider Selection](provider-selection.md)
- [Windows VNC and WSL2](vnc-windows.md)
- [Local Container](../providers/local-container.md)
- [Firecracker](../providers/firecracker.md)
- [Security](../security.md)
