# Cloudflare native artifact proof

Issue: https://github.com/openclaw/crabbox/issues/2859

## Source and scope

- Repository: `openclaw/crabbox`.
- Branch: `codex/cloudflare-native-artifacts-20261015`.
- Fresh remote base: `05eded31b4d8b9a96f8e56813de142f4a9255b8e`.
- Implementation source: initial commit `8143ed2f94c9a54b6aa6405c5e406740cf666a94`, scoped integration repair `1ef4e2d03a670d2d360dbe855f47a76a28db7fa2`. The final proof-only PR head is recorded in its body and review handoff.
- One isolated worktree; the landing checkout and its unrelated configuration were preserved.
- Scope: existing Cloudflare provider, shared artifact selection/materialization and delegated lifecycle hook, existing Worker native-exec transport, targeted tests and provider documentation.

## Behavioral evidence

The focused Go fixtures execute the existing collector and compare PNG, ZIP (larger than 64 KiB), JSON and archive bytes after local publication. Provider protocol fixtures cover fresh and reused leases, evidence after a failed command, claimed workdir/authentication, required outputs, corrupt/truncated/oversized transfer metadata, and redaction. Archive regressions reject traversal/absolute/protected paths, symlinks/hardlinks, unexpected/duplicate/excessive members, expanded-byte overflow, damaged gzip/truncated payloads and trailing archives before publishing files. Empty optional matches remain valid. The delegated lifecycle matrix covers its original providers without the new hook; new cases cover artifact-before-cleanup ordering and command/artifact/cleanup precedence.

The 54-test Cloudflare Worker suite covers exact binary bytes, fresh/repeated lease use, native protocol/authentication, wrong-workdir refusal, missing output, byte limits, interrupted reads, request-specific cleanup and stalled-transfer watchdogs. It uses fake native Container.exec processes; this is protocol proof, not a deployed Cloudflare observation.

## Verification

Runtime: task-scoped official mise Node `24.21.0` (repository `.node-version`: `24`); no global default changed. Latest public Workers type definitions `5.20261015.1` were checked for Container.exec/ExecProcess signatures; dependency and lock files remain unchanged.

| Gate | Result |
| --- | --- |
| Focused Go artifact/lifecycle/provider/conformance tests | Passed with race detector after repair (4 packages), plus all recommendation race tests after final scoring adjustment |
| Full `go test -race -timeout=20m ./...` | Failed on initial source head `8143ed2f`: CLI timed out at 20 minutes (1206.264s); several native-child readiness/deadline tests failed. See [failure summary](full-go-failures.txt). No baseline cause is claimed |
| `go vet ./...` | Passed on initial source; targeted vet passed after the scoped repair |
| Node 24 full Worker Vitest suite | Passed: 90 test files, 4,451 tests; 2 files / 15 tests skipped by existing integration gates |
| Worker format, lint, TypeScript checks | Passed |
| Changed Cloudflare Worker esbuild bundle | Passed, 40.1 KiB |
| `npm run build:cloudflare --prefix worker` | Environment blocker: Wrangler dry-run requires a Docker CLI, which cannot launch here |
| Docs contract check | Passed after regenerating the source-derived provider capability matrix: 262 Markdown files, 26 docs-site tests, 83 providers |
| `git diff --check` | Passed |

Generated local command output is under `runs/cloudflare-artifacts-runs/20261015/` (not committed). No generated tarballs, images, credentials, auth logs or runtime configuration are included in this proof.

Focused command: `go test -race ./internal/cli ./internal/providers/shared ./internal/providers/cloudflare -run 'TestDelegatedArtifact|TestDelegatedSandbox|TestCloudflareArtifact' -count=1`. Its terminal output is [focused-go.txt](focused-go.txt).

## Integration failures and scoped repair

The full run exposed three regressions caused by the new capability registration, reproduced with only their named tests before repair:

- `required_fix`: Cloudflare advertised downloads without implementing the existing `DelegatedRunDownloadBackend` interface. It now implements `FetchRunFile` through the same claimed runner/workdir, claim fence and archive validator. A focused test verifies binary bytes, a mismatched current configuration, unsafe paths and the caller byte limit.
- `required_fix`: artifact evidence alone increased the provider's fast-feedback score without a cache/local/CI-runner speed signal. The existing evidence bonus now requires that signal; other recommendation candidates remain eligible.
- `required_fix`: an old no-match fixture assumed no provider could combine run evidence and checkpoints. Cloudflare now legitimately can, so that fixture also requests a desktop capability.

Affected artifact, provider, shared lifecycle and feature-conformance race tests pass after repair. All recommendation race tests also passed after the final scoring adjustment. Terminal excerpts: [contract-repair-go.txt](contract-repair-go.txt), [recommendations-go.txt](recommendations-go.txt). The archive parsing split shares validation with `FetchRunFile` and does not add local publication to that interface.

The full run's other checkpoint/rsync/Actions/native-child failures and package watchdog remain unadjudicated outside this slice. They are preserved rather than described as baseline or fixed. The full suite was not rerun after the focused repair. This draft is not a merge-readiness claim.

## Review and gates

- Native ClawSweeper contract: `.github/workflows/clawsweeper-dispatch.yml` handles PR opened/synchronize events. Draft https://github.com/openclaw/crabbox/pull/2860 has native opened pickup acknowledgement comment `6201584239`; dispatch run `38801990371` succeeded. Terminal exact-head review is pending; no manual duplicate trigger.
- Spark-2 preflight from the operator Mac: effective `spark-2` routes to account `smoky`, host `192.168.1.41`, port `22`; bounded strict-host-key/BatchMode hostname probe timed out. Effective `spark-2.swarm` uses the same account/port and DNS hostname; its bounded probe failed DNS resolution. Identity-file count was one for both; no key contents or proxy commands were inspected.
- No OpenClaw review generation was dispatched while that assigned route was unreachable. The initial exact-head media preflight passed with zero candidate media and zero required fixes. The final proof-only head preflight and coordinator handoff follow this closeout. The upstream media shelf is `saari-co/public-oss-proof-assets`; this change needs no media uploads.
- Coordinator review of initial source commit `8143ed2f94c9a54b6aa6405c5e406740cf666a94` found no actionable runtime or documentation issue. The later scoped integration repair is separately identified above for final coordinator review. No native/Spark terminal attestation is claimed.
- Deferred proof: real Cloudflare runner deployment and live retrieval. No cloud provisioning/deployment, credentials, account changes or browser-desktop work occurred.
- Human gates retained: merge and any future deployment. The draft remains review preparation.
