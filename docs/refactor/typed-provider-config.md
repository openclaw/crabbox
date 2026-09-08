# Typed provider config bindings

Vercel Sandbox and CodeSandbox describe their mechanical config bindings once,
on the concrete structs in `internal/cli/config_vercel_sandbox.go` and
`internal/cli/config_codesandbox.go`. `scripts/configgen` reads each declaration
and emits its matching `_generated.go` file. Each generated file contains
pointer-valued YAML input fields, compiled defaults, file/environment overlays,
and flag storage, registration, and presence-based application.

This is a wiring refactor, not a behavior correction. Other providers retain
their existing configuration code. Provider selection, command routing, config
CLI presentation, and backend lifecycle are not part of generation.

## Why generation

Provider packages already import `internal/cli`, which owns `Config` and file
loading. Runtime typed descriptors would either stay in core and need a custom
YAML presence decoder, or require moving types to another package to avoid an
import cycle. A small standard-library Go generator preserves the existing
concrete runtime and YAML structs, reuses core's parsing helpers, and requires
no package relocation, new dependency, runtime reflection, or untyped config
bag. Struct tags are read only by the generator.

The tradeoff is checked-in generated code and a regeneration step. Normal builds
consume the output without running the generator. The source declaration remains
readable to Go tools; the output remains readable to reviewers. Field order is
source order, formatting uses `go/format`, and output contains no timestamps or
machine-specific paths. Its header identifies the generator and source file.

## Adding a field

1. Add an exported, singly named field to the provider's config struct. Supported types
   are `string`, `int`, `float64`, `bool`, and `[]string`.
2. Set its `config` YAML key, `env` variable, `flag` spelling, and `help` text.
   Explicitly set `sources:"user,repo,env,flag"` only after establishing that the
   value is safe in repository configuration and on argv. Use the exact
   `sources:"user,env,flag"` grant for an existing trusted-file-only binding;
   the loader's existing trust decision gates its file application. There is no
   implicit source grant. An optional `default` tag supplies a scalar default checked
   against the field type; otherwise the Go zero value applies. Current integer
   fields require `nonnegative:"true"` for eager file/environment validation.
3. Keep semantic and cross-field checks in the provider's
   validation function. Wire actual provider behavior there or in its
   existing client code as appropriate. Config presentation remains explicit in
   the existing config command, outside this generator.
4. Add contract tests for the field's presence, source precedence, invalid
   values, and provider behavior. Update the provider reference.
5. Run `go generate ./internal/cli`, review the generated diff, and run
   `go test -race ./scripts/configgen ./internal/providers/vercelsandbox ./internal/providers/codesandbox` plus the
   relevant configuration and CLI flag tests.

The standalone stale-output check, from the repository root, is:

```sh
go run ./scripts/configgen \
  -source internal/cli/config_vercel_sandbox.go \
  -output internal/cli/config_vercel_sandbox_generated.go \
  -type VercelSandboxConfig -provider vercel-sandbox -check
```

The generated-output freshness tests perform the same checks in ordinary
`go test ./...`. Generator tests cover deterministic output, missing/stale output
without writes, duplicate/missing bindings, unsupported types, default parsing,
and explicit source permissions. Do not edit the output by hand.

## Preserved contracts and security boundary

The loader still applies defaults, user files, repository files, environment,
and explicit flags in that order. Both repository filenames retain their
existing order. A YAML pointer distinguishes omission/null from explicit false,
zero, an empty string, or an empty list. Lists are trimmed and blank entries
removed, without deduplication. Empty environment strings fall through; a
nonempty list value containing only whitespace/commas clears the list. Existing
boolean environment aliases (`yes/no`, `on/off`, `1/0`) remain accepted.
Malformed boolean/float environment values keep the previous value, while
malformed or negative timeout values fail. These differences are preserved,
not standardized by this refactor.

Flags keep their names, help, types, defaults, and `flag.FlagSet` presence
semantics. Explicit false/zero/empty flags override earlier layers. Registration
never selects a provider; core still applies only the selected provider's flags
and records explicit provider selection. Vercel has no provider or config-field
aliases. Its existing credential environment aliases and their precedence stay
in runtime authentication code, outside the generated configuration surface.

Vercel deliberately has no token, auth-token, OIDC-token, API endpoint, or bridge
endpoint config field. Neither user nor repository YAML nor flags gain such a
surface. Runtime auth-store discovery, OIDC scope restrictions, credential
forwarding/redaction, and core's destination/provenance checks are unchanged.
CodeSandbox's eleven fields use the same generated bindings. Its `bridgeCommand`
and `sdkPackage` retain their existing trusted-user-file, environment, and flag
sources: repository files cannot replace or clear either value. The generated
file overlay receives the loader's existing `trusted` decision; it does not
infer trust from filenames. The other nine fields retain repository support.
CodeSandbox's provider aliases, generic sizing rejection, and semantic validation
remain in the provider wrapper, including their existing order.

The generator accepts only those two exact source grants. Credential
destinations, source-bound secrets, aliases, and provider selection policy stay
handwritten and require separate design before migration. Do not mark a
sensitive field as repo-safe just to make generation succeed.

Remaining providers can be considered individually after their existing
contracts are captured. This pilot does not mandate converting the full catalog
or moving provider types out of core.
