<!--
Optional linked context:
Add a visible
`Closes https://github.com/openclaw/crabbox/issues/<issue-number>` or
`Related: https://github.com/openclaw/crabbox/issues/<issue-number>` line below
this comment.

Required PR title:
type: user-facing description
Use a parenthesized scope only when it adds clarity:
fix(run): remote tests report success when the command fails

Types: feat, fix, improve, refactor, docs, chore.
For fixes, describe the user-visible symptom and trigger:
fix: remote tests report success when the command exits nonzero
Avoid implementation details such as:
fix: propagate the exec error

Write for a reader who knows the affected task, not the implementation.
The examples below are illustrative, not claims about this PR; use only what
is accurate for your change.

**MUST:** Keep **Allow edits from maintainers** enabled for this PR so maintainers
can help update the branch when needed.
-->

## What Problem This Solves

<!--
Describe the concrete user, product, or operational problem.
Use one short, plain-language sentence. For fixes, prefer:
"Fixes: <what goes wrong> when <trigger or condition>."
For other changes, describe the need without inventing a bug.
Name the affected workflow, not the code-level cause.

Avoid: "The exec handler drops the remote exit status."
Prefer: "Fixes: remote tests appear successful when the test command fails."
-->

## User Impact

<!--
"User impact: <what users, operators, or developers can now do or expect>."
Lead with the concrete outcome in plain language, usually one sentence.
For internal-only changes, say there is no user-visible change; do not invent a benefit.
Keep important risks, breaking changes, migrations, and required user actions visible here.

Avoid: "The error is now propagated through the handler."
Prefer: "User impact: developers can rely on failed remote test commands to
fail their CI job."
-->

## Why This Change Was Made

<!--
Briefly explain how the change addresses the problem without repeating the impact.
Keep the body short. Leave file lists, internal acronyms, and root-cause walkthroughs
in the diff or optional <details>; include technical detail only when it explains
behavior or a material tradeoff. Do not hide risks or required actions in <details>.

Avoid: "Changed run.go and added an assertion to the exit-status test."
Prefer: "Preserve the remote command's failure status so the calling script
can detect the failure; successful commands still return success."
-->

## Evidence

<!--
Show the most useful proof that this change works. Screenshots, screencasts,
terminal output, focused tests, CI results, live observations, redacted logs,
and artifact links are all useful. Include before/after evidence for visual
changes when it clarifies the result.

Reviewers will inspect the code, tests, and CI. Use this section to make the
validation easy to understand, not to restate the diff.
Lead with what was checked and the observed result, before commands or logs;
note meaningful gaps. Link long output or put it in optional <details>, keeping
the useful evidence summary visible.

Avoid: "Ran go test ./internal/cli."
Prefer: "Verified that a remote command exiting 7 returns 7 locally and a
successful command returns 0." Then include the commands, test results, or
other proof that supports that observation.
-->
