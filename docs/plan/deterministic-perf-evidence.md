# Deterministic Perf Evidence: Wasmtime Fuel Design

Status: proposed design for the first implementation slice. This document
resolves the workload, metric, evidence, and runtime preconditions in
[openclaw/crabbox issue 280](https://github.com/openclaw/crabbox/issues/280).
It builds on the feature contract added by
[openclaw/crabbox pull request 857](https://github.com/openclaw/crabbox/pull/857);
that [contract](../features/deterministic-perf-evidence.md) remains the concise
product boundary, while this page records the implementation decision.

Read when:

- implementing the first deterministic performance gate;
- changing the `fuel` metric or its evidence schema;
- evaluating a Wasmtime upgrade or a different metering runtime;
- reviewing the isolation boundary for metered workloads.

## Decision

The first gate will run a fixed subset of Crabbox's own workflow-policy Go tests
as a `wasip1/wasm` test binary and measure Wasmtime fuel consumed by the entire
module invocation. Fuel is a deterministic regression counter, not CPU time or
a claim about host performance.

Crabbox will invoke a pinned metering helper as a subprocess inside a
disposable runner. The helper owns Wasmtime, WASI configuration, fuel
measurement, and a small JSON protocol. Crabbox core owns provider-neutral
validation, artifact collection, and gate semantics. Wasmtime does not become
a dependency of the main Go module, and core does not branch on a provider
name.

The first slice will use the existing `--require-artifact-schema` and artifact
collection surfaces. New perf flags and a provider capability come only after
the proving workload produces identical counts on repeated runs and different
host architectures.

## Proving Workload

The workload is the `scripts` Go test binary, restricted to two existing tests:

- `TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes` parses a realistic
  workflow and verifies pinned action, reusable-workflow, and container forms;
- `TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys` verifies that the policy
  checker distinguishes executable workflow references from unrelated YAML.

This is intentionally a representative subset rather than the entire suite.
It runs the real workflow policy checker used by Crabbox's quality gate,
including YAML parsing, tree traversal, validation, Go runtime startup,
allocation, string handling, and the test harness, while avoiding tests that
need a host process, network, filesystem mutation, or provider credential. The
larger `internal/cli` package is not the first workload because its Unix secure
file paths do not currently compile for `wasip1`; adding portability shims would
turn the proving slice into unrelated runtime work. This subset is a better
first proof than a synthetic hash loop because a regression changes a Crabbox
workload that maintainers already understand.

The module build is outside the metered region. Pin the toolchain from
`go.mod`, remove host paths, and produce one test executable:

```sh
GOTOOLCHAIN=go1.26.5 GOOS=wasip1 GOARCH=wasm \
  go test -c -trimpath \
  -o /tmp/crabbox-workflow-pins.test.wasm \
  ./scripts
```

The exact guest invocation is:

```sh
/tmp/crabbox-workflow-pins.test.wasm \
  -test.run '^(TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes|TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys)$' \
  -test.count=1 \
  -test.shuffle=off
```

Once the phase-one helper and schema file exist, the end-to-end Crabbox proof
should have this shape on any SSH-backed disposable provider:

```sh
crabbox run \
  --artifact-glob '.artifacts/perf-evidence.json' \
  --require-artifact '.artifacts/perf-evidence.json' \
  --require-artifact-schema '.artifacts/perf-evidence.json=docs/schemas/perf-evidence-v1.schema.json' \
  -- sh -eu -c '
    mkdir -p .artifacts
    GOTOOLCHAIN=go1.26.5 GOOS=wasip1 GOARCH=wasm \
      go test -c -trimpath \
      -o /tmp/crabbox-workflow-pins.test.wasm \
      ./scripts
    crabbox-wasmtime-meter \
      --module /tmp/crabbox-workflow-pins.test.wasm \
      --workload crabbox/workflow-action-pin-tests/v1 \
      --budget 1000000000 \
      --output .artifacts/perf-evidence.json \
      -- \
      -test.run "^(TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes|TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys)$" \
      -test.count=1 \
      -test.shuffle=off
  '
```

`1000000000` is a calibration ceiling, not the permanent budget. The first
implementation PR must record at least 20 runs on both `linux/amd64` and
`linux/arm64`. Identical evidence counts are required; the CI budget is then
set to the observed count, or to a documented higher integer when the team
intentionally reserves headroom. No statistical tolerance is needed for a
deterministic counter.

Changing the selected tests, Go toolchain, target, WASI inputs, meter
configuration, or workload arguments creates a new workload revision and a new
budget. Changing Crabbox implementation code does not: that is the change the
gate is intended to measure.

## Metric: Wasmtime Fuel

The sole v1 metric is `fuel`, measured by Wasmtime's fuel mechanism
`wasmtime-fuel`.

The helper configures fuel consumption, creates a new store, sets
`initialFuel` to `9007199254740991` (the largest integer exactly representable
by common JSON consumers), instantiates the module, invokes its WASI `_start`,
and reads `remainingFuel` after normal completion. The reported value is:

```text
measured = initialFuel - remainingFuel
```

The measurement begins before instantiation so an executable module's start
function, Go runtime initialization, test harness, and selected tests are all
inside the counter. Compilation and cache lookup are outside it. The CI budget
is compared after successful completion; it is not used as the store's initial
fuel, because trapping at the budget would lose the exact consumed count.
Exhausting `initialFuel` is a measurement failure, not a budget result.

Wasmtime fuel is charged according to the WebAssembly instruction stream. Most
executed WebAssembly operators consume one unit; some structural operators
consume none. It does not count native host CPU instructions, compilation,
cache behavior, or ordinary time spent in host imports. The evidence therefore
uses the unit `fuel`, never `instructions`, `cycles`, or a time unit.

The count is host-independent when all of these inputs are fixed:

- the exact WebAssembly module and argv;
- Wasmtime version, source revision, enabled features, and meter configuration;
- the Go compiler and `wasip1/wasm` target;
- preopened files and their bytes;
- environment, clock, randomness, and every other WASI import result;
- the absence of asynchronous yielding, epoch interruption, and concurrent
  guest execution.

For this workload the helper preopens no directory, exposes no inherited
environment, denies network, returns a fixed random byte stream, and provides a
scripted monotonic clock. Stdout and stderr go to bounded sinks. Given the same
guest control flow, Wasmtime charges the same fuel before lowering the module
to host instructions, so CPU model, scheduler load, and wall-clock speed do not
change the count.

Fuel does not make arbitrary programs deterministic. A guest whose branches
depend on ambient files, clock, random values, network responses, races, or
uncontrolled host imports is ineligible for a hard gate. Moving work from the
guest into an unmetered host import can reduce fuel without improving the real
program; v1 therefore permits only the fixed WASI surface above.

## Evidence Artifact

The evidence file is one schema-versioned JSON object. It follows Crabbox's
existing run evidence conventions: camel-case JSON, `schemaVersion`, provider,
lease and run identifiers, artifact digests, and explicit gate output. Runtime
provenance is separate from provider identity, so an AWS, local-container, or
other adapter can produce the same metric without changing the schema.

Example:

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-07-27T19:00:00Z",
  "provider": "example-provider",
  "leaseId": "cbx_3f9a1c2d4e5b",
  "runId": "run_20260727_001",
  "workload": {
    "name": "crabbox/workflow-action-pin-tests/v1",
    "moduleSha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "toolchain": "go1.26.5",
    "target": "wasip1/wasm",
    "argv": [
      "-test.run",
      "^(TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes|TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys)$",
      "-test.count=1",
      "-test.shuffle=off"
    ]
  },
  "meter": {
    "mechanism": "wasmtime-fuel",
    "engine": "wasmtime",
    "engineVersion": "39.0.0",
    "engineRevision": "0123456789abcdef0123456789abcdef01234567",
    "configuration": "crabbox-wasmtime-fuel/v1",
    "wasiConfiguration": "crabbox-deterministic-wasi/v1",
    "comparisonKey": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "deterministic": true,
    "initialFuel": 9007199254740991,
    "remainingFuel": 9007199248501288
  },
  "results": [
    {
      "metric": "fuel",
      "unit": "fuel",
      "measured": 6239703,
      "budget": 1000000000,
      "exceeded": false
    }
  ],
  "exceeded": false
}
```

`comparisonKey` is the SHA-256 digest of RFC 8785 canonical JSON containing
the workload name and argv, metric and unit, Wasmtime version and revision,
meter configuration, toolchain, target, and fixed WASI input-set revision. It
does not contain `provider`, `leaseId`, `runId`, timestamps, host architecture,
the module digest, measured value, or budget. The module digest is expected to
change as the code under test changes; omitting it from the comparison key is
what permits a pull request to be compared with its baseline.

The v1 JSON Schema is:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://crabbox.sh/schemas/perf-evidence-v1.schema.json",
  "title": "Crabbox deterministic performance evidence v1",
  "type": "object",
  "required": [
    "schemaVersion",
    "generatedAt",
    "provider",
    "workload",
    "meter",
    "results",
    "exceeded"
  ],
  "properties": {
    "schemaVersion": { "const": 1 },
    "generatedAt": { "type": "string", "format": "date-time" },
    "provider": { "type": "string", "minLength": 1 },
    "leaseId": { "type": "string", "minLength": 1 },
    "runId": { "type": "string", "minLength": 1 },
    "workload": {
      "type": "object",
      "required": ["name", "moduleSha256", "toolchain", "target", "argv"],
      "properties": {
        "name": { "type": "string", "minLength": 1 },
        "moduleSha256": { "type": "string", "pattern": "^[a-f0-9]{64}$" },
        "toolchain": { "type": "string", "minLength": 1 },
        "target": { "const": "wasip1/wasm" },
        "argv": {
          "type": "array",
          "items": { "type": "string" },
          "maxItems": 128
        }
      },
      "additionalProperties": true
    },
    "meter": {
      "type": "object",
      "required": [
        "mechanism",
        "engine",
        "engineVersion",
        "engineRevision",
        "configuration",
        "wasiConfiguration",
        "comparisonKey",
        "deterministic",
        "initialFuel",
        "remainingFuel"
      ],
      "properties": {
        "mechanism": { "const": "wasmtime-fuel" },
        "engine": { "const": "wasmtime" },
        "engineVersion": { "type": "string", "minLength": 1 },
        "engineRevision": { "type": "string", "pattern": "^[a-f0-9]{40}$" },
        "configuration": { "const": "crabbox-wasmtime-fuel/v1" },
        "wasiConfiguration": { "const": "crabbox-deterministic-wasi/v1" },
        "comparisonKey": { "type": "string", "pattern": "^sha256:[a-f0-9]{64}$" },
        "deterministic": { "type": "boolean" },
        "initialFuel": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        },
        "remainingFuel": {
          "type": "integer",
          "minimum": 0,
          "maximum": 9007199254740991
        }
      },
      "additionalProperties": true
    },
    "results": {
      "type": "array",
      "minItems": 1,
      "maxItems": 1,
      "items": {
        "type": "object",
        "required": ["metric", "unit", "measured", "budget", "exceeded"],
        "properties": {
          "metric": { "const": "fuel" },
          "unit": { "const": "fuel" },
          "measured": {
            "type": "integer",
            "minimum": 0,
            "maximum": 9007199254740991
          },
          "budget": {
            "type": "integer",
            "minimum": 0,
            "maximum": 9007199254740991
          },
          "exceeded": { "type": "boolean" }
        },
        "additionalProperties": true
      }
    },
    "exceeded": { "type": "boolean" }
  },
  "additionalProperties": true
}
```

Schema validation proves shape, not internal consistency. The gate must also
verify all of these invariants:

- `meter.deterministic` is `true`;
- `measured == initialFuel - remainingFuel`;
- the top-level `exceeded` equals the sole result's `exceeded`;
- `exceeded == (measured > budget)`, so equality passes;
- `comparisonKey` matches the locally calculated key;
- baseline and candidate have the same schema version, metric, unit, and
  comparison key.

The helper writes a temporary file, flushes it, and atomically renames it only
after the guest exits successfully and all invariants pass. A command failure,
trap, exhausted measurement ceiling, malformed helper response, or missing
provenance is a measurement failure and cannot produce passing evidence.

### Versioning and stability

`schemaVersion: 1` is the compatibility boundary. Consumers must ignore
unknown fields. Producers may add optional fields without changing the version.
Removing or renaming a field, changing a field's type or meaning, changing the
fuel unit, allowing multiple results, or changing gate semantics requires a new
schema version.

The strings `fuel`, `wasmtime-fuel`, `wasmtime`,
`crabbox-wasmtime-fuel/v1`, and `crabbox-deterministic-wasi/v1` are stable
identifiers, not display labels. A Wasmtime or toolchain upgrade changes the
comparison key and requires fresh calibration, even when the JSON schema stays
at v1. Provider-specific metadata may be added as optional fields, but CI must
not need it to evaluate the gate.

Timing JSON may later embed the same meter/result objects as an optional field,
as described by the feature contract. The artifact remains the independently
versioned CI input; timing JSON must not become the only source of a budget
decision.

## Runtime And Security Boundary

Two runtime shapes were considered:

| Shape | Advantages | Costs and risks |
| --- | --- | --- |
| In-process Wasmtime Go binding | Direct store API; simple access to remaining fuel; one process protocol | Adds a long-lived Wasmtime binding and native runtime to `go.mod`; requires cgo/native archives across the release matrix; grows the main binary and release/supply-chain surface; a runtime panic, memory-safety defect, or guest escape shares the Crabbox CLI process, open descriptors, memory, and credentials |
| Subprocess metering helper | Keeps Wasmtime out of `go.mod`; isolates crashes and memory corruption from the Crabbox CLI; permits independent pinning, resource limits, provenance, and replacement; runtime-specific code stays behind an adapter | Adds a separately built and verified artifact plus a strict protocol; process isolation alone is not a sandbox; packaging must cover supported runner architectures |

Use the subprocess helper. Its exact fuel API is necessary because the ordinary
Wasmtime CLI can set a fuel ceiling but does not provide the complete structured
measurement/provenance protocol required here. The helper may use Wasmtime's
Rust crate, pinned in its own lockfile; that dependency is optional runtime
tooling rather than a permanent native dependency of every Crabbox build.

The security boundary is explicit:

- The Wasmtime guest sandbox is the inner boundary for WebAssembly memory,
  control flow, and the allowed WASI imports.
- The helper process is a crash and resource-containment boundary, not the
  trust boundary. It runs with an empty environment, closed inherited file
  descriptors, no network, no preopened directory for this workload, bounded
  stdout/stderr, a wall-clock deadline, and OS memory/process limits.
- The disposable Crabbox runner is the outer security boundary. A Wasmtime or
  helper compromise reaches the runner's unprivileged account, so the runner
  must contain no coordinator, provider, source-control, or user credentials
  that the workload does not need. The runner is deleted after the run.
- The controlling Crabbox process stays outside the runner. It accepts only one
  size-bounded JSON object from the helper, validates it strictly, treats all
  text as data, and fails closed on helper exit, timeout, protocol error,
  provenance mismatch, or missing evidence.

Fuel is not a denial-of-service boundary because host calls are not fully
represented by guest fuel. The wall-clock and OS resource limits remain
mandatory even when a fuel budget is present. No local in-process execution of
untrusted modules is part of v1.

Provider adapters may provision or locate the helper and report a generic
deterministic-metric capability. Core passes the workload and evidence request
through that capability. It must not contain `provider == ...` checks or
provider-specific runtime setup.

## Phased Implementation

### Phase 1: prove the contract

1. Add the v1 schema as a docs-owned JSON file and test its example fixtures.
2. Build the pinned subprocess helper for `linux/amd64` and `linux/arm64`, with
   the fixed WASI input surface and strict JSON output.
3. Run the exact Crabbox workload above using existing artifact collection and
   `--require-artifact-schema` behavior.
4. Record 20 identical counts on each architecture, verify cross-host equality,
   and select the first reviewed budget.

This phase adds no perf flag, timing field, provider name, or runtime dependency
to the main Go module. Failure to reproduce one exact count stops the work and
turns the mismatched input into an explicit part of the comparison key before
the product surface grows.

### Phase 2: integrate the gate

1. Add one provider-neutral capability for deterministic metric evidence.
2. Add the reviewed budget/evidence CLI surface without reusing `--profile`.
3. Have core validate helper output, write evidence before returning a budget
   failure, and preserve the command's own nonzero exit when the workload fails.
4. Add an optional timing JSON field whose absence leaves existing consumers
   unchanged.

Unsupported providers fail during local validation, before acquiring capacity
or running the workload.

### Phase 3: broaden only with evidence

Add another workload, runtime, or metric only after it can produce the same
provider-neutral artifact and state its own comparison boundary. Multiple
metrics require schema v2. Native profilers do not inherit a claim of
determinism from the fuel implementation.

## Out Of Scope

- A general WASI provider or arbitrary repository command compatibility.
- Treating fuel as elapsed time, CPU cycles, native instructions, cost, or a
  cross-language performance score.
- Wall-clock budgets, statistical benchmark aggregation, flame graphs, CPU
  samples, allocations, or host-call cost modeling.
- Network, writable workspace mounts, ambient environment variables, arbitrary
  WASI imports, threads, or nondeterministic guests.
- In-process execution in the Crabbox CLI or adding Wasmtime to the main
  `go.mod`.
- A hosted baseline service, automatic budget updates, or accepting a changed
  comparison key without maintainer review.
- Multiple metrics or provider-specific evidence fields required by CI.

## Open Questions For Maintainers

1. Should the phase-one helper be a Crabbox release asset, a pinned runner image
   component, or a separately versioned tool? The design requires verified
   provenance but does not choose its distribution channel.
2. Is whole-module fuel, including Go runtime and test harness startup, the
   desired v1 gate? A smaller exported-function region would be cheaper but
   would no longer exercise Crabbox's real Go test invocation.
3. Should phase two expose `--perf-budget fuel=N` and `--perf-evidence PATH`, or
   should repo-owned schemas and required artifacts remain the only public
   surface until a second workload exists?
4. Is exact equality across 20 runs on two architectures sufficient to approve
   `crabbox-wasmtime-fuel/v1`, or should the proof also cover two different
   Linux kernel and CPU generations?
5. Should a Wasmtime upgrade require a new meter configuration identifier, or
   is a changed comparison key plus recalibration sufficient while the helper's
   WASI policy is unchanged?
