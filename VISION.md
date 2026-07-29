# Crabbox Vision

Crabbox is a remote software testing and execution tool. It gives a caller a
consistent way to provision or select disposable machines, synchronize a
workspace, run commands, collect evidence, and clean up afterward.

The core promise is simple: a local command should become a trustworthy remote
run without forcing every repository or automation system to learn each
provider's lifecycle, transport, and evidence conventions.

## Who Crabbox Serves

Crabbox serves developers, CI systems, coding agents, test harnesses, and other
automation that need fresh or reusable remote execution environments. These
callers may need another operating system, architecture, machine size, GPU,
desktop, browser, private network, or a provider-specific sandbox.

The caller remains in control. It chooses what to run, interprets the result,
and decides what happens next. Crabbox supplies execution capacity and returns
inspectable output, artifacts, status, and provenance.

## In Scope

Crabbox owns the provider-neutral mechanics around remote execution. That
includes provider selection and capability discovery, lease acquisition and
release, workspace synchronization, command and job execution, lifecycle
cleanup, and clear diagnostics when a requested environment cannot be supplied.

Evidence is a first-class result, not an afterthought. Logs, exit status,
artifacts, test results, resource samples, and attestations should make a run
understandable and reproducible enough for a person or another tool to review.

Crabbox may support direct providers, coordinator-backed fleets, delegated
sandboxes, and service-control backends. Provider-specific details stay behind
provider adapters; core behavior and user-facing contracts remain neutral.

Warm capacity, caching, hydration, checkpoints, and interactive access are in
scope when they make repeated execution faster without weakening ownership,
isolation, cleanup, or evidence guarantees.

Integrations are welcome when they keep Crabbox as the execution authority.
Editors, terminal tools, CI jobs, and agent skills should call stable CLI or
structured interfaces instead of reimplementing provider access.

## Scope Boundary and Non-Goals

Crabbox is used by agents; it does not orchestrate agents. Agents, harnesses,
and orchestrators are callers. Crabbox provisions disposable remote machines,
runs commands on them, and returns evidence.

Crabbox does not own an agent's prompt loop, choose its next action, interpret
its model output, or supervise its work. Agent orchestration is a real need,
but it belongs one layer up in the caller.

OpenClaw supports calling Crabbox from its own agent-orchestration layer; that capability lives in the caller rather than in Crabbox.

The following are explicit non-goals:

- supervising or hosting long-running agent runtimes inside the box;
- delivering model credentials into leases;
- brokering model or API credentials into sandboxes on behalf of an agent;
- storing prompts, reasoning traces, agent memory, or conversation state;
- judging whether generated code, tests, or model output are semantically
  correct;
- becoming a general workflow engine, agent framework, or plugin marketplace.

A caller can run its own bounded harness as an ordinary command or job. In that
case Crabbox owns remote execution and evidence plumbing, while the caller owns
the harness protocol, credentials, decisions, and higher-level lifecycle.

## Quality Bar

Remote execution is infrastructure, so correctness includes failure behavior.
Operations should be bounded, cancellable, retry-safe where appropriate, and
explicit about partial success. Destructive and reuse operations require
verified ownership bound to the exact provider, resource, and claim. Adoption
must be explicit and conflict-safe, and cleanup must fail closed when identity
or provider inventory is ambiguous.

Funded or remote providers need real create, use, destroy, and partial-failure
proof with zero residue before merge. Claims should retain enough non-secret
metadata to route and guarantee cleanup without persisting credentials.

Secrets must not appear in command arguments, logs, artifacts, or persistent
state unless a documented feature explicitly requires protected storage.
Credential boundaries should be narrow, provider-specific where necessary,
and testable.

User-facing behavior needs source-backed documentation, regression coverage,
and proof at the real boundary. For a CLI change, build and invoke the binary.
For a provider change, exercise the provider or document the exact unavailable
dependency. For docs and generated surfaces, run their consistency gates.

Compatibility is a contract, not a reflex. Preserve public CLI, configuration,
data, and protocol behavior deliberately; remove speculative or unreachable
paths that have no users and no supported migration obligation.

## Guidance for Autonomous Contributors

Good contributions make the remote execution contract more reliable, portable,
observable, secure, or pleasant to use. Fix concrete bugs, improve provider
adapters, strengthen evidence, clarify diagnostics, reduce lifecycle leaks, and
add bounded capabilities that more than one caller can use.

Start from an observed need and identify the owning layer before writing code.
Keep provider reconciliation in adapters, shared execution behavior in core,
and project-specific workflows in the calling repository or integration.

Prefer the smallest complete change. Add a regression test for a bug, update
user-facing documentation when behavior changes, and demonstrate the result
through the same interface a user will exercise.

Proposals that move prompt loops, agent supervision, model credential delivery,
or application-specific policy into Crabbox are out of scope. Build those in
the caller and use Crabbox as the remote execution and evidence layer beneath
them.

When uncertain, optimize for a durable boundary: Crabbox should make remote
execution boring, inspectable, and safe while leaving higher-level decisions to
the tools that call it.
