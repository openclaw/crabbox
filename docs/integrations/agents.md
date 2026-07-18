# AI Agents and Harnesses

"Agent integration" can mean three different workflows. Choose the smallest
one that fits: teach a local coding agent to call Crabbox, run a repo-owned
one-shot harness through normal execution, or supervise a long-lived harness
inside a lease.

## Local agent clients

`crabbox init` writes an Agent Skills-format document alongside repository
configuration and the optional Actions hydration workflow:

```text
.agents/skills/crabbox/SKILL.md
```

```sh
crabbox init --detect
```

The skill uses the open [Agent Skills specification](https://agentskills.io/specification),
including required `name` and `description` frontmatter. It tells a coding
agent when to warm and reuse a lease, run a command or detected job, inspect a
failure, and stop the lease.

Skill content is portable, but discovery directories are owned by each agent
client. The default `.agents/skills` path is the broad shared location. When a
client requires another project path, select it during the first initialization;
for example, Claude Code uses `.claude/skills`:

```sh
crabbox init --detect --skill .claude/skills/crabbox/SKILL.md
```

Keep a custom path ending in `crabbox/SKILL.md`. The format requires the
declared `name` to match the directory containing `SKILL.md`; arbitrary
filenames and parent directories are not necessarily conformant.

Review generated skills like any other repository automation. They provide
instructions; they do not grant credentials or bypass the agent host's command
approval and sandbox policy. See [`crabbox init`](../commands/init.md) and
[Repository Onboarding](../features/repository-onboarding.md).

### Why skill first

For shell-capable coding agents, a concise skill preserves the complete CLI
instead of duplicating a changing subset of flags in one plugin per harness.
Repo-local jobs keep project commands and proof requirements reviewable in
`.crabbox.yaml`.

An MCP adapter may be useful later for clients without a shell or for a small
typed workflow such as lease discovery, status, logs, and artifact retrieval.
It should be local, use structured safety annotations, preserve the active
repository as its working context, and avoid becoming a second unversioned CLI.

## One-shot harnesses

If a repository already owns a credential-free harness or evaluator script,
run it like any other project command:

```sh
crabbox run -- ./scripts/agent-check.sh
```

For a repeatable flow, declare a named job. On SSH-backed Linux providers, or a
delegated adapter that advertises bounded artifact support, the job can also
require and download proof artifacts. Crabbox owns remote execution and proof
plumbing; the repository owns the harness protocol, prompts, role separation,
and result interpretation.

Crabbox does not deliver model credentials for this pattern. Do not put model
or tool secrets in repository YAML, command arguments, or `env.allow`. A real
model-backed run must wait for a separately reviewed, workload-specific
credential path.

See [Hermetic Agent Evidence](../features/hermetic-agent-evidence.md) for the
implemented run-evidence pattern.

## Long-running harnesses

A daemonized Codex, Claude Code, OpenCode, Amp, or generic harness running
inside the box is not a local plugin. It needs durable supervision and an
authenticated HTTP/SSE bridge.

That path is contract-only today and belongs under Station. The first
implementation must remain SSH-backed Linux only, bind the daemon to lease
loopback, pin downloaded artifacts, use attempt-scoped bridge authorization,
stop descendants deterministically, and keep model credentials disabled.

Crabbox owns station and lease lifecycle, workspace grounding, bridge policy,
logs, evidence, egress, and cleanup. The harness owns its model, prompt loop,
edits, tools, and API schema. See [Agent Runtime Bridge](../features/agent-runtime-bridge.md)
and [Station Profiles](../features/station-profiles.md).

Do not forward model or tool credentials through `env.allow` as a shortcut.
Scoped model access is a separate security-reviewed phase with revocation,
redaction, egress, budget, and evidence requirements.
