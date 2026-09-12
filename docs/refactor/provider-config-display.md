# Provider configuration display

`ProviderConfigShowProjector` gives a provider one passive, output-only field
definition for JSON and text. The shared collector and renderers consume
`ProviderConfigShowSection` and its ordered fields. Providers choose the public
field names and already-redacted values; the renderer does not inspect runtime
structs, infer visibility from input tags, or discover credentials or defaults.

Freestyle and Crownest are the first adopters. Both sections appear even when
another provider is selected. Freestyle reports only presence for an API key
already loaded into configuration; Crownest does not look up its separate key.
Zero sizes/timeouts, explicit false, and raw strings retain their configured
meaning. Neither section establishes authentication or readiness.

OpenComputer, OpenSandbox and CUA use the same passive capability for all 34
of their configuration fields. Their separate credential sources are not read.
CUA's bridge and SDK strings are references only: projection does not execute a
command, inspect an installation, resolve imports or enable provisioning.

## Ownership

- `JSONValue` is an explicitly selected public value, never a runtime-config dump.
- `TextValue` is the corresponding explicitly formatted, already-redacted value.
- Format-specific names are independent: published camel-case JSON and
  snake-case text names need not match. A field may intentionally appear in one
  format only; each section must provide both formats.
- `Providers` declares canonical coverage. A shared configuration owner can
  cover multiple registered providers without emitting duplicate sections.
- Existing URL redaction and configured/missing formatting are shared through
  `ConfigShowURL` and `ConfigShowSecretState`; presenters must not introduce
  new environment, file, SDK, CLI, or network reads.

Collection rejects duplicate keys, labels, field names and coverage. JSON
insertion also rejects collisions with the existing top-level view before
adding any section. Text reserves the existing line labels without evaluating
JSON values, which would repeat legacy environment-presence reads. A source
test keeps that label inventory aligned with the actual formatters.

New sections have stable text-label order immediately before the existing
offline inspection/status block. Existing lines and fields keep their current
order. Output errors, including short writes, propagate from text rendering.

Multipass, Tart and Lume now define their existing JSON/text fields through the
same passive API. Their sections are consumed at their original text positions:
`docker_sandbox`, Multipass, `machine0`, Tart, Lume, `cloudflare`. A small layout
tracks which supplied sections have been written, then emits only the remaining
sections before inspection. It does not consult the provider registry, resolve
missing data, or fall back to the old formatters. An absent optional section is
a no-op for this data-only renderer; real-provider tests and whole-binary output
comparisons establish the migrated built-ins' actual coverage and positions.

Local Container, Apple Container, MXC and Docker Sandbox also own their existing
35 display fields and retain their original text slots. Apple Container supplies
one shared section for both Apple Container and Apple Machine; it does not apply
either backend's runtime defaults. MXC retains JSON lists with text counts,
Docker Sandbox retains JSON lists with comma-joined text and `%g` CPU formatting,
and nil versus empty lists remain distinct. Internal-only fields stay omitted.

AWS, Azure and GCP own their existing 24 JSON fields and 17 text fields through
the same capability. Seven fields remain JSON-only rather than gaining new text
output. AWS's 32-bit and GCP's 64-bit disk sizes, ordered lists and exact text
slots are preserved. Instance-profile and service-account values remain
configured references; these projections do not inspect SDK credentials or add
authentication or readiness facts.

DigitalOcean, Vultr and Linode own all 17 of their existing JSON/text fields.
Their consecutive sections remain between Azure and GitHub Codespaces. The
projectors retain raw boot selectors and list shapes without resolving images,
applying SSH defaults or discovering authentication. Selected-provider defaults
and explicit SSH settings remain the existing configuration loader's concern.

Blacksmith Testbox, Agent Sandbox and Firecracker own all 33 of their existing
JSON/text fields. Their sections retain the original text positions. Workflow,
tool and path values remain references only; projection does not consult a CLI,
cluster or guest assets. Raw JSON strings, empty-only text dashes, duration
strings, integer sizes/timeouts and explicit booleans keep their existing forms.

Parallels owns its existing 15 JSON fields and 12 text fields, retaining its
original text position. Its typed template map and host list are copied before
loaded key values become presence markers; nested JSON names, nil/empty shapes
and host order stay unchanged. Text retains collection counts and its existing
raw strings and empty-only fallbacks. Projection does not select templates,
resolve hosts or read key files, and runtime-only selected-host state stays omitted.

Static SSH owns its six existing string fields through the canonical `ssh`
provider. Its published JSON key and text label remain `static`, including when
selected through the `static` or `static-ssh` aliases. Port remains a string,
text keeps empty-only dashes, and the section retains its original slot without
resolving an SSH target or changing the separate generic SSH display.

## Remaining migration

The baseline census contains 81 canonical providers: 49 have both value formats,
three have JSON only, and 29 have neither. Apple Machine shares Apple Container's
configuration, so complete coverage means **80 distinct sections**, not 81
duplicate sections.

The five newly added sections leave **24 missing sections and three missing text
sections**. They do not complete the migration. Existing provider projections
also still need to move out of the parallel JSON map and text formatter so
their field selection and transformations have one owner.

The Multipass/Tart/Lume, local-container, cloud, VPS, runtime, Parallels and Static SSH cohorts remove nineteen
of the original 49 canonical both-format providers from that legacy
implementation, leaving 30 in that cohort. The local cohort covers five
identities through four sections because Apple Machine shares Apple Container's
values. These migrations do not fill any of the missing sections above.

For each remaining provider, establish the explicit public field contract
before implementation. Preserve existing keys, types, null/empty distinctions,
raw-versus-display defaults, redaction, count-versus-list text summaries and
format-only fields. Keep nested External configuration allowlisted or summarized;
do not expose opaque maps. Paths and secret names are references, not permission
to read their contents. Runtime-only state stays omitted.

When migrating an existing text section, render it in its original layout slot
and retire the corresponding legacy label reservation together. Do not move
existing sections into registry order. Preserve Apple sharing and the generic
lines interleaved with provider sections. Built-in coverage is complete only
when every canonical provider has exactly one intended section; test fixtures
must not stand in for missing real providers.
