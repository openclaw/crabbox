# Typed provider config bindings

Vercel Sandbox, CodeSandbox, CUA, OpenSandbox, Anthropic Sandbox Runtime,
Cloud Run Sandbox, FastAPI Cloud, and Railway describe their mechanical config bindings
once, on the concrete structs in `internal/cli/config_vercel_sandbox.go`,
`internal/cli/config_codesandbox.go`, `internal/cli/config_cua.go`,
`internal/cli/config_opensandbox.go`,
`internal/cli/config_anthropic_sandbox_runtime.go`,
`internal/cli/config_cloud_run_sandbox.go`,
`internal/cli/config_fastapi_cloud.go`, and `internal/cli/config_railway.go`.
`scripts/configgen` reads each declaration
and emits its matching `_generated.go` file. Each generated file contains
source-admitted YAML input fields, compiled defaults, file/environment overlays,
and storage, registration, and presence-based application for admitted flags.

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
2. Flag-supported fields need their `flag` spelling and `help` text. Environment-supported fields need an
   `env` variable, and file-supported fields also need a `config` YAML key.
   Explicitly set `sources:"user,repo,env,flag"` only after establishing that the
   value is safe in repository configuration and on argv. Use the exact
   `sources:"user,env,flag"` grant for an existing trusted-file-only binding;
   the loader's existing trust decision gates its file application. An existing
   environment/flag-only field uses `sources:"env,flag"` and must omit the
   `config` tag entirely, including an empty tag. A CLI-only field uses
   `sources:"flag"` and must omit `config`, `env`, and `envAlias` tags entirely.
   An existing environment-only string uses `sources:"env"`: require its
   primary `env`, allow an existing alias, and omit `config`, `flag`, `help`, and
   `default` tags entirely. This mode retains a zero default and exposes no YAML
   or command-line field; it does not generate credential presentation or policy.
   There is no implicit source grant. An optional `default` tag supplies a scalar default checked
   against the field type; otherwise the Go zero value applies. Current integer
   fields require `nonnegative:"true"` for eager file/environment validation.
   A string field may name one existing fallback environment variable with
   `envAlias`; primary and alias names share collision checks. Empty aliases
   are invalid. The primary value wins, then the alias, then the prior value;
   empty values fall through, without trimming nonempty values.
   For an existing string file binding that ignores empty YAML values, declare
   `fileIgnoreEmpty:"true"`. This is valid only for strings with a file source;
   it adds an exact nonempty check without trimming, changing environment/flag
   behavior, or changing other fields' presence semantics.
   Use `reportApplied:"true"` only on string/bool fields whose accepted-input
   events are needed by an existing handwritten policy. See the report boundary
   below; this is not a new source grant.
3. Keep semantic and cross-field checks in the provider's
   validation function. Wire actual provider behavior there or in its
   existing client code as appropriate. Config presentation remains explicit in
   the existing config command, outside this generator.
4. Add contract tests for the field's presence, source precedence, invalid
   values, and provider behavior. Update the provider reference.
5. Run `go generate ./internal/cli`, review the generated diff, and run
   `go test -race ./scripts/configgen ./internal/providers/vercelsandbox ./internal/providers/codesandbox ./internal/providers/cua ./internal/providers/opensandbox ./internal/providers/anthropicsandboxruntime ./internal/providers/cloudrunsandbox ./internal/providers/fastapicloud ./internal/providers/railway` plus the
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

## Accepted input and flag presence

Opted-in declarations generate a provider-specific `Applied` report containing only
tracked fields. File/environment application returns the report with its error;
flag application returns the report. Bits are set inside the same accepted-input
branches that assign values, including assignments equal to the previous value.
Absent or ignored input, input disallowed by the field's source grant, and
parsing failures do not count as applied. Reports cover only opted-in fields.
Earlier accepted bits survive a later error, just as earlier config
mutations do; the report is not a transactional overlay. Defaults and flag
registration do not report input events.

Tracked fields with flags also produce a separate typed `VisitedFlags` query.
Generated assignment and existing core policy share this query's declaration and
visit predicate, but visits are not called applied values. Core retains its
existing post-success flag-provenance phase; provider wrappers do not acquire
that policy as a side effect. A declaration with no tracked flags emits no empty
visited-flags type or query. Non-opted-in providers keep their existing generated
signatures and output unchanged.

