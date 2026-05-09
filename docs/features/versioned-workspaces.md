# Versioned Workspaces

Read when:

- deciding how Crabbox should preserve agent attempts;
- designing checkpoint, fork, compare, or promote workflows;
- deciding what belongs in a provider-native snapshot versus Crabbox metadata.

Crabbox's current primitive is a lease: a remote workspace that can be warmed,
synced, observed, shared, and released. Versioned workspaces make that lease
history explicit. A checkpoint records the state and evidence around an agent or
maintainer attempt so later work can fork from it, compare against it, promote
it, or discard it.

This is not intended to replace Git. Git remains the source-of-truth for code
review and merges. Crabbox checkpoints sit above Git and capture the workspace
context that Git does not own: provider metadata, run handles, logs, test
summaries, artifacts, screenshots, command timing, and, where providers support
it, sandbox or image snapshot handles.

## Product Loop

The target workflow is:

1. Start from a repo checkout plus a lease or delegated sandbox.
2. Run an agent or maintainer attempt in that workspace.
3. Create a checkpoint with the workspace diff and run evidence.
4. Fork one or more new attempts from the checkpoint.
5. Compare attempts by file changes, tests, logs, artifacts, duration, and cost.
6. Promote the winning state into a branch, PR, reusable image, or provider
   snapshot.

The value is reversible autonomy. Agents can take larger swings because Crabbox
keeps a recoverable timeline and a reviewable record of what changed.

## Checkpoint Ledger

The provider-independent MVP is a checkpoint ledger. A checkpoint record should
be small enough to store and inspect locally or in the coordinator, while still
linking to heavier evidence stored elsewhere.

Suggested fields:

- checkpoint id and optional parent id;
- repo root, remote URL, branch, HEAD, and base ref;
- lease id, slug, provider, target OS, and class when available;
- associated run id, exit code, timing summary, and test summary when available;
- workspace manifest, changed-file summary, and optional patch/archive pointer;
- artifact bundle paths or hosted artifact URLs;
- created time, creator, label, and free-form notes.

The ledger should be append-only by default. Editing or deleting a checkpoint is
an operator action, not the normal agent path.

## Provider Boundary

Crabbox should expose one workflow while allowing providers to supply different
snapshot depths:

- **Generic SSH leases**: record Git diff, sync manifest, logs, and artifacts.
- **Delegated run sandboxes**: record provider sandbox ids and any snapshot ids
  the provider exposes.
- **CI-backed testboxes**: record run evidence and upstream job links, but do
  not pretend Crabbox can fork provider-owned state unless the backend supports
  it.
- **Managed cloud leases**: use Git/archive fallback first; add image/snapshot
  promotion only when the lease is intentionally prepared for reuse.

Provider-native snapshots are an optimization and fidelity upgrade, not the
first product dependency. The first user-visible contract should be checkpoint,
fork, compare, and promote.

## Command Shape

Expected command surface:

```sh
crabbox checkpoint create --id blue-lobster --name after-auth-fix
crabbox checkpoint list
crabbox checkpoint show chk_...
crabbox checkpoint diff chk_a chk_b
crabbox checkpoint fork chk_... --provider e2b
crabbox checkpoint promote chk_... --branch fix/auth-timeout
```

Early implementations can support only `create`, `list`, and `show`, backed by
a local ledger. Later iterations can add coordinator storage, portal timelines,
workspace restore, provider-native forks, and PR promotion.

## Non-goals

- Do not store secrets, provider tokens, browser credentials, or full home
  directories in checkpoints.
- Do not treat provider snapshots as portable across unrelated providers.
- Do not make checkpoint restore silently overwrite local work without an
  explicit confirmation or dry-run path.
- Do not replace GitHub PR review; checkpoints should make PR review easier by
  adding workspace evidence and provenance.

## Open Design Questions

- Which fields are local-only versus coordinator-backed?
- How should checkpoint ids map to provider-native snapshot ids?
- Which artifact bundles should be copied into checkpoint storage versus linked?
- What is the safest restore UX for dirty local worktrees?
- How should multiple agent attempts be grouped into a single comparison set?

Related docs:

- [History and logs](history-logs.md)
- [Artifacts](artifacts.md)
- [Sync](sync.md)
- [Provider backends](../provider-backends.md)
- [OpenClaw plugin](openclaw-plugin.md)
