# Experimental fuel evidence helper

This source-built helper measures the two existing in-memory workflow action-pin
tests with a pinned Wasmtime engine. It emits the existing v1 performance-evidence
artifact for Crabbox's ordinary SSH artifact consumer. It is optional; the Go CLI,
provider APIs, normal releases and timing JSON do not acquire a Rust dependency.

This implements the first slice of the
[accepted design](../../docs/plan/deterministic-perf-evidence.md).
Related: https://github.com/openclaw/crabbox/issues/280 and
https://github.com/openclaw/crabbox/pull/1196.

## Build and invocation

Linux amd64 and arm64 are the qualification targets. Build with Rust 1.97.0:

```sh
cargo build --locked --release --manifest-path tools/crabbox-wasmtime-meter/Cargo.toml
```

Compile the selected Go tests with Go 1.27.3, `GOOS=wasip1 GOARCH=wasm`, using
`go test -c -trimpath ... ./scripts`. The helper supplies the fixed anchored
selection, `-test.count=1` and `-test.shuffle=off`; it does not execute Node.js.
Use a fresh private output directory owned by the helper account:

```sh
crabbox-wasmtime-meter --module workload.wasm --module-sha256 <verified-sha256> \
  --output evidence/perf-evidence.json --workload example-org/workflow-action-pin-tests/v2 \
  --provider local-container --budget <reviewed-fuel-budget>
```

Exit 0 means a completed measurement within budget; exit 3 means valid evidence
exceeds the performance budget. Exit 2 is an invocation or measurement failure.
An existing output is refused. Guest failure, trap, exhausted measurement ceiling
or provenance mismatch cannot publish a new evidence file. Compilation is outside
fuel; instantiation, Go initialization and the selected test harness are included.

The measurement ceiling is fixed at `2^53 - 1`. It is distinct from the performance
budget. The engine's real `OutOfFuel` path is tested with a test-only small ceiling;
qualification does not attempt to exhaust the production ceiling. The initial CI
budget of one billion is a temporary calibration ceiling, not a permanent
regression budget or a budget transferred from v1. Review fresh v2 counts and any
deliberate headroom before adopting a budget.

## Reproducibility and interpretation

Wasmtime 50.0.1 is pinned to upstream source
`c307ec81de02852d815e9849f7c89d955c03c30d`; Cargo.lock binds dependencies.
The explicit features use Cranelift, synchronous preview1 linking and synchronous
calls. Preview1 includes async support dependencies, but execution does not use
async calls or fuel yielding. The deprecated Wasmtime 50 `async_support` switch
does not configure execution. Threads, compilation caching and parallel
compilation are not enabled.
Copy-on-write memory initialization is explicitly disabled, so instantiation does
not require a memfd-backed initialization image under the fixture's syscall and
file-size limits. This setting is preserved in `crabbox-wasmtime-fuel/v2`.

The v2 configuration explicitly selects Cranelift `OptLevel::None`; v1 used the
default `Speed` optimization policy. This is an experimental compilation policy:
skipping optimization may reduce compilation work and may make guest execution
slower. It does not establish a compilation-time or CPU improvement. Engine,
features, fuel ceiling and isolation limits remain unchanged. The selected
workload is now `example-org/workflow-action-pin-tests/v2` and needs fresh
qualification; successful v1 counts do not qualify it.

The guest receives fixed argv, an empty environment, closed stdin, no preopened
directories, no network capability, a repeating 0..255 random stream, a fixed wall
clock and a monotonic clock advancing 1,000 nanoseconds per read. Stdout and stderr
have separate 256 KiB limits. These inputs define `crabbox-deterministic-wasi/v1`.
Qualification is limited to this workload; arbitrary programs can still be nondeterministic.

The comparison key hashes RFC 8785 canonical JSON for the fixed ASCII projection
in `comparison_key`. Module digest, architecture, provider, timestamps, counts and
budget are deliberately excluded. Module identity is checked separately. Engine,
toolchain or input-contract changes require recalibration and key comparison.
Fuel is not CPU time, native instruction count, throughput or host-call cost.

The artifact remains `schemaVersion: 1`. Its schema accepts the named v1 and v2
meter configurations, preserving legacy examples and evidence. The fixture's
semantic gate accepts only the current v2 configuration, workload and derived
comparison key. A structurally valid legacy artifact cannot substitute for v2
qualification or calibration.

## Real consumer and isolation qualification

The separate PR workflow builds one baseline module and an explicit source
mutation, transfers the same bytes to native amd64 and arm64 jobs, and requires
twenty identical counts on each plus exact cross-host equality. It checks Go's
`PASS` output, budget equality and budget-minus-one, mutation sensitivity, semantic
evidence predicates and the real SSH/artifact path. Required artifacts and schema
validation apply to successful commands. Budget failures preserve exit 3 and use
the existing distinct `--download-on-failure` route, followed by semantic checks.

The fixture image is built and consumed only within its CI job. It is not pushed
to a registry. The external Crabbox process retains Docker control and the SSH
private key. The ordinary lease bootstrap account retains its existing privileges;
the meter uses a separate locked UID, empty groups/capabilities, `no_new_privs`, an
empty environment, closed inherited descriptors and a native syscall allowlist.
Socket operations, io_uring and cross-process descriptor acquisition are denied.
Alternate syscall ABIs are rejected. Unsupported restrictions fail before guest
execution; there is no permissive fallback or host firewall change.

The sole outer mount is the provider's generated read-only bootstrap script. Its
contents and digest are inspected. No Docker socket, optional host/cache mounts,
private key or credential enters the lease. The guest still has **zero preopens**.
The lease has a 4 GiB memory cap; the child has process/file/output limits and a
120-second deadline. Negative probes check socket/privilege/descriptor restrictions,
output and file limits, traps, guest errors, deadline, cancellation and reaping.
Seccomp is one control, not a claim that a subprocess alone is a security boundary.

Reports distinguish engine unit proof, consumer validation and observed native
runtime results. Missing or unequal counts, failed restrictions, skipped probes,
unclean teardown or absent provenance fail qualification. They are not a reason
to relax equality or replace the selected workload.

## License and delivery

This helper's original source uses the repository's MIT license. Wasmtime uses
Apache-2.0 WITH LLVM-exception; retain its
[upstream license](https://github.com/bytecodealliance/wasmtime/blob/c307ec81de02852d815e9849f7c89d955c03c30d/LICENSE).
Qualification records the locked dependency licenses, effective features, actual
compiler and native helper/image identities. This is source-built phase 1, not a
binary release. Selecting a release asset, published runner image or independently
versioned package requires that deliverable's distribution and notice review.
