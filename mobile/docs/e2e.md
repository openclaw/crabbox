# Headless end-to-end (`crabbox-sim`)

`crabbox-sim` proves the Crabbox iOS app's behavior without an iPhone, a
Simulator, or even Xcode. It drives the **exact** `CrabboxKit.reduce()` the
SwiftUI app uses, models WebView load effects deterministically, and asserts the
app's security and UI invariants after every single action. Because it depends
only on the portable targets, it builds and runs on Linux — which is what lets
it run on a sandbox.

## What it covers

- **18 checks** — 1 unit-vector check (`unit_vectors`, the dependency-free
  mirror of the XCTest cases for the pure URL/nav logic) plus 17 scenarios.
- **17 scenarios** — full Portal journeys: empty boot, boot from stored
  HTTPS/invalid coordinators, switching coordinators, HTTP rejection in prod,
  loopback HTTP acceptance in dev, LAN HTTP rejection, external-link tap
  (`mailto:`), internal navigation, back navigation (allowed and blocked),
  error-then-retry, reset-to-default, malformed URLs, go-home-after-drift, and
  draft-edit-clears-error.
- **13 invariants** — checked after *every* dispatched action in *every*
  scenario and for *every* LLM-chosen action. They encode the guarantees:
  current URL is always whitelisted-or-external; `homeURL` is always a
  normalized, non-empty coordinator after boot; SAVE persists
  `home == current == persisted`; RESET fully returns to the default; RELOAD
  touches only loading flags; settings/error coupling on SAVE/RESET; never
  `isLoading && loadFailed`; progress bounded and the loading bar never
  collapses below 6%; a URL error never coincides with a coordinator mutation;
  `webViewKey` is monotonic and bumps only on remount actions; GO_BACK emits its
  effect exactly when back navigation is possible; and no effect without a
  cause.

Each step is recorded in a trace, and scenario-specific `expect(...)` assertions
are collected alongside invariant violations so a single run reports every
problem at once. The process exits non-zero if any invariant is violated.

## Running it

```sh
swift run crabbox-sim          # all deterministic scenarios + invariants
swift run crabbox-sim --json   # machine-readable report (used in CI)
```

Human-readable output lists each check with `PASS`/`FAIL`, the step count, and
any violations, ending with a totals line. `--json` emits
`{ scenarios, totalSteps, violations, failures, ok }`.

### `--agent` — the tiny-LLM driver

```sh
swift run crabbox-sim --agent
```

This adds two exploratory runs (prod and dev `AppEnv`) in which a **tiny local
LLM** chooses the next action from a fixed vocabulary
(`OPEN_SETTINGS`, `ENTER_URL`, `TAP_CONNECT`, `TAP_RELOAD`, `TAP_HOME`,
`TAP_BACK`, `NAVIGATE`, …). The current `AppState` is rendered to compact text;
the model must reply with a single JSON action constrained to the allowed enum.

- Model: `qwen2.5:0.5b` via Ollama at `OLLAMA_HOST`
  (default `http://127.0.0.1:11434`), overridable with `CRABBOX_AGENT_MODEL`.
- **Deterministic fallback:** if Ollama is unavailable or returns something
  off-vocabulary, a rule-based selector picks an in-vocabulary action instead,
  so a missing model never fails the run. The LLM only *explores* — the 13
  invariants are always the judge.
- Step count is set with `CRABBOX_AGENT_STEPS` (default 24).

### `--chat` — real LLM smoke test

```sh
swift run crabbox-sim --chat
```

A live end-to-end test of the exact `SandboxEngine` the iOS Assistant ships. It
points at `OLLAMA_HOST` with `CRABBOX_AGENT_MODEL` (default `tinyllama`), checks
`isReady()`, sends a deterministic one-shot chat, and prints the reply. Exits
non-zero if the engine isn't ready — proving the engine the app uses works
against a real model.

### `--islo-demo` — the live "trigger islo from the phone" path

```sh
ISLO_API_KEY=ak_…  swift run crabbox-sim --islo-demo                     # lifecycle only
CRABBOX_ISLO_LLM=1 ISLO_API_KEY=ak_…  swift run crabbox-sim --islo-demo  # + LLM chat
```

Exercises the **exact `IsloClient` the app's Sandboxes tab uses** against the
real islo API (`api.islo.dev`): API-key→JWT exchange (`POST /auth/token`),
create → list → delete a sandbox, and — with `CRABBOX_ISLO_LLM=1` — the full
flow the phone drives: provision → detached Ollama bootstrap (the multi-minute
install + model pull runs detached because islo's `exec/stream` has a max
duration) → public share → chat → reply → cleanup. The sandbox is always
deleted, even on failure. Needs an islo key (`islo api-key create <name>`),
read from the environment and never passed on argv.

## Running on a sandbox

The deterministic suite (`swift run crabbox-sim`) needs no network and no model,
so it runs anywhere a Swift toolchain exists, including a freshly provisioned
sandbox. The `--agent` and `--chat` paths additionally need an Ollama endpoint
(local to the sandbox, or reachable via `OLLAMA_HOST`) with the relevant model
pulled. See [`../e2e/islo/run-on-sandbox.sh`](../e2e/islo/run-on-sandbox.sh) for
a script that provisions a sandbox (crabbox.sh as manager, islo.dev as the
direct provider), installs Swift, builds the package, and runs the simulator.

## In CI

GitHub Actions builds and tests `CrabboxKit` on Linux **and** macOS and runs
`swift run crabbox-sim --json` (deterministic, no Ollama/sandbox) on both. A
separate macOS job generates the Xcode project with XcodeGen and compiles the
iOS app build-only with code signing disabled — no device or Apple account
needed.
