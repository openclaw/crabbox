# Use Cases

Read this when you know what needs to happen but do not yet know which Crabbox
provider or execution model fits.

Start with the workload, not the vendor:

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

With no use case, `crabbox providers recommend` prints every supported
recommendation name. Recommendations use checked-in capability metadata. They
do not contact providers, confirm credentials or quota, benchmark startup time,
or certify a security boundary.

## Speed Up Test and Build Loops

Use this when a repository command is too slow, memory-hungry, or disruptive
locally.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

Prefer `fast-feedback` for local runtimes, reusable caches, and quick edit-run
loops. Prefer `linux-vm` when execution should move off the workstation or full
VM and SSH semantics matter.

## Give a Coding Agent a Disposable Environment

Use a delegated or local sandbox when an agent needs to execute generated code,
install dependencies, or inspect a repository away from the main checkout—or,
with a remote provider, away from the workstation.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

These are three different questions:

- `agent-sandbox` favors providers shaped for agent and devbox execution;
- `disposable-execution` requires a cleanup-capable temporary runtime;
- `isolated-execution` favors delegated and local sandbox boundaries.

The last recommendation is routing guidance, not a provider security
certification. Read the selected provider's trust and network model before
running hostile code or attaching secrets.

## Validate Linux, Windows, WSL2, or macOS

Use platform recommendations when behavior depends on the target operating
system:

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

Native Windows runs PowerShell and Windows tooling. WSL2 runs the POSIX contract
inside a nested-capable Windows VM. macOS targets may be local Apple Silicon VMs
or cloud Mac capacity, depending on the provider.

## Test a Browser or Visible Desktop

Use a desktop-capable SSH lease when a test needs a browser, screenshots, input,
or a handoff to a teammate:

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --desktop
crabbox webvnc --id blue-lobster
```

Pair it with evidence routing when the result must survive after the box:

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## Reuse Warm State

Use a warm box when repeated setup costs more than keeping or restoring prepared
state:

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` uses signals such as local runtime, cache volumes, retained
sessions, pause/resume, and workspace state. It does not promise the same
snapshot or warm-pool primitive on every provider.

## Fan Out Parallel Experiments

Use fork-capable workspace providers for parallel branches, best-of-N agent
attempts, or test shards that should start from prepared state:

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

This is provider-neutral checkpoint and fork routing. It is not a guarantee of
live-memory microVM forking.

## Run GPU Workloads

Use the GPU recommendation for model tests, CUDA builds, rendering, or other
accelerator-shaped work:

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

SSH-lease GPU providers are usually better for interactive debugging. Delegated
GPU providers are often a better fit when the provider should own execution
and result collection.

## Run Locally Without Cloud Credentials

Use a local container, VM, or policy sandbox for a fast smoke test that stays on
the workstation:

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

Local does not mean one isolation model. A Docker-compatible container, Apple
VM, Multipass VM, Windows Sandbox, and a local policy runtime have different
host access, persistence, and cleanup contracts. Read the selected provider
page before running unfamiliar code.

## Use Infrastructure You Already Own

Crabbox can use a durable SSH host, private virtualization stack, or direct
cloud account without a shared coordinator:

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Use `ssh` for a host whose lifecycle Crabbox must not own. Use a self-hosted
provider such as Proxmox, XCP-ng, Incus, KubeVirt, or Firecracker when Crabbox
should create and clean up the runtime.

## Share a Governed Team Fleet

Use the coordinator path when a team needs shared provider credentials, lease
ownership, cleanup, usage records, active-lease limits, and monthly spend caps:

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

The coordinator is a control plane. The CLI still connects directly to normal
SSH runners for sync and command execution.

## Need a More Specific Route?

The CLI also recommends providers for artifacts, CI proof, code interpreters,
failure diagnostics, MCP attachment, network containment, pause/resume, preview
URLs, reachability, remote development, resource observability, reusable run
sessions, web-app smoke tests, and Worker/module execution.

See [`crabbox providers recommend`](commands/providers.md#providers-recommend) for
the complete current list, aliases, filters, scores, and caveats.

## Related Docs

- [Provider Selection](features/provider-selection.md)
- [Provider Reference](providers/README.md)
- [Nested Execution](features/nested-execution.md)
- [Pricing and Costs](pricing.md)
