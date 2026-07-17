# Repository Guidelines

## Project Structure & Module Organization

Crabbox is a Go CLI plus a Cloudflare Worker coordinator. The CLI entrypoint is `cmd/crabbox`, with implementation and Go tests in `internal/cli`. Worker source lives in `worker/src`, with Vitest tests in `worker/test`. Documentation lives in `docs/`; command docs are under `docs/commands`, and feature notes under `docs/features`. Release configuration is in `.goreleaser.yaml`; GitHub Actions live in `.github/workflows`. Generated outputs such as `bin/`, `dist/`, `worker/dist/`, and `worker/node_modules/` should not be edited by hand.

## Product Positioning

Crabbox is a generic remote software testing and execution tool. New code, docs, tests, and examples should not mention OpenClaw, Peter, or other project/person-specific workflows unless the file is explicitly about legacy compatibility or release history. Prefer neutral examples such as `example-org`, `alice@example.com`, `my-app`, `test:live`, and generic repository workflows.

## Architecture Boundaries

Keep core provider-neutral. Core may pass generic request/lease context and call provider capabilities for defaults, access, provision, images, release, cleanup, and diagnostics. Provider-specific reconciliation, firewall/security-group semantics, labels, snapshots, hosts, regions, rollout compatibility, and resource naming live behind provider adapters. No `provider == aws/gcp/...` logic in core unless it is unavoidable routing/config glue and no provider hook fits.

## Build, Test, and Development Commands

- `go build -trimpath -o bin/crabbox ./cmd/crabbox`: build the local CLI.
- `go vet ./...`: run Go static checks.
- `go test -race ./...`: run the Go test suite with the race detector.
- `gofmt -w $(git ls-files '*.go')`: format Go files.
- `npm ci --prefix worker`: install Worker dependencies.
- `npm run format:check --prefix worker`: verify TypeScript formatting.
- `npm run lint --prefix worker`: run `oxlint`.
- `npm run check --prefix worker`: run TypeScript typechecking.
- `npm test --prefix worker`: run Vitest tests.
- `npm run build --prefix worker`: dry-run the Worker build through Wrangler.
- `node scripts/build-docs-site.mjs`: generate the docs site into `dist/docs-site`.

## Coding Style & Naming Conventions

Use standard Go formatting and keep package names short and lowercase. Prefer table-driven Go tests where behavior has multiple cases, and keep command behavior close to the matching file in `internal/cli` (for example, cache behavior in `cache.go`). Worker code is TypeScript ESM; use existing module boundaries in `worker/src` and rely on `oxfmt`, `oxlint`, and `tsc`.

## Testing Guidelines

Name Go tests `*_test.go` beside the code they cover. Name Worker tests `*.test.ts` under `worker/test`. Add regression tests for bug fixes when practical. Before handoff, run the relevant subset; before release or broad changes, run the full CI-equivalent gate from the README.

## Commit & Pull Request Guidelines

History uses Conventional Commit prefixes such as `feat:`, `fix:`, `docs:`, and `ci:`. Keep commits focused and mention user-visible behavior changes. Pull requests should include a clear summary, verification commands, config or secret implications, and screenshots only for generated docs or UI changes. Issue/PR references: always use full GitHub URLs, every time.

## Releasing

Follow `docs/RELEASING.md` exactly; the flow is gated and mostly irreversible. Pitfalls that have broken past releases:

- The signed tag annotation must be exactly the bare version (`git tag -s v0.39.0 -m "v0.39.0"`), never a descriptive message. `scripts/verify-release-source.sh` requires the tag subject to equal the version, and the protected tag ruleset blocks deleting or recreating a wrong tag.
- Bump every version-carrying file, not just the changelog: the `CHANGELOG.md` section heading plus `worker/package.json` and both root entries in `worker/package-lock.json`. See the Release Checklist in `docs/operations.md`.
- The producer requires a merged authorize-source record at `release/records/vX.Y.Z.json` (binding the tag object and source commit) on `main` before `scripts/build-release-candidate.sh` will build; if the tag is recreated, update the record's `tagObject`.
- The producer is credential-free and refuses to run if any release credential is present; unset every variable in the check at the top of `scripts/build-release-candidate.sh` (the GitHub, Homebrew-tap, and Actions tokens plus the codesign identity and notary profile), not just `GH_TOKEN`/`GITHUB_TOKEN`.

## Security & Configuration Tips

Keep provider and broker tokens out of the repository. Do not pass secrets as command-line arguments. Local config belongs in `~/.config/crabbox/config.yaml`, `~/Library/Application Support/crabbox/config.yaml`, `crabbox.yaml`, or `.crabbox.yaml` as documented.
Tenki provider SSH uses `tenki sandbox ssh-proxy` with Tenki-managed key/cert files under `~/.config/tenki`; do not use Crabbox per-lease keys for gateway auth.
OpenComputer provider auth: Crabbox reads the API key from `CRABBOX_OPENCOMPUTER_API_KEY`/`OPENCOMPUTER_API_KEY` or the `oc` CLI config (`~/.oc/config.json`) and sends it only in the `X-API-Key` header — never persist `osb_` keys in Crabbox config or place them on argv.
