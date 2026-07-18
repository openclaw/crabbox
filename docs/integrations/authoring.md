# Integration Authoring

Read this before adding an editor extension, terminal plugin, coding-agent
package, task pack, or other external control surface.

## Classify it first

| Question | Crabbox concept |
| --- | --- |
| Where does execution capacity come from? | Provider |
| What optional software or reachability does a lease expose? | Lease capability |
| What repeatable repository command should run? | Job or preset |
| What local UI or agent invokes Crabbox? | Integration |
| What long-running workload is supervised inside a lease? | Station profile or harness |

Do not use "plugin" as a generic answer to all five questions. Host plugin
manifests are distribution details for integrations. Runtime-loaded provider
plugins and Station harnesses have different trust and lifecycle contracts.

## Local integration contract

Keep a local integration thin:

1. Discover the installed `crabbox` binary and fail with an upgrade or install
   hint when the required command is unavailable.
2. Invoke it from the active repository with argument arrays where the host API
   permits, without an extra shell.
3. Use documented versioned JSON for structured automation. Keep human output
   for terminal panes and explicit copy/paste workflows.
4. Forward process cancellation and keep foreground lifecycle commands alive
   for as long as their documented consumer session lasts.
5. Never store or proxy provider credentials, broker tokens, SSH keys, or lease
   claims. Never call provider APIs directly.
6. Surface cost-bearing acquire actions and destructive release actions clearly.
7. State platform, target, shell, provider-capability, and sync-back limits.

The CLI remains the only authority for config precedence, credential source
binding, provider selection, ownership, sync, evidence, and cleanup.

## Package and workflow metadata

The goal router is intentionally compact. Every first-party package or workflow
guide linked from it should make these fields visible:

| Field | Meaning |
| --- | --- |
| Type | Editor, terminal UI, local agent client, or Station runtime |
| Status | Available, package available, experimental, or contract only |
| Install owner | The host registry/CLI or Crabbox itself |
| Required Crabbox contract | Commands and versioned JSON schemas consumed |
| Platforms and targets | Local OS plus supported remote target OS |
| Credentials | Where auth stays and whether the integration stores anything |
| Lifecycle | Who acquires, keeps alive, cancels, and releases |
| Return path | Streamed output, artifacts, copied files, patch, or pushed branch |
| Validation | Contract tests and real host/runtime proof |

Use immutable versions or digests for downloaded runtime artifacts. An editor
task pack that only calls the installed CLI does not need its own dependency
resolver.

## When core should change

Prefer an integration-only change when existing commands provide the required
behavior. Change Crabbox core only for a provider-neutral capability that more
than one client needs, such as a stable structured handoff.

Do not add:

- a hidden command dedicated to one editor when a public command can express
  the workflow;
- a second provider or credential client inside an integration;
- an MCP tool for every CLI flag without a smaller typed workflow;
- an in-box daemon before Station supervision, bridge authorization, cleanup,
  and evidence exist;
- a runtime executable marketplace before signature, update, permission, and
  compatibility policy are designed.

## Validation

At minimum:

- parse and validate the host manifest and every task/tool schema;
- execute the real command, argv, CWD, environment, and cancellation boundary
  against a controlled fake `crabbox` binary;
- prove credential-shaped values are not persisted or printed;
- exercise read, acquire/run, cancellation, and release flows;
- add redacted proof from the real host before claiming registry availability;
- run the normal Crabbox docs, unit, and integration checks.

For an in-box runtime, use the stricter review checklist in
[Agent Runtime Bridge](../features/agent-runtime-bridge.md).
