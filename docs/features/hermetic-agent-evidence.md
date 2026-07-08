# Hermetic Agent Evidence

Read this when:

- running AI-generated code and tests from the same specification;
- keeping the code writer, test writer, and QA reviewer in separate contexts;
- using Crabbox to collect a durable proof file from the remote run.

Crabbox is a useful execution layer for hermetic-agent workflows because the
repository owns the agent protocol while Crabbox owns the remote run and
evidence plumbing: sync the checkout, run the command on a clean runner or
delegated sandbox, require proof artifacts, download bounded evidence, and keep
the exact command output reviewable.

This page documents the public demo tracked by
[openclaw/crabbox#1020](https://github.com/openclaw/crabbox/issues/1020):

- Demo repo: <https://github.com/zozo123/hermetic-agents-demo>
- Live page: <https://zozo123.github.io/hermetic-agents-demo/>

## Demo Shape

The demo models three roles:

| Role | Allowed context | Forbidden context |
| --- | --- | --- |
| Code writer | `spec/problem.md`, `guides/code-writer.md` | Generated tests |
| Test writer | `spec/problem.md`, `guides/test-writer.md` | Implementation output |
| QA arbiter | Spec, both outputs, hidden oracle | May send blame, not forbidden artifacts |

The seeded failure is intentionally a bad generated test expectation. The
implementation follows the spec, the test expects the wrong case-folding result,
and QA assigns the disagreement to `test_writer`. That matters because hidden or
generated tests are not automatically truth; the reviewer must still decide
which side violated the spec.

## Local Proof

The demo can run without Crabbox:

```sh
./scripts/run_hermetic_agents_demo.sh
python3 scripts/hermetic_agents_demo.py --self-test
bazelisk test //:hermetic_agents_e2e_test
```

Those commands write:

```text
docs/metrics/hermetic-agents-e2e.json
docs/metrics/hermetic-agents-e2e.md
```

The JSON includes the role manifests, leak-check results, artifact digests, QA
verdict, and the exact disagreement assigned to the test writer.

## Crabbox Run

The demo repo includes a `.crabbox.yaml` job:

```sh
export CRABBOX_ISLO_API_KEY=ak_...
crabbox job run hermetic-agents
```

That job uses `provider: islo`, runs the local proof script, requires the JSON
proof file after command success, and downloads the JSON/Markdown evidence into
`.crabbox/proofs/`.

The same flow without the job wrapper is:

```sh
crabbox run --provider islo \
  --require-artifact docs/metrics/hermetic-agents-e2e.json \
  --download docs/metrics/hermetic-agents-e2e.json=.crabbox/proofs/hermetic-agents-e2e.json \
  --download docs/metrics/hermetic-agents-e2e.md=.crabbox/proofs/hermetic-agents-e2e.md \
  --shell './scripts/run_hermetic_agents_demo.sh'
```

Use any SSH-backed Linux provider instead of Islo when you want Crabbox-managed
SSH and rsync rather than a delegated sandbox. The same `--require-artifact` and
`--download` gates apply on supported Linux targets.

## Why This Belongs In Repo Config

The agent protocol should stay in the application repo:

- the distilled spec and role guides are project-specific;
- the oracle and blame policy are part of the review contract;
- the proof schema can evolve with the demo or product under test.

Crabbox should stay generic:

- sync the exact checkout being reviewed;
- execute the declared command remotely;
- require bounded proof artifacts before reporting success;
- download evidence for reviewers, agents, or CI handoff.

This boundary keeps Crabbox from becoming an agent framework while still making
agent output auditable.

## Review Checklist

Before sharing a hermetic-agent run, verify:

- The proof JSON contains separate `code_writer` and `test_writer` manifests.
- Leak checks passed for both writer roles.
- QA verdict and blame assignment are present.
- Required artifacts were enforced by Crabbox, not only generated locally.
- Downloaded proof files contain no secrets or raw customer data.

For artifact semantics and storage limits, see [Artifacts](artifacts.md). For
delegated Islo behavior, see [Islo Provider](../providers/islo.md).
