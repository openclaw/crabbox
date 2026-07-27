# Crabbox

**Run any repository command in the right box.**

## What Crabbox Is

Crabbox keeps the edit-and-run loop on your laptop while moving execution to a
local sandbox, cloud VM, existing SSH host, managed developer environment, or
hosted agent sandbox. Depending on the selected provider, it syncs the checkout
or hands the workload to a provider-owned execution contract, returns available
output and evidence, and releases owned capacity when cleanup is supported.

```sh
crabbox run -- pnpm test
```

## One Loop, Many Kinds of Box

Every run follows the same basic path:

1. **Choose** a provider directly or ask Crabbox to recommend one for the job.
2. **Lease or reuse** a short-lived box, sandbox, VM, or host.
3. **Sync or hand off** the workload according to the provider's execution
   contract.
4. **Run** the command and stream its output.
5. **Collect available proof and apply cleanup** according to the provider's
   capabilities and the run policy.

The execution substrate can change without turning the workflow into a
provider-specific script. Start with [Use Cases](use-cases.md) when you know the
job but not the provider, or browse the [Provider Reference](providers/README.md)
when you already know where the work should run.

## Start Here

- [Getting Started](getting-started.md) — install the CLI and complete a first
  remote run.
- [Use Cases](use-cases.md) — choose a workflow such as fast feedback, agent
  execution, cross-platform validation, browser QA, fan-out, or GPU work.
- [Pricing and Costs](pricing.md) — understand the current open-source,
  bring-your-own-compute cost model and its guardrails.
- [How Crabbox Works](how-it-works.md) — follow a run across the CLI,
  coordinator, and runner.

## Pick an Operating Model

| Path | Best For | Ownership |
| --- | --- | --- |
| Local runtime | Fast, credential-free development checks | Your workstation and local runtime |
| Direct cloud or SSH | Personal cloud accounts, private hosts, and self-hosted virtualization | Your provider account or infrastructure |
| Team coordinator | Shared credentials, leases, cleanup, usage, and spend caps | Your Cloudflare or Node.js/PostgreSQL deployment |
| Delegated execution | Provider-shaped agent, CI, browser, or GPU runs | The selected provider or self-hosted runtime operator |

Crabbox software is MIT-licensed. It does not currently publish a hosted
control-plane plan; provider compute and any coordinator infrastructure are
billed by their respective operators. See [Pricing and Costs](pricing.md) for
the exact boundary.

## Trust Boundary

Crabbox is a developer execution tool, not one uniform security sandbox.
Isolation depends on the selected runtime. A local container with the host
Docker socket, a managed microVM, a shared team VM, and a provider-owned sandbox
have different boundaries.

Use [Provider Selection](features/provider-selection.md) to route a workload,
then read that provider's documentation before running unfamiliar or untrusted
code. The recommendation command is workflow guidance, not a security
certification.

## Go Deeper

- **CLI and configuration:** [CLI](cli.md),
  [Command Reference](commands/README.md),
  [Configuration](features/configuration.md), and
  [Repository Onboarding](features/repository-onboarding.md).
- **Fleet and operations:** [Architecture](architecture.md),
  [Infrastructure](infrastructure.md), [Operations](operations.md),
  [Observability](observability.md), and [Security](security.md).
- **Runs and evidence:** [Jobs](features/jobs.md),
  [Actions Hydration](features/actions-hydration.md),
  [Artifacts](features/artifacts.md), [Checkpoints](features/checkpoints.md),
  and [Interactive Desktop and VNC](features/interactive-desktop-vnc.md).
- **Platform boundaries:** [Nested Execution](features/nested-execution.md)
  distinguishes WSL2, container engines, prepared KVM hosts, and local
  sandboxes.
- **Extensibility:** [Integration Catalog](integrations/README.md),
  [Provider Authoring](features/provider-authoring.md), and
  [Source Map](source-map.md).

## About These Docs

The Markdown in `docs/` is the user-facing source for
[crabbox.sh](https://crabbox.sh/). Implementation truth stays in code; the
[Source Map](source-map.md) traces documented behavior back to its owner.

Build and validate the site locally:

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```
