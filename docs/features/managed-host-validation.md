# Managed-host validation

The managed `ssh-host` provider is distinct from direct `ssh`. Validate against
an isolated coordinator and disposable target before enrolling shared capacity.
Never point the smoke harness at an existing broker or host.

## Local Linux borrower proof

Requirements: Docker with a Linux engine, Go from `go.mod`, Node from
`.node-version`, and installed coordinator dependencies. Docker runs the target
privileged **inside the disposable engine VM** to allow nested user/PID/mount
namespaces; this is a test-fixture requirement, not a production container recipe.
The target uses an init process to reap children during account teardown.

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
# Use amd64 instead when the Docker engine is x86_64.
GOOS=linux GOARCH=arm64 go build -trimpath -o bin/crabbox-host-linux ./cmd/crabbox
npm run build:node --prefix worker
python3 scripts/live-managed-ssh-host-smoke.py --evidence /tmp/managed-host-proof
```

The script creates its own loopback-only coordinator, PostgreSQL database,
network, Linux target, synthetic credentials, and isolated CLI home/config. It
uses an anonymous public Git fixture for the existing ready-pool scrub protocol;
no repository credentials are forwarded. The resulting `result.json` records
completed checks and failure diagnostics, and resources are removed in `finally`.
An interrupted process may need manual removal of its exact `cbx-host-proof-*`
resources; do not remove unrelated containers.

Coverage includes borrower sync/exec, unauthorized access, cleanup-before-reuse,
two portable-pool generations, concurrent allocation, idempotent admission,
coordinator restart, failed cleanup quarantine, expiry, host-agent restart,
and draining/removal without stopping the physical host. A failed run is not
proof of checks that follow its failure. Linux namespace restrictions and lack
of network isolation are described in [managed SSH hosts](../providers/ssh-host.md).

## Windows qualification boundary

Windows amd64/arm64 compilation checks API and build compatibility only. The
MXC adapter requires separate interactive-desktop hardware qualification of
provision/start, the nested SSH endpoint, borrower sync/exec, key rotation,
stop/deprovision, crash recovery, and the requested hardware capabilities.
No successful Linux result or standalone MXC probe establishes those claims.
Keep Windows deployment experimental until that complete scenario is proven.
