# Integrations

Crabbox integrations let editors, terminal UIs, and coding agents use the
installed `crabbox` CLI without taking over provider credentials, lease
ownership, synchronization, evidence, or cleanup.

**Integration** is the umbrella term. A plugin is only one host-specific
package format. Providers remain execution substrates, and a harness running
as a long-lived daemon inside a lease belongs to the planned Station runtime
rather than to a local plugin system.

## Choose the surface

| Goal | Surface | Status |
| --- | --- | --- |
| Teach a local coding agent when and how to use Crabbox | [`crabbox init` Agent Skill](agents.md#local-agent-clients) | Available |
| Run a repo-owned one-shot harness remotely | [`crabbox run` or a named job](agents.md#one-shot-harnesses) | Credential-free run-evidence pattern available |
| Reuse repository setup on a warm lease | [GitHub Actions hydration](../features/actions-hydration.md) | Available |
| Use Zed as a local Crabbox control surface | [Zed extension package](editors.md#zed-control-surface) | Package available; registry publication pending |
| Open a synced lease as a remote editor workspace | [`crabbox open --editor=zed`](editors.md#remote-editor-handoff) | Available |
| Control leases and jobs from Herdr | [Herdr plugin](editors.md#herdr) | Available |
| Run a long-lived coding harness inside a lease | [Station agent-runtime bridge](agents.md#long-running-harnesses) | Contract only |

Status labels are deliberate:

- **Available** means the integration or workflow is implemented on current
  `main`.
- **Package available** means source and validation exist, but installation may
  still use the host's development flow.
- **Contract only** means the security and lifecycle boundary is documented but
  no user-facing command is shipped.

## Two extension planes

```text
local machine                                      leased runner
-------------                                      -------------
IDE / terminal UI / coding agent
          |
          +-- host-native integration --> crabbox CLI --> run or job
                                          |
                                          +-----------> future Station
                                                          |
                                                          +-- harness daemon
```

### Local control surfaces

Local integrations invoke the installed CLI from the active repository. The
CLI remains the authority for configuration, credentials, cost, ownership,
sync, run history, artifacts, and release. The host owns installation and UI.

This is the current pattern used by Zed, Herdr, editor handoffs, and the
generated Agent Skill. See [Editors and control surfaces](editors.md) and
[AI agents and harnesses](agents.md).

### In-box runtimes

A long-running harness inside a lease has a different trust boundary. Crabbox
must supervise its process, bind its control port to loopback, authenticate the
bridge, enforce stop and expiry, and redact evidence. That work belongs under
Station. It must not be smuggled into a provider adapter, editor package, or
ambient environment forwarding.

See the [Agent Runtime Bridge](../features/agent-runtime-bridge.md) and
[Station Profiles](../features/station-profiles.md) contracts.

## What Crabbox does not ship

Crabbox does not currently load arbitrary executable plugins or provide a
plugin marketplace. Adding such a loader would make core responsible for
discovery, signatures, updates, version negotiation, permissions, and
credential exposure without improving the existing host-native installation
model.

Crabbox also does not currently expose an MCP server. Coding agents with a
shell can use the generated skill and CLI today. A future local MCP adapter is
reasonable only when typed discovery, structured approvals, and result schemas
materially improve on that path without duplicating the complete CLI surface.

There is no generic integration SDK or implemented HTTP/SSE agent bridge, and
`zed` is currently the only accepted `crabbox open --editor` value.

## Add an integration

Use the [integration authoring contract](authoring.md) before adding a new
editor, agent client, task pack, or host plugin. Keep the package thin and make
its lifecycle and credential boundaries visible.
