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
client. The default `.agents/skills` path is the broad shared location, not a
universal one. When a repository must support a client with another project
path, repeat `--skill` and include both destinations during initialization; for
example, Claude Code uses `.claude/skills`:

```sh
crabbox init --detect \
  --skill .agents/skills/crabbox/SKILL.md \
  --skill .claude/skills/crabbox/SKILL.md
```

When at least one `--skill` is supplied, it replaces the implicit default, so
include `.agents/skills/crabbox/SKILL.md` explicitly when you want to retain
the shared copy. Crabbox requires every destination to be repository-relative
and end in `crabbox/SKILL.md`: the format requires the declared `name` to match
the directory containing `SKILL.md`.

### Discovery compatibility

The [Agent Skills standard](https://agentskills.io/specification) defines
`SKILL.md`, not a mandatory search path. Official client documentation was
audited on 2026-07-18; host behavior can change independently of Crabbox.

| Client | Discovers the default `.agents/skills` path | Notes |
| --- | --- | --- |
| [Codex CLI and IDE](https://developers.openai.com/codex/skills/) | Yes | Scans from the working directory through the repository root; use `/skills` or mention `$crabbox`. |
| [Cursor](https://cursor.com/docs/skills.md) | Yes | Also scans Cursor-, Claude-, and Codex-specific skill roots. |
| [GitHub Copilot](https://docs.github.com/en/copilot/concepts/agents/about-agent-skills) and [VS Code](https://code.visualstudio.com/docs/agent-customization/agent-skills) | Yes | Also support `.github/skills` and `.claude/skills`. |
| [Windsurf/Cascade](https://docs.devin.ai/desktop/cascade/skills) | Yes | Also supports `.windsurf/skills`. |
| [Devin](https://docs.devin.ai/product-guides/skills) | Yes | Discovers project skills from the repository root. |
| [Zed Agent](https://zed.dev/docs/ai/skills) | Yes | The worktree must be trusted; use the slash command, `@skill`, or AI → Skills to verify discovery. |
| [Gemini CLI](https://geminicli.com/docs/cli/using-agent-skills/) | Yes, from the workspace root | Launch Gemini from the repository root, trust the workspace, then use `/skills list` or `/skills reload`. |
| [Google Antigravity](https://antigravity.google/docs/skills) | Yes | `.agent/skills` is a legacy alternative. |
| [OpenCode](https://opencode.ai/docs/skills/) | Yes | Walks from the current directory to the worktree root; [`opencode debug skill`](https://opencode.ai/docs/cli/) shows the resolved inventory. |

The same shared path is also documented by [Roo Code](https://docs.roocode.com/features/skills),
[Amp](https://ampcode.com/manual#agent-skills),
[Warp](https://docs.warp.dev/agent-platform/capabilities/skills),
[OpenHands](https://docs.openhands.dev/overview/skills),
[Letta](https://docs.letta.com/letta-agent/skills), and
[Mistral Vibe](https://github.com/mistralai/mistral-vibe). Goose supports it
through its [Summon extension](https://goose-docs.ai/docs/guides/context-engineering/using-skills/).

These clients document a different native project path, so generate an
additional copy when the repository needs one:

| Client | Documented project path |
| --- | --- |
| [Claude Code](https://code.claude.com/docs/en/skills) | `.claude/skills/crabbox/SKILL.md` |
| [Junie](https://junie.jetbrains.com/docs/agent-skills.html) | `.junie/skills/crabbox/SKILL.md` |
| [Kiro IDE](https://kiro.dev/docs/skills/) and [Kiro CLI](https://kiro.dev/docs/cli/skills/) | `.kiro/skills/crabbox/SKILL.md`; Kiro CLI custom agents must add the skill as an explicit `skill://` resource |
| [Qwen Code](https://qwenlm.github.io/qwen-code-docs/en/users/features/skills/) | `.qwen/skills/crabbox/SKILL.md` |
| [Factory Droid](https://docs.factory.ai/cli/configuration/skills) | `.factory/skills/crabbox/SKILL.md` |
| [Cline](https://docs.cline.bot/customization/skills) | `.cline/skills/crabbox/SKILL.md` (Cline's documented native path) |

Do not assume that every Agent Skills client scans `.agents`. Do not generate
every vendor path speculatively either: clients that scan multiple roots may
surface duplicate names, and their precedence rules are host-owned. Generate
only the paths the repository's contributors use, keep the copies identical,
and verify discovery in each host.

Review generated skills like any other repository automation. They provide
instructions; they do not grant credentials or bypass the agent host's command
approval and sandbox policy. See [`crabbox init`](../commands/init.md) and
[Repository Onboarding](../features/repository-onboarding.md).

The standards-compliant generated metadata is scheduled for Crabbox 0.40.0.
Released 0.39.0 binaries still emit a body-only skill; until upgrading, add the
required `name` and `description` frontmatter manually.

### Install through ecosystem skill managers

Crabbox also publishes its generic Skill at the non-hidden
`skills/crabbox/SKILL.md` ecosystem installer convention. This makes the
authoritative generic Skill visible to installers instead of requiring them to
search Crabbox's repo-local `.agents` projection.

GitHub CLI 2.90 or newer maps Agent Skills into many host-specific locations:

```sh
gh skill install openclaw/crabbox skills/crabbox \
  --pin refs/heads/main --agent codex --scope project
```

Replace `codex` with the target reported by `gh skill install --help`. The
cross-client Skills CLI can install the same source and prompt for a target:

```sh
npx skills add https://github.com/openclaw/crabbox --skill crabbox
```

The generic source has an [official-repository skills.sh
listing](https://www.skills.sh/openclaw/crabbox/crabbox).
The checked-in `skills/crabbox` source and `.agents/skills/crabbox` projection
are byte-identical and CI rejects drift. Use `crabbox init` when the repository
also needs Crabbox configuration, Actions hydration, and detected project-job
instructions; use a skill manager when only agent discovery is missing.
`--pin refs/heads/main` selects this unreleased branch explicitly; after 0.40.0
is tagged, omit it to follow GitHub CLI's latest-release resolution.

### Discover from crabbox.sh

The docs build publishes the same Skill with a content digest through
Cloudflare's [draft Agent Skills discovery
protocol](https://github.com/cloudflare/agent-skills-discovery-rfc):

- `https://crabbox.sh/.well-known/agent-skills/index.json`
- `https://crabbox.sh/.well-known/agent-skills/crabbox/SKILL.md`

The index declares the draft 0.2.0 schema and a SHA-256 digest of the exact
published `SKILL.md`. Clients that implement domain discovery can therefore
find and verify Crabbox without a GitHub-specific registry. After the next docs
deployment, the Skills CLI can consume the same endpoint directly:

```sh
npx skills add https://crabbox.sh --skill crabbox
```

Domain discovery is an emerging transport, not part of the core Agent Skills
format, so the checked-in installer source and host-native discovery paths
remain the compatibility baseline.

### Discover through AI catalogs

For broader task-time discovery, the docs build also publishes a
`specVersion: 1.0` [AI Catalog](https://github.com/Agent-Card/ai-catalog)
manifest at:

```text
https://crabbox.sh/.well-known/ai-catalog.json
```

The catalog follows the draft [Agentic Resource Discovery
specification](https://github.com/ards-project/ard-spec), identifies the
artifact as `application/agent-skills+md`, and points to the same published
`SKILL.md`. It includes representative queries for remote testing,
cross-platform validation, and auditable evidence so ARD-compatible discovery
services can match Crabbox at task time. The site also advertises the catalog
through an HTML `ai-catalog` link and an `Agentmap` directive in `robots.txt`.

The ARD draft's examples and bundled conformance helper currently disagree on
older Skill media-type names. Crabbox follows the current AI Catalog integrated
ecosystem type, `application/agent-skills+md`.

AI Catalog and ARD add discovery metadata; they do not install, trust, or run
the Skill. A client still decides whether to fetch and install the artifact,
can verify its bytes against the adjacent Agent Skills index, applies its
configured user or administrator approval, and executes Crabbox through its
normal host policy.

The static site currently serves these files with GitHub Pages' conventional
JSON and Markdown media types. That matches the [ARD publisher
quickstart](https://agenticresourcediscovery.org/how_to_publish/), but a strict
client that requires the AI Catalog vendor media types may skip the entry until
crabbox.sh uses a header-capable serving layer.

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
