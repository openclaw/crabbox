# Versioned Workspace Implementation Plan

Read when:

- turning Crabbox's agent workspace direction into implementation work;
- deciding whether a checkpoint feature belongs in core, the coordinator, or a
  provider backend;
- slicing pull requests for checkpoint, fork, compare, restore, and promote.

## Goal

Make Crabbox the control plane for versioned agent workspaces. A maintainer or
agent should be able to capture a meaningful attempt, inspect what changed,
fork another attempt from it, compare attempts, and promote the best result to
Git or a reusable provider state.

The first useful product is not a full VM snapshot engine. The first useful
product is a provider-independent checkpoint ledger that connects repo state,
lease state, run history, logs, test summaries, artifacts, and provider
metadata.

## Architecture Boundary

Core owns the stable workflow:

- checkpoint ids and metadata schema;
- local ledger persistence;
- generic Git diff, manifest, and artifact references;
- command UX for create, list, show, diff, restore, fork, and promote;
- coordinator API shape once local UX is proven.

Providers own deeper state only when they can do it natively:

- sandbox snapshot ids;
- copy-on-write forks;
- VM image or volume snapshot handles;
- provider-specific restore constraints.

Every provider must still work through the generic fallback unless a command
explicitly requires native state. A provider without native snapshots can still
participate through repo patches, sync manifests, run logs, and artifacts.

## Milestones

### 1. Local Checkpoint Ledger

Deliver a local append-only JSON ledger under Crabbox state.

Scope:

- typed checkpoint metadata;
- safe `chk_...` ids;
- newest-first listing;
- duplicate and unsafe-id rejection;
- tests for persistence and parsing.

Out of scope:

- remote restore;
- provider snapshots;
- coordinator storage;
- portal UI.

### 2. Create, List, And Show

Expose the first command surface:

```sh
crabbox checkpoint create --id blue-lobster --name after-auth-fix
crabbox checkpoint list
crabbox checkpoint show chk_...
```

`create` should capture:

- repo root, remote URL, branch, HEAD, and base ref;
- selected lease id or slug when provided;
- provider, target, class, and server type when resolvable;
- optional run id;
- changed/deleted file counts from the sync manifest;
- optional artifact bundle paths.

JSON output must be available from the start. Human output can stay compact.

### 3. Compare Attempts

Add:

```sh
crabbox checkpoint diff chk_a chk_b
```

Start with metadata and repo-level comparison:

- branch, HEAD, base ref;
- changed/deleted file counts;
- provider and target;
- run id, exit code, duration;
- artifact counts and paths.

Then add patch-aware comparison once checkpoint creation writes patch files.

### 4. Restore And Fork

Restore should be conservative:

```sh
crabbox checkpoint restore chk_... --dry-run
crabbox checkpoint restore chk_... --apply
```

Rules:

- refuse to overwrite dirty local work without an explicit flag;
- print the files that would change before applying;
- keep provider-native restore behind capability checks.

Fork should create a new workspace from a checkpoint:

```sh
crabbox checkpoint fork chk_... --provider e2b
```

Generic fork can start a fresh lease and replay repo state. Native fork can use
provider snapshot handles when the backend advertises support.

### 5. Promote

Promotion turns a checkpoint into durable project output:

```sh
crabbox checkpoint promote chk_... --branch fix/auth-timeout
```

Initial promotion can:

- create or switch to a local branch;
- apply the checkpoint patch;
- print the suggested PR body with run ids, logs, artifacts, and test summary.

Later promotion can open or update a GitHub PR and publish evidence through the
existing artifact pipeline.

### 6. Coordinator And Portal

Once local UX is stable, move the ledger into coordinator-backed storage for
shared workspaces.

Coordinator work:

- checkpoint create/list/show routes;
- owner/org scoping consistent with leases and runs;
- links to run history, logs, results, telemetry, and artifacts;
- retention limits and explicit deletion policy.

Portal work:

- checkpoint timeline on lease and run pages;
- parent/child graph;
- compare view;
- fork/promote affordances for authenticated owners.

## Storage Model

Keep the checkpoint record small. Store or link heavy evidence separately.

Good checkpoint metadata:

- ids, timestamps, creator, notes;
- repo metadata;
- lease and provider metadata;
- run ids and summaries;
- relative paths to local patch, manifest, archive, or artifact files;
- hosted artifact URLs;
- provider snapshot handles.

Avoid:

- provider secrets;
- environment variable dumps;
- SSH keys;
- browser cookies;
- full home directories;
- unbounded logs.

## Pull Request Order

Prefer small, mergeable PRs:

1. docs for roadmap and product boundary;
2. local checkpoint record and store;
3. checkpoint `create/list/show`;
4. checkpoint docs and command-surface checks;
5. checkpoint `diff`;
6. patch/archive capture;
7. restore dry-run and apply;
8. provider capability wiring;
9. provider-native snapshot experiments;
10. coordinator and portal storage.

Do not make early PRs depend on a specific provider. The generic path is what
makes Crabbox the control plane instead of another sandbox wrapper.
