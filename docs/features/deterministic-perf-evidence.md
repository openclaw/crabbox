# Deterministic Perf Evidence

Read this when:

- designing a reproducible performance or regression gate for `crabbox run`;
- deciding whether a profiling backend belongs in Crabbox;
- reviewing a PR that adds perf-budget flags, fuel metering, or metric evidence.

**Status:** contract only. Crabbox does not yet ship a `--perf-budget`,
`--fuel-budget`, WASI, or wasmtime-fuel backend. This page records the feature
boundary for the deterministic evidence work tracked by
[openclaw/crabbox#280](https://github.com/openclaw/crabbox/issues/280), so the
runtime can land later in smaller, testable slices.

Crabbox already records wall-clock timing, phases, test results, proof blocks,
and run artifacts. Those are useful operational evidence, but wall-clock timing
is too noisy to be a hard cross-provider regression gate. Deterministic perf
evidence is the narrower contract: a backend reports one or more reproducible
metrics for a fixed workload, and Crabbox can fail the run when a metric exceeds
an explicit budget.

## Product Boundary

Deterministic perf evidence is not a general profiler and not a benchmark
ranking system.

The v1 goal is:

- run a workload under a backend that can produce a deterministic metric;
- record the metric, mechanism, engine, module identity, and budget result;
- fail after command success when a configured budget is exceeded;
- keep enough JSON evidence for CI, reviewers, and future reports.

Non-goals:

- comparing arbitrary wall-clock speed across cloud providers;
- claiming a fuel or instruction count is real CPU time;
- replacing normal Linux, container, or SSH providers with a restricted runtime;
- adding a broad observability platform to Crabbox core;
- rendering raw metric counters in human proof Markdown by default.

## First Metric

The first metric should be a deterministic instruction or fuel counter for a
pure, fixed-input workload.

A good proving workload:

- is compiled to a pinned `wasm32-wasip1` artifact;
- reads all input before the metered region;
- does no clock, random, network, or data-dependent host I/O inside the metered
  region;
- commits the input fixture and expected output digest;
- uses a pinned engine and compiler version.

This is a regression gate, not a speed claim. A pull request that changes an
algorithm can exceed the fuel budget even when the wall-clock time on one runner
looks fine. A host, engine, compiler, or import change can invalidate old
budgets even when the source did not regress.

## Evidence Schema

When implemented, deterministic metrics should extend timing JSON with an
optional object. Existing timing consumers must remain unchanged when the field
is absent.

```json
{
  "fuel": {
    "instructions": 5821447,
    "mechanism": "wasmtime-fuel",
    "engine": "wasmtime@1.2.3",
    "module": "sha256:0123456789abcdef",
    "deterministic": true
  }
}
```

Rules:

- `instructions` is the measured deterministic counter.
- `mechanism` names the metering method, not the provider.
- `engine` must include enough version identity to know whether two runs are
  comparable.
- `module` identifies the executed artifact by digest or another stable
  identity.
- `deterministic=false` means Crabbox may record the result but must not treat
  it as a hard comparable budget gate.

If Crabbox later supports multiple metrics in one run, the timing field should
grow by adding an array or nested results object without changing the meaning of
the single-metric v1 fields.

## Budget Evidence

The budget gate needs a separate machine-readable record for orchestrators and
CI logs. The file should be opt-in and written before Crabbox returns a budget
failure.

```json
{
  "provider": "metered-runtime",
  "leaseId": "cbx_3f9a1c2d4e5b",
  "runId": "run_20260704_001",
  "engine": "wasmtime@1.2.3",
  "results": [
    {
      "metric": "fuel",
      "measured": 5821447,
      "budget": 5000000,
      "exceeded": true,
      "unit": "fuel"
    }
  ],
  "exceeded": true
}
```

The exact flag names are still future work. The expected behavior is:

- each budget is an inclusive maximum, so `measured == budget` passes;
- any exceeded metric turns an otherwise successful command into a Crabbox gate
  failure;
- the command's own nonzero exit code still wins when the workload fails before
  budget evaluation;
- budget failure uses Crabbox's post-run gate semantics, like required
  artifacts, rather than pretending the user command itself failed.

## Capability Gate

Most providers cannot produce deterministic perf evidence. The implementation
should add an explicit provider feature before any CLI flag is accepted. A
provider that lacks the feature must fail during local validation with a clear
unsupported-provider error.

Do not infer support from:

- Linux target support;
- artifact support;
- timing JSON support;
- benchmark ledger support;
- delegated-run status.

The feature means the backend can produce the requested metric with stable
provenance and enough tests to prove the budget semantics.

## Runtime Boundary

A future WASI or wasmtime-fuel backend should be a narrow metering backend. It
should not become a general Crabbox provider for arbitrary repository checks.

Keep the runtime minimal:

- accept a prebuilt module or a clearly documented build artifact;
- run with explicit input and bounded filesystem access;
- deny network by default;
- avoid ambient environment inheritance;
- meter only the deterministic region;
- fail closed when engine, module, or determinism metadata is missing.

Host calls are not free in product terms even if the engine's fuel counter does
not charge them. A workload that moves expensive work into host imports can make
the metric meaningless. The docs and validation must say this plainly.

## Review Checklist

Before merging an implementation PR, verify:

- The CLI does not reuse `--profile` for perf mode; profiles already mean
  workspace policy.
- Unsupported providers fail before the workload starts.
- Timing JSON remains backward compatible when perf evidence is absent.
- Budget evidence is written before returning a budget failure.
- Command failure, provider failure, malformed metric output, and budget failure
  have distinct errors.
- Engine, mechanism, module identity, determinism, metric value, and budget are
  present in tests.
- Nondeterministic inputs, missing provenance, and engine mismatch fail closed
  for hard gates.
- Docs describe fuel/instruction counts as deterministic regression counters,
  not wall-clock performance.

## Related Docs

- [Observability](../observability.md)
- [Artifacts](artifacts.md)
- [Provider backends](../provider-backends.md)
- [Source map](../source-map.md)
