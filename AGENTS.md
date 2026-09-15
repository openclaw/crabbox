# Repository Guidelines

`CLAUDE.md` is a symlink to this file. Edit `AGENTS.md` and preserve the link so agent instructions stay in sync.

## Project Structure & Module Organization

Crabbox is a Go CLI with an optional coordinator on Cloudflare Workers or Node.js with PostgreSQL.

| Area | Location |
| --- | --- |
| CLI entrypoint and command behavior | `cmd/crabbox`, `internal/cli` |
| Go provider adapters | `internal/providers/<name>` |
| Shared coordinator and Node.js runtime | `worker/src`, `worker/node` |
| Coordinator tests | `worker/test` |
| Command and feature documentation | `docs/commands`, `docs/features` |
| CI and release configuration | `.github/workflows`, `.goreleaser.yaml` |

Use `docs/source-map.md` for the detailed code map. Generated outputs such as `bin/`, `dist/`, `worker/dist/`, and `worker/node_modules/` should not be edited by hand.

## Working Approach

- Read the affected implementation, callers, and nearby tests before editing; use `VISION.md` for product scope. Identify the expected behavior and how to verify it. For changes spanning multiple boundaries, outline the approach before implementation.
- Make the smallest complete change that solves the requested problem. Preserve unrelated work; avoid incidental refactors, dependency upgrades, and repository-wide formatting.
- Treat design principles below as decision rules for the requested change, not a mandate to restructure existing code. Explain a necessary tradeoff when the simplest local fix would break an established contract.
- Keep this file focused on durable, actionable rules. Link to detailed documentation instead of duplicating it; verify commands and paths against the repository when updating guidance.

## Product Positioning

Crabbox is a generic remote software testing and execution tool. New code, docs, tests, and examples should not mention OpenClaw, Peter, or other project/person-specific workflows unless the file is explicitly about legacy compatibility or release history. Prefer neutral examples such as `example-org`, `alice@example.com`, `my-app`, `test:live`, and generic repository workflows.

## Architecture Boundaries

Keep core provider-neutral. Core may pass generic request/lease context and call provider capabilities for defaults, access, provision, images, release, cleanup, and diagnostics. Provider-specific reconciliation, firewall/security-group semantics, labels, snapshots, hosts, regions, rollout compatibility, and resource naming live behind provider adapters. No `provider == aws/gcp/...` logic in core unless it is unavoidable routing/config glue and no provider hook fits.

Before adding or changing a provider, read `docs/features/provider-authoring.md` and `docs/provider-backends.md`. Preserve shared behavior across the Cloudflare and Node.js coordinator runtimes; keep runtime-specific storage and scheduling in their existing adapters.

## Design Principles: KISS, YAGNI, DRY, SOLID

Use idiomatic Go and TypeScript; prefer existing abstractions and plain functions.

- **KISS / YAGNI:** Choose straightforward control flow. Add a helper, option, dependency, or extension point only when the current requirement needs it.
- **DRY:** Share policy with the same meaning and reason to change. Similar-looking provider code is insufficient reason to couple providers.
- **Single responsibility:** Separate orchestration, provider operations, persistence, and presentation. Split by responsibility, not arbitrary function or file length.
- **Open/closed:** Extend providers through existing registration and capability hooks; add a hook only when the current contract cannot express a real requirement.
- **Liskov substitution:** Preserve the advertised backend's errors, cancellation, ownership, and cleanup semantics. Reject unsupported requested capabilities before side effects; retain documented fallbacks.
- **Interface segregation:** Define small interfaces at the consumer. Use optional capabilities instead of adding unrelated methods to every backend.
- **Dependency inversion:** Pass external dependencies at existing boundaries so orchestration depends on behavior contracts. Keep concrete types where an interface adds no value.

## Build, Test, and Development Commands

Run from the repository root. Use the Go toolchain declared in `go.mod` and the Node version in `.node-version`.

