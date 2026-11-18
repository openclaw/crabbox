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
attachment and mapping effects. Ownership tags are supplied only at resource
creation. Before launch it tests standalone restatement, missing, changed and
removed ownership tags against its already-owned physical key; all thirteen
standalone mutation/removal probes must return `UnauthorizedOperation`. Callers
cannot select probes, actions or DryRun parameters, and candidate requests cannot
proxy standalone `CreateTags` or `DeleteTags`.

Each run permits at most 32 persisted checks, with one DryRun per request
digest and no retries. The `tags-immutable` check first reads the ledger's exact
key ID, verifies its name and all four ownership tags, then performs the fixed
negative DryRun. Both requests share one ten-second timeout and the original forward
deadline. Key metadata is capped at 64 KiB and error XML at 16 KiB.

Additive `keyObservation` evidence records a resource digest and fixed-order
tag match flags; service metadata contains only HTTP status, hashed request IDs
and allowlisted error codes. Raw correlation IDs stay in private authority
storage, outside attestations and candidate responses. A missing, mismatched,
denied or interrupted key read fails closed. An unresolved or failed check fences subsequent
forward work across restart; the existing cleanup owner remains usable.
Passing evidence may be reused only for that exact request within the same
immutable run. Actual effects retain their final dispatch-time deadline checks.

If execution stops before first-walk proof is complete, finalization still
exports its independent cleanup attestation. Missing observation evidence keeps
qualification failed even when cleanup is finalized.

These checks establish only the requested EC2 authorization outcomes. They do
not prove missing principal-tag trust rejection, foreign-resource isolation,
KMS success, storage readiness, successful cleanup or bootstrap improvement.
Those require their own applicable evidence. Historical attestations remain
valid without the additive private `authorizationChecks` field. A changed
package requires a fresh protected build, reviewed source admission and exact
serving-version binding; old artifacts cannot substitute.

## Protected key authorization

The `key-authorization` workflow mode reuses this private package and the existing
qualification owner, registry, finalizer and reaper. Its immutable purpose permits
one fixed public-key import and the authority's thirteen existing tagging DryRuns.
The protected arm job invokes that operation; the candidate executor is skipped.
No guest, image, snapshot or volume may be created in this mode. The authority
rejects candidate AWS requests and first-walk preparation, while the temporary
session policy independently denies compute and storage effects. Only trusted
controller code constructs tagging probes; IAM is not a DryRun-only boundary.

This mode requires the distinct `key-tag-authorization-aws-session/v2` session
profile with matching handoff and recovery identity. It keeps the existing
130/150/270-minute lifecycle offsets; the diagnostic itself has a ten-minute
forward-work bound and does not reopen after an uncertain result. Preserve the
original run's credential margin and independent cleanup authority. Final success
requires all thirteen checks and authoritative zero-residue finalization. It is
an authorization diagnostic, not evidence of first-walk performance improvement.
Installation, namespace-preserving deployment and execution require separate
operator admission; merging tooling performs none of those actions.


## Creation-only ownership contract

Fresh private first-walk and key-authorization identities require
`taggingContract: "creation-only/v1"`. The field is bound through service props,
deployment identity, registry, attestation and recovery binding. Missing or
unknown values cannot enroll or resume forward work. Existing records without
this field retain their exact identity for finalization and retirement; recovery
must not rewrite them into the new contract.

The reusable permission policy v2 requires matching run, owner, source and expiry
request tags on each created resource. Dependent `ec2:CreateTags` is allowed only
with the documented `ec2:CreateAction` contexts for `ImportKeyPair`, `RunInstances`,
`CreateImage` and `CreateVolume`; standalone tag writes and removals are denied.
Creating operations and exact ownership readback remain mandatory. Negative
DryRuns alone do not establish successful creation, KMS access or lifecycle
completion for every resource type.

Fresh sessions use `first-walk-aws-session/v2` or the key profile above. The
compact session policy retains `CreateTags` for dependent creation authorization
and the forward-work cutoff. Legacy v1 profiles cannot issue forward sessions;
they are accepted only by the permanently forward-denied cleanup route for an
original legacy owner. Session and stored tagging contracts must match. The
existing permissions boundary, fixed resources, trusted issuer, resource limits
and independent recovery window remain required.

Install permission changes and update the private authority only through a
separately admitted migration with no active forward or recovery dependency.
Preserve the original owner and cleanup credentials until complete settlement.
Historical positive-tag diagnostics remain historical failures; they do not
satisfy this changed admission contract. Normal production provider tagging is
unchanged.