Reports contain mechanical facts, not permission decisions. Handwritten owners
map those facts to source enums, precedence, or other existing policy without
re-reading YAML predicates, reparsing environment values, or inferring intent
from value changes. Authentication, destination checks, redaction, and source
trust remain outside the generator.

## Preserved contracts and security boundary

The loader still applies defaults, user files, repository files, environment,
and explicit flags in that order. Both repository filenames retain their
existing order. A YAML pointer distinguishes omission/null from explicit false,
zero, an empty string, or an empty list. Only an explicit `fileIgnoreEmpty:"true"`
binding ignores an empty string; whitespace is still applied. Lists are trimmed and blank entries
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

CUA's fourteen runtime/flag fields include thirteen YAML fields. Its `APIURL`
retains environment/flag-only input and is absent even from trusted user YAML.
`CRABBOX_CUA_API_URL` retains precedence over `CUA_BASE_URL`. The four bridge/SDK
settings retain trusted-file-only admission; the other nine YAML fields remain
repository-safe. Its sizing guard still precedes the flag-value type assertion,
unlike CodeSandbox's wrapper. Read-only lifecycle restrictions are unchanged.

CUA's runtime fallbacks use the generated defaults as well. The Go bridge
resolves an empty or whitespace-only fallback import before supplying both JSON
and environment settings, preserving the effective SDK choice formerly supplied
by Python. Python retains request-over-environment precedence but no longer owns
duplicate import defaults. Other string fields retain their existing
blank-before-trim behavior. The fixed 15-second doctor budget and the Python
version check for the actual `cua_sandbox` module remain separate contracts.

OpenSandbox's twelve runtime/flag fields include ten YAML fields and eleven
environment fields. `APIURL` has no YAML source; `CRABBOX_OPENSANDBOX_API_URL`
retains precedence over `OPEN_SANDBOX_API_URL`. `ForgetMissing` remains CLI-only:
file and environment overlays leave it untouched, and only a visited flag copies
its parsed value. Early provider validation still checks only the two timeout
integers; URL, platform/resource and request-budget checks stay at their later
owners. No configuration layer gains cleanup authority.

Anthropic Sandbox Runtime's three fields retain user/repository file,
environment, and flag sources. Its `cliPath` ignores omitted, null, and empty
YAML values, while `settings` can be explicitly cleared and `debug: false`
overrides true. Nonempty whitespace still reaches the existing provider
validation, and an explicitly empty CLI flag still overrides and fails that
validation. The native binary fallback uses the same generated `srt` default.
The `srt` provider alias, native argument/environment handling, and SRT-owned
settings and sandbox-policy validation remain outside generation.

Cloud Run Sandbox's six fields include five YAML bindings; the gateway URL stays
environment/flag-only, including in trusted user config. The three string YAML
bindings ignore empty values without trimming, while explicit false values
still apply. Existing environment aliases, generic sizing guards, and validation
order remain in place. The launcher, doctor, cleanup hint, claim scope, and
workdir helper use the generated CLI/workdir defaults. Raw-zero config, operation
option precedence, keeper workdir omission, helper cwd, and timeouts retain their
separate semantics; generated defaults do not fill every empty runtime option.

FastAPI Cloud's four fields include an environment-only token and three
file/environment/flag values. All four existing environment aliases retain raw
nonempty precedence. Empty YAML values are ignored; a repository API URL still
records repository provenance and remains subject to the existing later
credential-destination checks. Applied reports carry accepted token/URL events
to the existing file/environment source mapping, while central flag provenance
uses the distinct visited query at its unchanged phase. Client token checks,
endpoint validation, redirects, service-control restrictions, and redacted
presentation remain handwritten and deferred as before.

Railway's four fields use the same existing mechanisms: an environment-only API
token, three nonempty-only YAML bindings, four environment alias chains, and
three flags. Accepted token/URL reports feed core's existing source mapping;
raw URL flag visits remain a separate post-success provenance step. The real
client shares the generated endpoint default, while claim scope and command
routing keep their existing behavior for an empty configured endpoint. Token-first
validation, Railway's own URL validator, provider aliases, bridge behavior,
HTTP timeout, and service lifecycle remain outside generation.

The generator accepts only these five exact source grants. Credential handling,
destination validation and provenance, provider aliases, and provider selection
policy stay handwritten. A declared environment alias copies the existing string
fallback only; it does not define credential forwarding or destination authority.
Do not mark a sensitive field as repo-safe just to make generation succeed.

Remaining providers can be considered individually after their existing
contracts are captured. This pilot does not mandate converting the full catalog
or moving provider types out of core.