- `go build -trimpath -o bin/crabbox ./cmd/crabbox`: build the local CLI.
- `go vet ./...`: run Go static checks.
- `go test -race -timeout=20m ./...`: run the Go test suite with the race detector and CI's race-test package timeout.
- `gofmt -w path/to/changed.go`: format changed Go files; substitute their actual paths.
- `npm ci --prefix worker`: install coordinator dependencies.
- `npm run format:check --prefix worker`: verify TypeScript formatting.
- `npm run lint --prefix worker`: run `oxlint`.
- `npm run check --prefix worker`: run TypeScript typechecking.
- `npm run check:node --prefix worker`: typecheck the Node.js coordinator.
- `npm test --prefix worker`: run Vitest tests.
- `npm run build --prefix worker`: dry-run the Worker build through Wrangler.
- `npm run build:node --prefix worker`: build the Node.js coordinator.
- `node scripts/build-docs-site.mjs`: generate the docs site into `dist/docs-site`.

## Coding Style & Naming Conventions

Use standard Go formatting and keep package names short and lowercase. Prefer table-driven Go tests where behavior has multiple cases, and keep command behavior close to the matching file in `internal/cli` (for example, cache behavior in `cache.go`). Worker code is TypeScript ESM; use existing module boundaries in `worker/src` and rely on `oxfmt`, `oxlint`, and `tsc`.

## Errors, Resources, and Compatibility

- Propagate cancellation and deadlines through HTTP, SSH, subprocess, and polling operations. Follow existing lifecycle ownership; cleanup that must outlive cancellation needs an independent, bounded context.
- Bound retries and preserve provider retry policy. A timeout does not prove creation failed: reconcile ambiguous results or use supported idempotency before retrying a resource creation.
- Make resource ownership and cleanup explicit on success, failure, and cancellation. Preserve exact ownership checks before destructive provider operations; never broaden cleanup to compensate for uncertain state.
- Return errors with useful operation context and preserve their cause. Keep the primary failure visible when cleanup also fails; do not turn a failed operation into apparent success.
- Preserve CLI flags, exit codes, JSON output, config semantics, and coordinator contracts unless the task explicitly changes them. Update affected docs and contract tests together.

## Testing Guidelines

- Put Go tests in `*_test.go` beside the code and coordinator tests in `worker/test/*.test.ts`. For bug fixes, reproduce the failure before changing behavior when practical, then keep the regression test.
- Test observable behavior at the lowest level that proves the contract. Cover relevant failure, cancellation, and cleanup paths. Use deterministic fakes at external boundaries; ordinary tests must not require live credentials or provision paid resources.
- Run the affected package or test file first, plus applicable formatting and static checks. Shared coordinator changes need checks/builds for both runtimes. Before release or broad changes, run the full CI-equivalent gate from the README and `.github/workflows/ci.yml`.
- For instructions-only changes, check referenced paths, commands, symlinks, and existing tests that validate the instructions. Application suites are unnecessary. Docs-site or generated-documentation changes need their relevant documentation checks.
- Investigate failures and keep assertions aligned with the intended contract. Do not disable checks or weaken assertions merely to obtain a passing result.
- Before handoff, review the final diff for unintended changes, compatibility regressions, and leaked credentials. Report what changed, the exact checks and outcomes, and any unverified behavior. Re-run affected checks after subsequent edits; an earlier pass does not validate newer changes.

## Commit & Pull Request Guidelines

History uses Conventional Commit prefixes such as `feat:`, `fix:`, `docs:`, and `ci:`. Keep commits focused and mention user-visible behavior changes. Pull requests should include a clear summary, verification commands, config or secret implications, and screenshots only for generated docs or UI changes. Issue/PR references: always use full GitHub URLs, every time.

