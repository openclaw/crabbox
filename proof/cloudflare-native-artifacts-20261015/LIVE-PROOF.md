# Deployed Cloudflare artifact qualification packet

PR: https://github.com/openclaw/crabbox/pull/2860

## Status and claim boundary

Prepared; **not executed on Cloudflare**. The fixture has been executed locally with real Chromium and Playwright 1.61.1. The local PNG was 473,295 bytes, beyond the former 64 KiB download boundary. Local evidence is not evidence of Cloudflare transport or cleanup.

Current-head CI at `9a9d59a7ee809568d37476ae427c75a8d210ab04` completed successfully (run `38807115898`; the existing desktop resize job was skipped). This packet changes proof sources only; a later head needs its own CI evidence. The native input-safety hold remains unresolved, with no reviewer verdict. No flagged material was read and no scanner re-review was requested. A successful isolated experiment does not clear that merge gate.

## Fixed fixture and caller

- [live-browser.mjs](live-browser.mjs): creates a local page, real Chromium screenshot, real Playwright trace ZIP and result JSON. Requests are aborted; no store, customer data, login or external browser page is involved. It records browser/package/platform versions, Chromium executable hash, installed lockfile hash and artifact sizes/hashes. `failure` writes artifacts and deliberately exits 23.
- [browser-package.json](browser-package.json) and [browser-package-lock.json](browser-package-lock.json): exact Playwright 1.61.1 dependency resolution, installed with `npm ci --ignore-scripts`. Browser and system dependencies are installed inside the disposable container by the pinned package's own installer.
- [live-smoke.py](live-smoke.py): defaults to planning. With explicit `--execute`, creates a tiny isolated source fixture and one kept `standard-2` lease with a 45-minute shared wall-clock budget. First run succeeds; the second reuses the exact lease with `--no-sync` and deliberately exits 23. Both use `--download`, `--artifact-glob`, and `--require-artifact` for all three files. It compares each downloaded file against remote SHA-256/length and the published archive, validates ZIP contents/PNG signature/JSON case identity, and preserves both receipts. No artifact bytes pass through stdout.
- [live-smoke.test.py](live-smoke.test.py): seven credential-free checks including hash corruption, duplicate/wrong-case evidence, small PNG, invalid trace and a simulated transport proving one lease, deliberate exit 23 and exact-handle stop. This simulation is explicitly not Cloudflare proof.

The wrapper supplies the approved candidate endpoint and already scoped clean credential environment; credential values never appear in arguments. Do not invoke this against a shared working runner. The proof harness never deploys, rotates credentials or performs account-wide cleanup.

```sh
python3 proof/cloudflare-native-artifacts-20261015/live-smoke.py \
  --crabbox /absolute/path/to/exact-candidate-crabbox \
  --runner-url https://candidate-runner.example.workers.dev \
  --output /absolute/path/to/new-proof-directory
# Add --execute only inside the source-bound admitted runtime consumer.
```

## Runtime owner admission and completion

The CLI and Worker must both be built from the exact reviewed candidate source. Bind their SHA-256 values, fixture hashes, source commit, account, fresh Worker/app/namespace identities, image digest, endpoint, budget and cleanup scope before action. Reuse an existing qualified image only after compatibility checks; this feature needs its existing Bash/tar/timeout tools and makes no image changes. The new Worker must advertise `CBX-CF-ARTIFACT-1`. An old runner is deliberately refused before allocation and cannot qualify the new protocol.

Use one isolated candidate Worker and at most one lease; fresh resource identities must be absent before creation. A scoped credential consumer owns creation, ephemeral bearer, image choice, account mutations and cleanup. Do not repoint a persistent runner, reuse its bearer, reset consumed enrollment fences or edit store state.

The harness always attempts the exact recorded lease's stop command. A missing handle is **ambiguous allocation**, not evidence that no container exists; reconcile through the runtime owner's recorded IDs before retrying. Stop failure or timeout remains a failure requiring reconciliation. Even successful stop only establishes the command's reported outcome: independently observe provider terminal state/absence, remove the exact owned disposable Worker/app/namespace, and verify that cleanup. Do not delete a shared image. Preserve the working runner's identity unchanged.

Required final packet: source and binary/bundle hashes; deployed version/image/identity; authenticated protocol readiness; fresh and reused run outputs; PNG/trace/JSON/archive hashes; exit codes 0 and 23; exact lease identity; independent cleanup observations; unchanged shared runner proof. Report failures and partial cleanup accurately. No live result is claimed until this packet exists.
