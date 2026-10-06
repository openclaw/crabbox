# Crabbox

**On-demand computers for agents.**

## What Crabbox Is

Crabbox gives coding agents and humans on-demand computers for tests and builds.
Keep editing locally while a remote box runs your working tree, including
uncommitted changes. Crabbox streams output, returns the command’s exit code,
and cleans up the lease according to the provider’s contract.

With a provider configured, run the test command your repository already uses:

```sh
crabbox run -- pnpm test
```

## One Loop, Many Kinds of Box

The standard remote execution loop:

1. **Edit locally** in your existing checkout.
2. **Lease a box** or reuse warm capacity.
3. **Sync** the working tree, including uncommitted changes.
4. **Run** your command on the box.
5. **Stream output and return the exit code** to the caller.
6. **Clean up** owned capacity, or keep it warm for the next run.

Delegated providers can own the execution and sync path. Reuse, evidence, and
cleanup follow the selected provider’s capabilities and the run policy.

The execution substrate can change without turning the workflow into a
provider-specific script. Start with [Use Cases](use-cases.md) when you know the
job but not the provider, or browse the [Provider Reference](providers/README.md)
when you already know where the work should run.

## More Agents, Less Waiting

- **Agents in parallel:** put tests and builds on separate boxes instead of
  competing for one laptop’s CPU, RAM, and ports.
- **Warm boxes:** reuse prepared environments to keep the edit-run loop short.
- **Cross-platform tests:** target Linux, macOS, native Windows, or WSL2 from
  the same local checkout. Availability depends on the provider.
- **See and drive the box:** use [VNC or WebVNC](features/interactive-desktop-vnc.md)
  on supported desktops, request a browser, or use VS Code in the browser with
  `--code` on managed Linux. The authenticated [portal](features/portal.md)
  shows leases and run logs; WebVNC supports watching or taking control while
  the local bridge runs.
- **Shared team capacity:** an optional coordinator owns cloud credentials,
  shares leases, and enforces expiry and configured spend caps for brokered
  providers.

The [provider catalog](providers/README.md) covers clouds, local VMs, SSH hosts,
and delegated sandboxes. The [external plugin contract](providers/external.md)
lets you connect your own backend. The website generates its provider wall and
category counts from the catalog metadata.

## Start Here

- [Getting Started](getting-started.md) — install the CLI and complete a first
  remote run.
- [Use Cases](use-cases.md) — choose a workflow such as fast feedback, agent
  execution, cross-platform validation, browser QA, fan-out, or GPU work.
- [Pricing and Costs](pricing.md) — understand the current open-source,
  bring-your-own-compute cost model and its guardrails.
- [How Crabbox Works](how-it-works.md) — follow a run across the CLI,
  coordinator, and runner.
- [Crabbox Vision](../VISION.md) — understand product scope, non-goals, and
  lifecycle safety rules.

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

Run trusted repositories with trusted teammates. Crabbox is a developer
execution tool; isolation depends on the selected runtime. Reuse and destructive
cleanup require verified ownership of the exact resource and claim. Ambiguous
ownership fails closed.

Use [Provider Selection](features/provider-selection.md) to route a workload,
then read that provider’s documentation and the [security model](security.md).
The recommendation command is workflow guidance, not a security certification.

## Go Deeper

- **CLI and configuration:** [CLI](cli.md),
  [Command Reference](commands/README.md),
  [Configuration](features/configuration.md), and
  [Repository Onboarding](features/repository-onboarding.md).
- **Fleet and operations:** [Architecture](architecture.md),
  [Infrastructure](infrastructure.md), [Operations](operations.md),
  [Observability](observability.md), [Read-Only Device Pairing](features/device-pairing.md),
  and [Security](security.md).
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

When editing `docs/`, use the site's supported level 1–4 headings and
triple-backtick code fences. Site-target heading links are checked against the
same heading identities the renderer emits; examples and comments do not reserve
anchors. Keep published heading IDs stable. Repository-only Markdown targets
retain the checker's separate existing GitHub-oriented anchor rules, not a claim
of complete GitHub Markdown support.
