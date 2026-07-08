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

The best Crabbox fit is a **run-evidence pattern**, not a new agent framework:

- `crabbox run` executes the repo-owned hermetic harness.
- `--require-artifact` turns the proof JSON into a post-run gate.
- `--download` pulls the bounded proof files back for review or agent handoff.
- Provider choice stays ordinary Crabbox policy: Islo for delegated sandbox
  execution, or an SSH-backed Linux provider when the operator wants
  Crabbox-managed SSH and rsync.

Crabbox should not judge model output, store reasoning traces, decide whether a
test is correct, or deliver model credentials for this pattern. Those decisions
belong to the repo-owned harness and, later, to separately reviewed Station and
agent-runtime bridge work.

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

## Concept Map

| Crabbox concept | Fit in this demo |
| --- | --- |
| Run | One remote execution of the repo-owned hermetic harness. |
| Workspace | The synced checkout containing specs, guides, harness code, and output paths. |
| Provider | The remote execution substrate; the demo defaults to `islo` but does not require it. |
| Delegated mode | With Islo, the provider owns archive upload and command transport; Crabbox owns CLI semantics, claims, required artifacts, downloads, and status. |
| Artifacts | The proof JSON/Markdown are bounded run evidence, not raw transcripts or secrets. |
| Jobs | `.crabbox.yaml` names the repeatable remote proof as `hermetic-agents`. |
| History/logs | Brokered SSH providers can add central run history; direct/delegated runs still provide live output and local proof downloads. |
| Station | Future fit for long-running agent harnesses. This demo is intentionally a one-shot run. |

## Trust Boundary

This pattern does not turn Crabbox into a hostile multi-tenant sandbox. Treat the
repository config and harness as executable project automation, just like a
Makefile or CI workflow.

Keep these boundaries explicit:

- The code/test writer separation is enforced by the harness manifests, not by
  Crabbox itself.
- Crabbox can require that the proof file exists; it does not validate the proof
  schema unless the repo command does so.
- Provider API keys and model/tool credentials must stay out of repo YAML and
  command arguments.
- Proof artifacts should be small, bounded, and redacted before sharing.
- If a real model-backed version needs credentials, do not forward ambient
  secrets through `env.allow`; wait for a reviewed workload-specific credential
  path.

## Station Later

Long-running hermetic-agent systems may eventually fit Station better than
one-shot `run`: a station could supervise coder/tester/QA processes, record
attempt lifecycle, bridge a repo-owned harness API, and revoke model access on
stop. That is not what this demo uses.

Today, keep the path simple:

```text
repo harness -> crabbox run -> required proof artifact -> downloaded evidence
```

When Station and the agent-runtime bridge mature, the same proof schema can
become station evidence without moving prompt loops or test interpretation into
Crabbox core.

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

## Good Repo Config

Keep the Crabbox YAML focused on execution policy:

```yaml
jobs:
  hermetic-agents:
    provider: islo
    target: linux
    shell: true
    command: ./scripts/run_hermetic_agents_demo.sh
    requiredArtifacts:
      - docs/metrics/hermetic-agents-e2e.json
    downloads:
      - docs/metrics/hermetic-agents-e2e.json=.crabbox/proofs/hermetic-agents-e2e.json
      - docs/metrics/hermetic-agents-e2e.md=.crabbox/proofs/hermetic-agents-e2e.md
    stop: always
```

Keep the agent policy in repo-owned files such as:

```text
spec/problem.md
guides/code-writer.md
guides/test-writer.md
guides/qa-arbiter.md
scripts/hermetic_agents_demo.py
```

## Review Checklist

Before sharing a hermetic-agent run, verify:

- The proof JSON contains separate `code_writer` and `test_writer` manifests.
- Leak checks passed for both writer roles.
- QA verdict and blame assignment are present.
- Required artifacts were enforced by Crabbox, not only generated locally.
- Downloaded proof files contain no secrets or raw customer data.

For artifact semantics and storage limits, see [Artifacts](artifacts.md). For
delegated Islo behavior, see [Islo Provider](../providers/islo.md).
