# Private first-walk qualification package

This temporary package is diagnostic tooling. It makes no performance improvement
and is outside the normal Go, Worker, Node.js and documentation build inputs.
Already merged qualification tooling remains a separate final-closeout obligation.

`manifest.json` pins accepted-main commit
`c75e9323c8ecf36b8cb4b6da1a524a046e26538c` and its tree. `runtime.patch`
preserves the prior diagnostic implementation, including snapshot-use protection
through attachment, execution and complete cleanup, exact volume identity,
uncertain-response recovery and cancellation. It does not replace the lifecycle.
The serial ownership routine retains numeric ownership checks, descriptor
anchoring, no-follow traversal and conditional writes. No scheduler or bypass is
included. `readiness-reporting.patch` preserves the separate reporting fix for
possible product adoption; the private overlay does not apply it.

## Build boundary

The protected `image-qualification.yml` workflow is dispatch-only. In first-walk
mode its credential-free build checks out three separate trees: protected tooling,
candidate data and the exact baseline. The candidate's production inputs must
match the baseline and its package must match the protected package. Candidate
scripts, patches, configuration and package hooks are never executed. Protected
code verifies the patch digest and every preimage, applies the protected patch to
the disposable baseline, then verifies the complete changed-path set, postimages,
recipe and full Worker source digest. It uses the pinned baseline's Go inputs and
Worker lockfile (`npm ci --ignore-scripts`), and the protected private configuration.
Only this separate checkout receives the overlay.

The resulting version-2 build receipt and artifact manifest bind baseline commit
and tree, package/overlay/recipe/configuration hashes, complete resulting Worker
source identity and every output's size/hash. Credentialed consumers require the
same package identity and reject legacy first-walk manifests. Mint and retained
qualification keep their version-1 contract. Old first-walk artifacts/handoffs
cannot be reused: build fresh artifacts and a fresh handoff from adopted protected
tooling. This package must reach protected main before that supported dispatch;
this draft does not authorize adoption or execution.

Production configuration, source, generated bootstrap and CLI readiness reporting
remain byte-identical to the declared baseline. There is no production import of
this package. No deployment/publication path filters are changed. All changed
paths are outside the existing coordinator deployment and Pages publication
filters. General CI may run; merging tooling does not itself dispatch qualification.

## Local validation

Use Node from `.node-version` and Go from `go.mod`, with no cloud credentials.
From the repository root:

```sh
node --test scripts/image-qualification*.test.mjs scripts/image-qualification*.test.js
```

For an end-to-end build rehearsal, make separate local checkouts of the candidate,
the protected tooling and the manifest's baseline. In a credential-free shell:

```sh
npm ci --prefix /absolute/baseline/worker --ignore-scripts
QUALIFICATION_MODE=first-walk \
QUALIFICATION_CANDIDATE_SHA=<candidate-commit> \
QUALIFICATION_WORKFLOW_SHA=<protected-commit> \
node /absolute/protected/scripts/image-qualification-control.mjs prepare-build \
  /absolute/protected /absolute/candidate /absolute/artifact/build-inputs.json \
  /absolute/baseline
```

Then follow the dispatch workflow's Go and Wrangler **dry-run** build, staging and
manifest commands. Never use the patched checkout as a production source tree.
Run overlay Worker tests/typechecks and generated-recipe consistency checks there.
The protected executor, authority, finalizer and independent reaper still own live
resource settlement; build/test success is not live lifecycle proof.

## Observation still required

A separately authorized synthetic cycle must validate the complete private
lifecycle before admitting a representative capture. One untouched disposable
snapshot data copy must contain both the saved workspace and populated cache.
Attach after stock readiness; bind the exact snapshot, volume and NVMe identity;
mount privately without selecting duplicated UUIDs/labels. Run the diagnostic
serial routine once, capturing elapsed/counters, process CPU/I/O, device activity
and exits. Verify ownership/layout afterward, reusing that pass for fanout/depth.
Allow the historical 35-minute walk plus provisioning, mounting, verification and
complete recovery reserves. Record journal replay and omitted root-boot contention.
A warm rescan cannot substitute. No deployment, allocation, spending, credential
setup, campaign access, production changes or capture admission follows from this
package.
