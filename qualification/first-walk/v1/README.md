# Private first-walk build package

This package supplies temporary diagnostic tooling for the dispatch-only image
qualification workflow. Normal Go, Worker, Node.js and documentation builds do
not import it. It contains no performance optimization or ownership bypass.

`manifest.json` pins the baseline commit/tree, overlay, ownership recipe,
configuration and complete resulting Worker source. Protected build tooling
applies `runtime.patch` only in a separate baseline checkout and checks every
preimage, postimage and changed path. Numeric ownership checks, descriptor
anchoring, no-follow traversal, conditional writes and lifecycle recovery remain
part of that implementation.

## Source and artifact admission

For `mode=first-walk`, leave `pull_request` blank and provide an exact 40-character
`candidate_sha` already on the protected default branch's history. The current
protected workflow commit is also eligible. An open candidate PR is not required.
Mint and retained qualification continue to require their open same-repository PR.

An accepted source commit alone is insufficient: its production inputs must match
the package's pinned baseline, and its package must equal the protected package.
Choose a compatible accepted commit; never weaken those checks or revert newer
production changes. Baseline changes require a reviewed package identity update.
Candidate files are data, never executable build controls. Protected tooling and
the baseline's pinned dependencies own the credential-free build.

Version-2 receipts and sealed artifacts bind the exact source, protected workflow,
baseline, overlay, recipe, configurations, complete private Worker source and
output sizes/hashes. Credentialed consumers still require fresh artifacts from
the same protected run, unchanged protected main, and their existing environment
approvals. Legacy first-walk manifests cannot substitute. Live execution,
authority credential custody, admission and complete cleanup remain separate.

## Validation

Use Node from `.node-version`, without cloud credentials:

```sh
node --test scripts/image-qualification*.test.mjs scripts/image-qualification*.test.js
```

For a private build rehearsal, use separate exact protected, source and baseline
checkouts. Run `image-qualification-control.mjs prepare-build` in first-walk mode,
then the protected workflow's Go and Wrangler dry-run build and manifest steps.
Local receipts are not hosted artifacts or serving-version proof. The executor,
checkpoint owner, authority, finalizer and independent reaper retain lifecycle
ownership; source admission does not authorize deployment or resource allocation.

Investigation plans, acceptance checklists, operational receipts and handoffs
belong in internal artifacts, outside this package and public build outputs.

## Internal authorization checks

The private first-walk authority performs fixed EC2 DryRun checks after its
existing request and ledger validation, before forward launch, capture, copy,
attachment, mapping and tag effects. Before launch it also tests unchanged,
missing, changed and removed ownership tags against its already-owned physical
key. Callers cannot select probes, actions or DryRun parameters.

Each run permits at most 32 persisted checks, with one dispatch per request
digest, no retries and a ten-second request/body timeout bounded by the original
forward deadline. Error XML is capped at 16 KiB. Private attestation evidence
contains only action, probe, request digest, timestamps, outcome and an
allowlisted service code. An unresolved or failed check fences subsequent
forward work across restart; the existing cleanup owner remains usable.
Passing evidence may be reused only for that exact request within the same
immutable run. Actual effects retain their final dispatch-time deadline checks.

These checks establish only the requested EC2 authorization outcomes. They do
not prove missing principal-tag trust rejection, foreign-resource isolation,
KMS success, storage readiness, successful cleanup or bootstrap improvement.
Those require their own applicable evidence. Historical attestations remain
valid without the additive private `authorizationChecks` field. A changed
package requires a fresh protected build, reviewed source admission and exact
serving-version binding; old artifacts cannot substitute.
