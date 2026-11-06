# Native input-safety hold investigation

## Decision

Maintainer qualification is required. No owned synthetic fixture is confirmed as the trigger, so no runtime/test change or new review request was made. An unverified detector finding does not establish either a real credential or a harmless fixture. The hold remains active and no review verdict exists.

## Exact identities and safe evidence

- PR: https://github.com/openclaw/crabbox/pull/2860 (draft).
- Investigated current head: `01c38947bde5f36ca4e4c885a44e3bf9075c885d`.
- Base: `05eded31b4d8b9a96f8e56813de142f4a9255b8e`.
- Held review source: initial implementation commit `8143ed2f94c9a54b6aa6405c5e406740cf666a94`.
- Hold: https://github.com/openclaw/crabbox/pull/2860#issuecomment-6201584239.
- Failed native generation: https://github.com/openclaw/clawsweeper/actions/runs/38803376754 (completed failure, attempt 1).
- Scanner/control source: `291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0`.
- Sanitized receipt: [input-safety-metadata.json](input-safety-metadata.json).

The diagnostic manifest reports `agent_input_scan` / `findings`, exit 79, retryable false; one `unclassified_finding` / `finding_not_reviewed`; detector 58, HTML decoder, verified false; scanner line 463 of staged material `prompt`. It provides no file references. The public TruffleHog 3.97.4 enum identifies detector 58 as `CloudflareGlobalApiKey`. This is category and prompt scope only, not attribution to a changed source/test fixture.

The detector's public legacy-format implementation accepts a hexadecimal sequence near its provider keyword, and review inputs contain legitimate Git object identities. Such an overlap is possible; the safe manifest cannot establish it. No detected value was inspected or reconstructed. A bounded offline standard-library check of the published legacy key regular expression accepted a known public Git object identity in wholly synthetic provider-labelled text, both before and after HTML decoding. This demonstrates only a possible collision; it is not a native scan or attribution of this hold. [Synthetic regex result](input-safety-synthetic-regex.json).

## Policy and privacy

The exact failing scanner revision's [diagnostic writer](https://github.com/openclaw/clawsweeper/blob/291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0/src/clawsweeper-review-failure-diagnostics.ts) and [scan diagnostic type](https://github.com/openclaw/clawsweeper/blob/291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0/src/agent-input-scan-fixtures.ts) were inspected before artifact access. They omit native scanner stdout/stderr on an input-scan refusal; the manifest contains closed reason codes and host-staged metadata, with hashed file references when present. Only the bounded `manifest.json` member of diagnostic artifact 12004659007 was read; the archive was processed in memory and no other member was opened or persisted. No workflow logs, raw matches, credential values, credential stores or authentication logs were inspected.

The [current refusal-hold contract](https://github.com/openclaw/clawsweeper/blob/291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0/docs/proof/scanner-refusal-hold/README.md) retains the hold across source changes. A fresh verified explicit review request can release a hold; automatic source changes do not. This task additionally requires remediation or qualification before making that request. The [fixture policy](https://github.com/openclaw/clawsweeper/blob/291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0/README.md) scans the prompt, complete patch and committed regular-file blobs at both base and head. Intentional fixtures need exact maintainer-reviewed qualification; fixture words, test paths, checkout ignores and unchanged-line assumptions grant no exemption. The current policy explicitly keeps prompt findings blocking; source fixture classification cannot be inferred for this prompt-only finding. The public [comment admission source](https://github.com/openclaw/clawsweeper/blob/291ab8a0090a8f76c08e4d0e39ea4cf1fdcbe0c0/src/repair/comment-router/admission.ts) permits a PR author to request their own read-only re-review, and the router verifies current comment authority. That permission does not let a contributor qualify host scanner input. Qualification belongs to maintainer-owned host policy, not a flag or exemption in this PR. No scanner policy, permissions, credentials or security settings were changed.

## Concrete maintainer request

Please have a ClawSweeper maintainer inspect this run's protected scanner context and establish safe attribution for detector 58 / HTML / unverified / prompt line 463 at the held source. Confirm whether this is source metadata, an intentional fixture, or material needing removal through the appropriate private channel. Do not publish the detected value or raw scanner output. If it is intentional nonsensitive material, qualify its exact current source under the existing scanner policy. Once remediation or qualification is established, one fresh authorized native re-review can be requested for the then-current PR head and body. Repeated requests before qualification would only replay the refusal and are not a resolution.

## Checks and retained gates

No runtime/test source changed during this investigation, so no tests were rerun. Existing focused test proof remains applicable to the implementation. Current-head CI was observed once; [the sanitized snapshot](input-safety-ci.json) records 28 successful checks, one skipped check and three in-progress checks (Release Check, Go core, Go modules). No terminal native review or final CI pass is claimed. No fresh native review generation was requested; the known generation is terminal and the hold persists. Merge, deployment and any credential/security-policy action remain human gates.