Maintainers and agents add user-visible fixes and features to `CHANGELOG.md` as work lands, normally under `## Unreleased`; contributor PR authors leave changelog edits to maintainers. Use concise one-line bullets, full PR links, and contributor thanks by `@login`. Release preparation finalizes the accumulated section's version and date; do not defer changelog maintenance until release time.

## Releasing

Follow `docs/RELEASING.md` exactly. One explicit full release/publish request authorizes the complete normal sequence: preparation/tagging/build/signing, private draft/upload, native dispatch/proof, publication, ordinary Homebrew tap update, independent public/native/Go installation smokes, and closeout, without renewed chat approval at each stage. Narrow requests stay narrow. The original request is the authorization; GitHub events alone do not authorize a release. Sequential technical gates, identity binding, credential isolation, immutability, exact frozen inputs, immediate publication readbacks, and cancellation boundaries remain mandatory. Publication does not require a particular PR-approval ruleset or an administrative writer freeze; existing GitHub merge protections still apply. The final read and publication are not atomic, and a detected post-publication mismatch is an incident rather than permission to rewrite the release. Explicit cancellation requires renewed direction before mutations resume. Publication establishes tap eligibility; public smoke results are not an approval gate. Dispatch the existing ordinary tap updater explicitly and retry Homebrew alone after failure, never production or publication. Tap maintainers own executable formulae; evaluate them only credential-free. Cancellation cannot stop independent reconciliation of an already-public release. Pitfalls that have broken past releases:

- Before tagging on any maintainer Mac, that machine's SSH signing key must BOTH be in `.github/release-allowed-signers` AND be registered on the maintainer's GitHub account as a signing key. GitHub evaluates SSH tag-signature verification at push time only; a tag pushed before its key is registered is permanently `unknown_key` and can never pass `scripts/publish-release.sh`'s `verification.verified` gate. Check `gh api repos/openclaw/crabbox/git/tags/<tag-object> --jq .verification` immediately after pushing the tag, before building anything.
- The signed tag annotation must be exactly the bare version (`git tag -s v0.39.0 -m "v0.39.0"`), never a descriptive message. `scripts/verify-release-source.sh` requires the tag subject to equal the version, and the protected tag ruleset blocks deleting or recreating a wrong tag.
- Bump every version-carrying file, not just the changelog: the `CHANGELOG.md` section heading plus `worker/package.json` and both root entries in `worker/package-lock.json`. See the Release Checklist in `docs/operations.md`.
- The producer requires a merged authorize-source record at `release/records/vX.Y.Z.json` (binding the tag object and source commit) on `main` before `scripts/build-release-candidate.sh` will build; if the tag is recreated, update the record's `tagObject`.
- The producer is credential-free and refuses to run if any release credential is present; unset every variable in the check at the top of `scripts/build-release-candidate.sh` (the GitHub, Homebrew-tap, and Actions tokens plus the codesign identity and notary profile), not just `GH_TOKEN`/`GITHUB_TOKEN`.

## Security & Configuration Tips

For auth, isolation, or credential-handling changes, read `SECURITY.md` and `docs/security.md` first. Preserve the documented trusted-user/team model and supported workflows. Captured command output and failure bundles are not automatically scrubbed; review them before sharing.

Keep provider and broker tokens out of the repository. Do not pass secrets as command-line arguments. Local config belongs in `~/.config/crabbox/config.yaml`, `~/Library/Application Support/crabbox/config.yaml`, `crabbox.yaml`, or `.crabbox.yaml` as documented.
Tenki provider SSH uses `tenki sandbox ssh-proxy` with Tenki-managed key/cert files under `~/.config/tenki`; do not use Crabbox per-lease keys for gateway auth.
OpenComputer provider auth: Crabbox reads the API key from `CRABBOX_OPENCOMPUTER_API_KEY`/`OPENCOMPUTER_API_KEY` or the `oc` CLI config (`~/.oc/config.json`) and sends it only in the `X-API-Key` header — never persist `osb_` keys in Crabbox config or place them on argv.
