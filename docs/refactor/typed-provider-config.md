# Typed provider config bindings

Vercel Sandbox, CodeSandbox, CUA, OpenSandbox, Anthropic Sandbox Runtime,
Cloud Run Sandbox, FastAPI Cloud, Railway, Upstash Box, Cloudflare's container
runner, Cloudflare Sandbox, E2B, Blaxel, Azure Dynamic Sessions, SmolVM, Semaphore,
Tensorlake, Orgo, OpenComputer, Modal, Morph, exe.dev, OVHcloud, Lume, Runpod, Vast,
W&B, Scaleway, Tencent Cloud, DigitalOcean, Vultr, Linode, Sealos DevBox, KubeVirt,
Agent Sandbox, AWS Lambda MicroVM, Namespace Devbox, Namespace Instance, Coder, Multipass, and Machine0
describe their mechanical config bindings
once, on the concrete structs in `internal/cli/config_vercel_sandbox.go`,
`internal/cli/config_codesandbox.go`, `internal/cli/config_cua.go`,
`internal/cli/config_opensandbox.go`,
`internal/cli/config_anthropic_sandbox_runtime.go`,
`internal/cli/config_cloud_run_sandbox.go`,
`internal/cli/config_fastapi_cloud.go`, `internal/cli/config_railway.go`,
`internal/cli/config_upstash_box.go`, `internal/cli/config_cloudflare.go`,
`internal/cli/config_cloudflare_sandbox.go`, `internal/cli/config_e2b.go`,
`internal/cli/config_blaxel.go`, `internal/cli/config_azure_dynamic_sessions.go`,
`internal/cli/config_smolvm.go`, `internal/cli/config_semaphore.go`,
`internal/cli/config_tensorlake.go`, `internal/cli/config_orgo.go`,
`internal/cli/config_opencomputer.go`, `internal/cli/config_modal.go`,
`internal/cli/config_morph.go`, `internal/cli/config_exe_dev.go`,
`internal/cli/config_ovh.go`, `internal/cli/config_lume.go`,
`internal/cli/config_runpod.go`, `internal/cli/config_vast.go`,
`internal/cli/config_wandb.go`, `internal/cli/config_scaleway.go`,
`internal/cli/config_tencentcloud.go`, `internal/cli/config_digitalocean.go`,
`internal/cli/config_vultr.go`, `internal/cli/config_linode.go`,
`internal/cli/config_sealos_devbox.go`, and
`internal/cli/config_kubevirt.go`, `internal/cli/config_agentsandbox.go`, and
`internal/cli/config_aws_lambda_microvm.go`, `internal/cli/config_namespace.go`, and
`internal/cli/config_namespace_instance.go`, `internal/cli/config_coder.go`, and
`internal/cli/config_multipass.go`, and `internal/cli/config_machine0.go`.
`scripts/configgen` reads each declaration
and emits its matching `_generated.go` file. Each generated file contains
source-admitted YAML input fields, compiled defaults, file/environment overlays,
and storage, registration, and presence-based application for admitted flags.

Generation owns mechanical bindings, not provider policy. Other providers retain
their existing configuration code. Provider selection, command routing, config
CLI presentation, and backend lifecycle are not part of generation.

Crownest's complete five-field owner is `internal/cli/config_crownest.go`.
Its shallow file snapshot ignores a trimmed-blank URL only for application,
without changing the persisted input. Pointer strings, zero timeout and false
cleanup values retain their existing file semantics. Flag validation still
runs after all visited assignments; URL validation remains provider-owned.

A bool may have one `envAlias`: the primary wins when it parses, including
false, otherwise the alias is tried with the same bool parser. The explicit
`envInt:"checked-alias"` mode requires a nonnegative int and exactly one alias.
It selects by raw nonempty text, parses once through the shared strict parser,
names the selected variable on errors and preserves the old field on failure.
Earlier accepted effects are returned; later fields are not applied. Existing
strict and tolerant-fallback integer modes retain their distinct behavior.
These fixed modes add no parser callbacks, trimming or general alias policy.

Freestyle declares all five fields in `internal/cli/config_freestyle.go`.
Its API key remains environment-only, and the API URL admits trusted user files
but not repository files. Enforcing that rule in the file binding removes the
save-and-restore special case from both configuration loaders. The four file
fields retain value storage; file sizes accept only positive values, while
environment and flag integers retain their existing signed values until provider
validation. Generated constants also own the two configured fallback defaults;
endpoint validation, workspace containment and lifecycle behavior stay with the
provider.

Phala's complete six-field hybrid owner is `internal/cli/config_phala.go`.
Its nullable bool remains nil by default; accepted file, environment and ordinary
generated flag assignments copy into a fresh pointer, while ignored inputs keep
the previous pointer. File storage remains a single pointer with its existing
omission behavior. The primitive's scalar bool flag defaults to false for nil;
Phala supplies its existing effective default through a local registration copy,
without changing runtime configuration. Pointer defaults, aliases and unrelated
scalar/list modes are not accepted.

Phala uses manual flag application with six generated bindings and one separate
provider-owned skip control, not a seventh configuration member. The imperative
application order and existing policy remain unchanged. Its typed file snapshot
wrapper retains conditional admission without modifying the file DTO. File paths
expand only when accepted, environment fallback paths expand unconditionally,
and flag paths remain raw. The ordinary registration order of distinct named
flags does not affect name-sorted help, metadata or parsing. Native provider and
attestation algorithms remain outside this owner.

Firecracker's complete fifteen-field owner is `internal/cli/config_firecracker.go`.
It uses existing value-string, present-int, tolerant integer environment and
raw-positive duration modes. Eight file strings remain trusted-user-only;
the three pointer integers retain explicit signed values and the release bool
retains explicit false. File duration text stays raw for persistence.
The initializer preserves the shared WorkRoot default through a typed alias,
with the remaining thirteen nonzero defaults generated from the declaration.
The flag wrapper consumes earlier accepted path/user/root effects before a
timeout error, leaving the later release value, marker, intent and final default
phase untouched on that error. Environment path expansion remains unconditional
on the final fallback values. Backend defaults and native operations stay
outside the generated owner.

Incus uses a complete hybrid owner in `internal/cli/config_incus.go`. The exact
type-doc directive `//configgen:flag-application manual` generates file/env
application, flag storage and registration, and a typed raw-visit query for all
admitted flags, but no flag `Apply` method. Its provider retains the existing
ordered enum/duration checks, projections, accepted-input records and final
normalization. Raw visits are not accepted-input facts. The directive must occur
once on the selected type and requires at least one flag; no callbacks or stages
are encoded in metadata. Ordinary declarations retain their generated output.

Runtime-only fields may add unique `yaml:"-"` and `json:"-"` omission tags to
`sources:"runtime"`; other serialization tags and values are rejected. Incus's
checkpoint metadata remains runtime-only and omitted from both serializations.
Its initializer retains the shared WorkRoot default through a typed alias;
generated defaults own the remaining compiled values. File paths expand only
after accepted assignments, while environment paths expand their final fallback
values unconditionally. The imperative flag path keeps its original ordering.

Sealos DevBox uses all sixteen bindings together. Its fifteen file strings retain
value storage and trusted-user admission; the pointer-backed release boolean
also accepts repository input and preserves explicit false. Applied facts for
the two host paths let file and flag wrappers expand only accepted values;
environment wrappers expand the final fallback values unconditionally. The
existing path algorithm, guest-work-root rules, explicit markers, and validation
remain outside generation. Moving path expansion immediately after assignment
is valid here because these string/bool bindings are non-fallible and no
intermediate observer reads them; it is not a generic delayed-normalization rule.

KubeVirt's complete twelve-field binding uses a mandatory typed file wrapper.
The wrapper copies file input and applies the existing conditional public-key
admission rule to that snapshot before calling the generated overlay. A rejected
snapshot value becomes an ignored empty assignment, not a request to clear the
runtime value. The original file object remains available unchanged to the
writer. The generated overlay is a mechanical primitive; production file
callers must go through the wrapper rather than treating source tags alone as
the complete admission policy.

Accepted reports retain KubeVirt's six host-path transformations and release
marker. File/flags expand only accepted paths; environment processing expands
final fallback values unconditionally. Only flags copy the raw provider work
root to the generic root, without introducing a new explicitness bit. The
provider owns the shared inherited-root decision used by configuration and
command forwarding; its distinct routing and trim-aware fallback rules stay
separate. No generator callback or new normalization policy is introduced.

Each provider owns an `ExpandAppliedLocalPaths` method used by its file and
flag wrappers. It consumes the actual applied report and preserves the ordered
field transformations; it does not change acceptance, markers, or guest paths.
Environment fallback expansion stays explicit and does not manufacture an
all-true applied report.

Agent Sandbox uses all twelve bindings together, including both `time.Duration`
timeouts. Its seven file strings remain trusted-user-only and value-backed;
duration strings, the integer and booleans retain repository admission. The file
wrapper expands only accepted Kubeconfig input, including partial results before
an integer error. Environment processing expands the final inherited-or-new
Kubeconfig even when no override was accepted. The delete marker stays outside
generation and is applied only when its field was reached. Flag application
copies all visited values before the provider's existing validation.

Duration support is deliberately one fixed mode: canonical standard-library
`time.Duration` with `duration:"positive-overlay"`. File-admitted fields must
also declare `fileStorage:"value"`; their YAML fields remain raw strings, not
parsed durations or pointers. File/env input calls the existing positive,
tolerant `applyLeaseDuration` helper without trimming. That wrapper delegates
parsing and assignment to the strict `ApplyLeaseDuration` helper, discarding its
error for file/environment overlays; flag callers retain their error policy.
Invalid, zero and negative
inputs do not replace the current runtime value, but raw file values survive
configuration writes. By default, flags use `flag.Duration` and copy explicit
zero/negative values before provider validation; they do not inherit file/env
acceptance. The Namespace string-flag exceptions are described below.
Declared defaults must parse to a positive duration and produce typed constants;
an omitted default remains zero. Arbitrary qualified types, alternate parsers,
callbacks and additional file/environment duration policies are not supported.

Blacksmith uses all six bindings together, with four string flags and two
file/environment-only fields: a positive-overlay duration and a pointer-backed
file boolean. These flagless fields retain zero defaults and do not introduce
timeout or debug flags. Its four environment coordinates apply earlier than its
timeout and debug settings. `envSplitBefore:"true"` on the timeout field preserves
that boundary by generating `applyEnvPrefix` and `applyEnvSuffix` instead of a
combined `applyEnv`. Both use the same field emitter and return ordinary applied
reports; the loader records each report at its original position. An intervening
error therefore cannot apply later fields early. At most one split is allowed,
on an environment-admitted field with a nonempty environment prefix and suffix.
The split adds no callbacks, normalization, or provider-specific generation.

AWS Lambda MicroVM uses all seven bindings together. Its four string flags trim
only accepted values; file/environment strings remain raw. The existing flat
AWSRegion flag stays in the provider wrapper and applies before generated fields,
with provider validation last. It is not a new nested Region setting.

Its connector lists use two fixed source modes. `envList:"csv"` calls the existing
`splitCSV` after a raw-nonempty guard: absent/empty input preserves the prior
list, whitespace yields nil, and comma-only input yields a nonnil empty list.
`flagList:"scalar-empty-nil"` keeps joined defaults and last-scalar-wins parsing,
but returns nil for every all-empty result. Unlike `empty-scalar`, registration
does not discard inherited defaults. Both keep order, duplicates and literal
`none`; `fileList:"raw"` separately retains cloned pointer-list input and writer
presence. Native connector fallback selection remains outside generation.

Namespace Devbox uses all eight bindings together. Its duration retains the raw
positive-overlay file/environment rule, but opts into
`flagDuration:"trim-positive"` with a required literal `flagDurationError`.
That flag is registered as a string with the configured duration's `.String()`
default. Application trims and parses at the field's original position; a
nonpositive or malformed value returns exit 2 without assigning the duration or
later fields. String-duration modes add an error result to flag application. Existing
adopters retain their signatures and generated bytes.

The provider wrapper consumes the partial Size fact before returning a duration
error, preserving uppercase normalization and generic server-type effects.
WorkRoot mirroring and delete markers remain later accepted effects. File/env
wrappers consume only delete facts, not flag-only normalization or mirroring.
Three pure getter fallbacks use the same named configured defaults; their
trim rules and provider-timeout-before-generic-timeout precedence remain intact.
Class-derived Size selection and native provider behavior are separate.

The strict flag mode has no parser callbacks, configurable error code or runtime
policy registry. Its diagnostic is literal text, not a formatting template.

Namespace Instance declares all nine input fields while retaining its tenth,
runtime-only `TenantID` member in place. The exact standalone tag
`sources:"runtime"` excludes a field from generated defaults and every input
surface; combining it with any other tag is rejected. The file DTO still has
nine fields in the same order, including a raw duration string and value slice.

Its `fileList:"raw"` plus `fileStorage:"value"` binding treats nil as absent,
clones nonnil input, and clears to nil for an explicit empty slice. The writer
continues omitting both nil and empty slices. The existing presence-aware
environment list parser is unchanged. `flagList:"append-trimmed"` snapshots the
inherited list and appends each whole trimmed occurrence, including empty strings,
commas and literal `none`; it neither splits CSV nor clears on first use. It
reuses the ordinary list flag's storage/rendering/defensive Getter methods and
registers after the scalar flags, preserving the original registration order.

`flagDuration:"raw-zero-reset"` retains Namespace Instance's string flag:
trimmed `0s` resets to zero; otherwise the existing raw `ApplyLeaseDuration`
helper accepts empty input as a no-op or a positive duration without trimming.
Invalid input returns the ordinary duration error, preserving earlier field
assignments and skipping later fields and the provider's selected defaults call.
It does not use Devbox's trimmed-positive/exit-2 rule.

File assignments can follow declaration order because they are independent,
non-fallible overlays with no intermediate observer. Accepted CLI path expansion
follows the overlay and still runs only for accepted file input. Environment
processing expands the final fallback path unconditionally; flags remain raw
until the provider's unchanged selected-defaults phase. Native instance defaults,
TenantID derivation, configuration display and provider execution stay unchanged.
The two raw-empty native defaults and the exact default-CLI routing comparison
reuse the same configured constants; no trim or fallback rule changes. The `nsc`
doctor label remains a tool name, not a configurable default.

Coder declares all ten bindings together. Its distinct file DTO retains seven
value strings, two pointer booleans and a value parameter list, in the same order.
The existing `UnmarshalYAML` method stays handwritten beside the declaration,
unchanged: its plain-DTO decode can reject input before the later scalar branch.
Generation does not add a new scalar-YAML compatibility promise.

`fileList:"nonempty-normalized"` accepts a nonempty file list and delegates to
`normalizeList`, preserving fresh storage and a nonnil empty result for all-blank
input. Nil and empty lists preserve prior configuration; file writer omission
remains separate from runtime application. `envList:"trimmed-nonempty"` ignores
wholly blank input, then uses the shared environment-list value parser: a whole
case-insensitive `none` clears to an empty slice, otherwise ordinary CSV applies.
The presence-aware environment wrapper uses that same parser without changing
its existing absent-versus-present behavior. `flagList:"csv"` reuses `splitCSV`,
retaining scalar last-occurrence-wins parsing, joined defaults, blank-to-nil and
comma-only-to-empty results. Literal `none` is not a flag selector. The duplicate
provider-local flag splitter is removed.

File path expansion consumes accepted CLI/RichParameterFile facts after the
independent, non-fallible overlay. Environment handling still expands both final
fallback paths unconditionally, and flags keep them raw. The provider wrapper
retains its selected sizing/target guards before the type assertion, generic
WorkRoot mirroring after accepted flags, and selected validation after all fields.
No delete marker or configuration-display surface is added. Four configured
constants replace equal literals in existing fallback consumers, retaining their
raw-versus-trimmed predicates and order. Provider names, SSH usernames, enum
values, native execution and transport remain separate contracts.

Multipass declares all eight input fields together, retaining six value strings,
a value integer and a raw duration string in its file DTO. Strings stay raw and
nonempty-only, including CLIPath: no local path expansion is added. File CPUs
apply only when positive; environment CPUs retain tolerant parsing with accepted
zero/negative values, and flags retain their existing unrestricted integer input.
File/environment durations keep the tolerant positive-only overlay.

Its `flagDuration:"raw-positive"` mode registers a string and calls the existing
`ApplyLeaseDuration` helper at the field's original position. Raw empty input is
a no-op, positive input assigns, and padded/nonpositive/invalid input returns
the existing ordinary error. It neither trims nor treats `0s` as a reset, and
rejects `flagDurationError`. Partial applied facts let the provider preserve
earlier explicit-image and generic user/root effects before returning a timeout
error. Only successful application reaches its existing selected-default phase.

`initialMultipassConfig` combines six generated fixed defaults with the supplied
OS-derived image and the existing shared POSIX work-root constant. The named
work-root default aliases that constant instead of duplicating its literal, and
initialization does not mark the image explicit. File/env wrappers consume only
accepted Image facts, not flag-only generic user/root effects. The pure runtime
default function shares four configured fallbacks but keeps its existing
predicates and inherited-root resolution; it does not reset CPU, memory or disk.
Its lower `26.04` image fallback remains separate from portable OS selection.
Native VM lifecycle, mounts and commands are unchanged.

Machine0 uses the existing mechanisms for all eleven inputs, while SizeExplicit
remains a runtime-only member in its original position. Its pointer-backed
ImageVersion accepts explicit zero and negative file values; environment parsing
is tolerant and flags keep signed integers, with validation at its existing later
phase. Eight file strings stay raw and nonempty-only, and both raw duration
strings retain their original DTO representation. CLI paths are not expanded.

Both duration flags use the same raw-positive mode in their original order.
Applied Size and WorkRoot facts preserve earlier explicit-size and generic
server-type/root effects before either duration error is returned. An error in
PollInterval retains an already applied CreateTimeout. File/env Size acceptance
sets only the runtime explicitness bit, including values equal to the default;
absent input preserves it. The selected-provider predicate and default callback
remain unchanged. Backend defaults already read the core initializer, so no
backend rewrite is needed. Empty WorkRoot remains the dynamic resolved-user
sentinel. Live catalog selection, key lookup, and native lifecycle stay outside
the generated bindings.

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

Not every fallback is a base configuration default. exe.dev keeps Image and
WorkRoot raw-empty: native creation omits an unspecified image, and work-root
resolution can inherit the generic root. Named runtime/display constants beside
the declaration share those fallback values without `default` or `flagFallback`
tags, while their existing raw-versus-trimmed predicates stay with the callers.

OVHcloud shares configured endpoint, image, and flavor defaults without coupling
them to its fixed regional endpoint aliases or machine-class profiles. Image
explicitness still records accepted input, including a value equal to the default;
it is not inferred from whether the final value differs from that default.

Lume shares its configured CLI, base, user, and work-root defaults while retaining
its user-dependent runtime root calculation. Changing the guest user can replace
the old default root with `/Users/<user>/crabbox`; this trim-aware decision and
native storage resolution remain outside generation.

Runpod likewise keeps User and WorkRoot raw-empty so runtime defaults can inherit
the generic SSH user and work root. Its named runtime fallbacks are not field
defaults. A nonzero file disk value is an accepted input event; Runpod's later
repair of nonpositive disk sizes to 20 remains provider policy.

Vast uses accepted-input reports to normalize InstanceType only after a visited
flag; its file/environment bindings keep the raw value for later provider phases.
Its work-root and release-action markers remain handwritten policy. Nonzero integer
file overlays ignore zero, while ordinary float overlays accept explicit zero;
neither rule moves Vast's validation or post-flag defaulting into generation.

W&B keeps its raw image and lifetime empty/zero, with named runtime fallback
constants beside the declaration. The provider retains its raw-versus-trimmed
image decisions and TTL rounding/clamp. Its integer environment alias uses nested
tolerant parsing: a malformed primary falls back to the alias, while a parsed zero
or negative primary wins. Its vendor login and netrc resolution stay client-owned.

Scaleway shares its four configured defaults while preserving explicit-source
markers, SDK location precedence, and independent portable-OS/class mappings.
Its list file input accepts only nonempty raw lists; scalar flag input registers
an independent empty default and produces nil for empty results. Environment
parsing retains its distinct nonnil empty result. These are source-specific
assignment rules, not a shared normalization policy.

Tencent Cloud preserves signed 64-bit numeric bindings and an entirely zero-valued
raw configuration. Named runtime constants share effective fallback values without
initializing flag defaults. Its trusted endpoint admission, four explicit markers,
class matrix, market selection, and service-family endpoint policy remain separate.

DigitalOcean has no provider flags. Its declaration owns file/environment input
and a used zero constructor; the generator emits no flag storage, registration,
application, or presence APIs for a flagless schema. Runtime region/image values
remain separate from portable-OS mapping and lower generic-field inheritance.

Vultr reuses the same flagless bindings without extending the generator. Its two
raw file lists retain sharing and its environment lists retain their own empty
representation. Its handwritten `WithRuntimeDefaults` value method owns raw-empty
region/user-scheme filling at the existing core and backend phases and supplies
read-only user-scheme projections. It preserves other fields and slice sharing;
the generated raw constructor stays zero-valued. The lower region helper retains
its separate generic-location fallback. SSH-user policy, native boot-source
parsing, and OS catalog selection remain outside this transformation.

Linode combines generated flagless bindings with a concrete typed initializer.
The initializer supplies configured region/type defaults and copies the image
already resolved by core's portable-OS mapping, including an empty image. It
does not repeat that lookup or introduce an eager image fallback. Accepted image
and type inputs still set their existing explicit-source markers; later OS
selection, class policy, and validation before backend defaults remain separate.
All five file fields retain their legacy value-backed YAML storage, so saving
configuration omits ignored empty strings and lists without adding defaults.

## File storage and configuration writes

File assignment and file persistence are separate contracts. Commands such as
`config set-broker` read and rewrite the whole user configuration. A pointer to
an explicit empty value survives YAML `omitempty`, whereas the corresponding
value field is omitted. Ignoring an empty overlay does not establish which
representation a provider historically used.

File fields remain pointers by default, preserving explicit false, zero, empty
strings and empty-list clears. Use `fileStorage:"value"` only when the field's
original value-backed storage and zero-ignoring assignment rule are established.
The generator then emits a value field with the same YAML tag and a raw-value
predicate; it does not normalize the stored value or change environment/flag
handling. An explicitly present empty provider block remains `{}`.

| Kind | Required file rule |
| --- | --- |
| `string` | `fileIgnoreEmpty:"true"` |
| `[]string` | `fileList:"nonempty-raw"`, `fileList:"nonempty-normalized"`, or nil-presence/cloning `fileList:"raw"` |
| `int`, `int64` | `fileInt:"positive"` or `fileInt:"nonzero"` |
| `float64` | `fileFloat:"positive"` |
| `time.Duration` | raw string with `duration:"positive-overlay"` |

Apart from the explicit raw value-slice combination, presence-sensitive rules,
booleans and fields without file input cannot use value storage. Existing
pointer-backed fields must not opt in merely because they
ignore zero. Modal's `secrets: []` deliberately remains pointer-backed so a write
retains the explicit clear. Positive-only numeric overlays still store negative
inputs verbatim: ignoring an assignment is not permission to erase its file value.

## Apple VM's concrete source owner

`config_apple_vm.go` owns all eight settings, initialization, and complete file
and environment application without generated bindings. Runtime and file types
remain distinct: three pointer-backed file integers preserve explicit zero and
null/omitted values, so deriving the file type from runtime values would lose a
real storage contract. Initialization accepts the already-selected image and
checksum pair without repeating OS-image selection or setting explicit markers.

Shared accepted-image and checksum operations own the associated value and
marker changes together. File and environment callers retain their raw-input
acceptance; flags call those operations only after their existing trimming and
validation. Source selection, legacy precedence, signed numeric parsing, exact
errors and earlier mutations on failure remain explicit and ordered.

The provider retains its sixteen current/deprecated flag spellings, current-name
visit precedence, image-identity display, early validation and selected-provider
defaults call. Native behavior and default-image selection are not source-input
events and do not use the explicit-image transition. No generic alias, callback
or staged-validation framework is added to the generator.

## Lambda's concrete owner

Lambda uses `config_lambda.go` without generated bindings. Its structured mount
list and different file/environment image-pair rules remain explicit typed code.
`LambdaConfig` owns one eight-field YAML shape; the defined
`type fileLambdaConfig LambdaConfig` preserves the existing file-input type name
in decoder diagnostics without duplicating the fields. File input stays zero-valued,
and runtime initialization is a separate function. No JSON tags or custom marshal
methods are added; the supported file writer and config-show formats stay unchanged.
Ad-hoc YAML serialization of the raw runtime type is not a supported contract.

The source applicators and existing mount parser live with that concrete owner.
`WithRuntimeDefaults` owns raw-empty region/type filling and the image-family
fallback when both image and family are empty. Runtime initialization delegates
from the zero value. Core applies the transform before its explicit OS override;
the backend retains generic server-type projection before applying it. Trim-aware
provider lookups remain separate from these raw-empty rules.
Native mount filtering, lookup, credentials, class selection and OS mappings remain
provider policy. The following field instructions apply to generated owners.

## Local Container's concrete source owner

`config_local_container.go` keeps eleven runtime members distinct from nine
file/environment settings. The file type retains two pointer booleans and its
existing omissions. CLI-only volumes and transient checkpoint metadata remain
runtime data, not new persisted settings; their list/map identities survive
ordinary source application. The initializer accepts the resolved image and
keeps the raw work root empty without marking compiled defaults explicit.

Three accepted-input operations pair Runtime, Image and WorkRoot assignments
with their existing source markers. File/environment predicates and scalar flag
visitation stay with their current callers. The flag work-root operation sets
the provider bit before the independent generic root copy/snapshot; later
observers see the same state, including the existing empty-snapshot semantics.
No concurrency guarantee or callback layer is introduced.

Flag storage, raw volume-list behavior, creation-only checks and provider
defaults remain provider-owned. NoHostname stays file/environment-only. This
concrete owner needs neither generated runtime-state exclusions nor a new flag
list framework; native, socket, mount and checkpoint behavior is unchanged.

## Apple Container's shared concrete owner

`config_apple_container.go` owns all seven shared settings without generated
bindings. A YAML-tagged `AppleContainerConfig` and distinct defined
`fileAppleContainerConfig` retain one field roster, the existing file type name,
and zero-valued decoding. File persistence and the six-field config-show view
remain unchanged; raw runtime JSON retains its existing shape. Ad-hoc YAML
serialization of the runtime type is not a supported compatibility contract.

Initialization accepts the already-resolved OS image, including an empty value,
rather than resolving it again. File input clones nonempty argument lists;
environment input ignores empty whitespace-tokenized results; a visited flag
clears the list to nil when tokenization is empty. These are distinct policies,
not interchangeable list modes.

The owner provides two typed flag surfaces: seven Apple Container flags and four
Apple Machine flags with their existing names and help. They are not aliases or
a configurable prefix factory. Provider wrappers retain image markers, generic
user/root propagation, and the exact selected Container defaults call. OS-image
selection and native Container/Machine behavior remain outside this owner.

## Adding a field

1. Add an exported, singly named field to the provider's config struct. Supported types
   are `string`, `int`, `int64`, `float64`, `bool`, `[]string`, and the fixed
   `time.Duration` binding described above. Existing runtime-only members can
   instead use only the exact `sources:"runtime"` tag; no binding is generated.
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
   An existing string, string list, boolean, or positive-overlay duration with
   file/environment input but no flag uses the exact
   `sources:"user,repo,env"` grant: require `config` and primary `env`, and omit
   `flag`, `help`, and `default` tags entirely. It retains a zero default and
   uses existing file predicates and applied reports without adding a flag or
   changing trust policy. This grant does not permit a file input on an
   environment-only field.
   String lists, booleans, and durations are admitted only by this untrusted-file-capable
   no-flag grant, with their existing file/environment rules; other no-flag grants remain
   string-only. Duration file storage stays a raw string, boolean file storage
   stays a pointer, and neither gains a flag. A schema with no flag-admitted
   fields emits no placeholder flag
   API or flag import. Mixed schemas retain their admitted flag bindings.
   A trusted-file/environment string without a flag uses the exact
   `sources:"user,env"` grant with the same absent flag/help/default requirement;
   its file assignment uses the loader's existing trusted decision.
   There is no implicit source grant. An optional `default` tag supplies a scalar default checked
   against the field type; otherwise the Go zero value applies. Current integer
   fields require `nonnegative:"true"` for compiled-default validation and
   ordinary file/environment checks; the explicit source modes below preserve
   providers whose input rules differ.
   An existing source-specific integer can opt into `envInt:"fallback"` to use
   core's `getenvInt` or `getenvInt64` for environment input only. It requires an environment
   source, `int` or `int64`, and the existing nonnegative policy; empty or unknown modes are
   rejected. File rules remain independently selected, flags remain deferred, and
   malformed environment input keeps the previous value while parsed negatives
   retain each provider's existing later handling. No parser function is supplied
   by the tag.
   Environment-admitted `int64` fields currently require this fallback mode;
   strict `int64` environment parsing and aliases are not generated. File fields
   use `*int64` by default or `int64` with `fileStorage:"value"`; flags use
   `flag.Int64`, with no platform-width conversion.
   Compiled `int` defaults retain the existing signed 32-bit check; `int64`
   defaults are checked at signed 64-bit width.
   An existing positive-only integer file binding can opt into
   `fileInt:"positive"`: only a present value greater than zero assigns;
   omitted/null/zero/negative input is ignored. It requires an int or int64 with file
   admission and the existing nonnegative default policy. This fixed predicate
   changes no environment or flag behavior and accepts no custom expressions.
   `fileInt:"present"` instead applies every nonnil integer pointer, including
   zero and negative values. It requires the same file-admitted int/default
   policy, but deliberately adds no file-value check. Omitted/null fields remain
   ignored; environment parsing is still selected independently.
   `fileInt:"nonzero"` applies a present value only when it differs from zero,
   including negative values. Omitted/null/zero input preserves the prior value.
   It requires the same file-admitted int or int64 and nonnegative compiled-default policy,
   but adds no file-negative rejection. Environment and flag behavior do not change;
   later validation or default repair remains with the provider.
   A file-admitted `float64` with the same existing positive-only YAML rule can
   use `fileFloat:"positive"`. It emits the literal greater-than-zero predicate
   without changing float parsing or adding finite/range validation to file or
   environment input. Float default validation remains separate; `fileInt` and
   the integer-only nonnegative policy do not become float policies.
   A string field may name an existing fallback environment variable with
   `envAlias`, and a second with `envAlias2` only when the first is present.
   All names share collision checks; empty aliases and fields without environment
   admission are rejected. Integer fields permit exactly one `envAlias` only with
   `envInt:"fallback"`, using nested `getenvInt` calls so a malformed primary falls
   back to the alias and then the prior value; parsed zero and negatives still win.
   Strict integers and other non-string types reject aliases, and `envAlias2`
   remains string-only. For string aliases, the primary value wins,
   then the first alias, then the second, then the prior value. Empty values
   fall through without trimming nonempty values. No arbitrary alias list or
   custom parser is accepted.
   An existing single-alias string binding whose configured value outranks the
   alias can declare `envAliasAfterConfig:"true"`. It requires environment
   admission and exactly one alias, with no `envAlias2`. A raw nonempty primary
   assigns first; otherwise the alias assigns only when the current config value
   is exactly empty. Applied reports follow those accepted branches, not value
   changes. This fixed rule performs no trimming and is not a general precedence
   list or runtime credential resolver.
   A flag-admitted string with an existing raw-empty registration fallback can
   declare `flagFallback:"value"` instead of a `default` tag. The value must be
   nonempty. Its generated constant supplies only the flag's raw-empty fallback;
   the base config stays zero, whitespace is preserved, unvisited flags do not
   assign, and explicitly empty flags still clear. Runtime consumers may use
   the same constant through their existing fallback logic. No expression,
   trimming mode, or duration parser is generated.
   For an existing string file binding that ignores empty YAML values, declare
   `fileIgnoreEmpty:"true"`. This is valid only for strings with a file source;
   it adds an exact nonempty check without trimming, changing environment/flag
   behavior, or changing other fields' presence semantics.
   Existing list bindings can opt into fixed source-specific rules on `[]string`:
   `fileList:"raw"` clones a supplied YAML list without normalization, preserving
   raw elements, order, and duplicates; omission/null preserves the prior value,
   while an explicit empty list clears it. `fileList:"nonempty-raw"` instead
   ignores nil/empty lists and directly assigns a nonempty raw list without
   cloning, preserving its backing-array sharing. `envList:"presence"` delegates to
   core's `getenvList`, including present-empty and `none` clearing. The repeatable
   `flagList:"replace-append"` uses one shared flag-value implementation: first
   occurrence clears configured defaults, later occurrences append, and each
   comma-separated occurrence trims and drops blanks without deduplication.
   Registration and application clone the list; unvisited flags do not assign.
   `flagList:"append-trimmed"` instead appends whole trimmed occurrences to the
   inherited snapshot, preserving empty strings and commas as literal values.
   It registers after scalar flags and keeps defensive copy semantics.
   Coder's `fileList:"nonempty-normalized"`, `envList:"trimmed-nonempty"`, and
   `flagList:"csv"` preserve the three distinct list contracts described above.
   `flagList:"empty-scalar"` instead registers an empty string independently of
   configured values. Repeated occurrences use the last scalar, and application
   trims comma-separated items, drops blanks, and returns nil when none remain.
   Unvisited flags preserve the prior list; ordinary environment parsing remains
   independent and can produce a nonnil empty list.
   These modes require their corresponding admitted source and reject unsupported
   values or types. They accept no custom parser, separator, or expression and
   leave ordinary list bindings unchanged.
   Use `reportApplied:"true"` only on string/bool fields whose accepted-input
   events are needed by an existing handwritten policy. See the report boundary
   below; this is not a new source grant.
   A file-admitted string can declare one `configAlias` YAML key. Its assignment
   follows the primary immediately, using the same trust and empty-value rules,
   regardless of document order. An accepted alias sets the same opted-in report
   bit. YAML names and generated input member names must not collide. This does
   not add environment aliases, flags, alternate parsing, or alias-specific policy.
3. Keep semantic and cross-field checks in the provider's
   validation function. Wire actual provider behavior there or in its
   existing client code as appropriate. Config presentation remains explicit in
   the existing config command, outside this generator.
4. Add contract tests for the field's presence, source precedence, invalid
   values, and provider behavior. Update the provider reference.
5. Run `go generate ./internal/cli`, review the generated diff, and run
   `go test -race ./scripts/configgen ./internal/providers/vercelsandbox ./internal/providers/codesandbox ./internal/providers/cua ./internal/providers/opensandbox ./internal/providers/anthropicsandboxruntime ./internal/providers/cloudrunsandbox ./internal/providers/fastapicloud ./internal/providers/railway ./internal/providers/upstashbox ./internal/providers/cloudflare ./internal/providers/cloudflaresandbox ./internal/providers/e2b ./internal/providers/blaxel ./internal/providers/azuredynamicsessions ./internal/providers/smolvm ./internal/providers/semaphore ./internal/providers/tensorlake ./internal/providers/orgo ./internal/providers/opencomputer ./internal/providers/modal ./internal/providers/morph ./internal/providers/exedev ./internal/providers/ovh ./internal/providers/lume ./internal/providers/runpod ./internal/providers/vast ./internal/providers/wandb ./internal/providers/scaleway ./internal/providers/tencentcloud ./internal/providers/digitalocean ./internal/providers/vultr` plus the
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
existing order. Presence-sensitive YAML bindings use pointers to distinguish
omission/null from explicit false, zero, an empty string, or an empty list.
Value-backed fields retain their declared zero-ignoring rules. Only an explicit
`fileIgnoreEmpty:"true"` binding ignores an empty string; whitespace still applies.
List normalization follows each declared source mode: raw file lists stay raw,
while ordinary comma-separated environment input trims blanks without deduplication.
Ordinary empty environment strings fall through; presence-based list bindings
can clear on empty input. Existing boolean aliases (`yes/no`, `on/off`, `1/0`)
remain accepted. Malformed boolean/float environment values keep the previous
value. Strict nonnegative integer bindings reject malformed or negative input;
explicitly tolerant integer bindings retain their documented fallback behavior.
These differences are preserved, not standardized by generation.

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

Its runtime fallback helpers also use the generated bridge-command, SDK-package,
operation-timeout, doctor-list-limit, and workdir defaults. The provider still
owns when to apply them: strings are trimmed before falling back, and the two
integer helpers fall back for nonpositive values. The fixed SDK workspace mount
`/project/workspace` is a separate platform boundary, not a configurable default;
changing the default workdir must not move archive staging or mount-replacement
behavior. Cleanup budgets and command-timeout extensions remain independent.
The Go list producer sends the resolved positive limit to the embedded bridge;
the bridge does not choose a second list default.

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

Upstash Box's six fields include an environment-only API key, four nonempty-only
YAML strings, and a presence-based `keepAlive` boolean. Accepted key/endpoint
reports feed the existing source mapping; endpoint flag visits retain central
post-success provenance. Generated constants also supply the client, endpoint
host and claim scope, runtime, size, workdir, and core server-type fallbacks.
Their existing normalization is preserved, including core's raw size fallback
and narrower provider spelling match. Exact provider alias guards, subsequent
validation, the fixed workspace root, uploads, and lifecycle policy stay with
their existing owners.

Cloudflare's container runner declares all three string fields. Its token keeps
existing nonempty file/environment admission without a flag; the URL and workdir
retain flags. Accepted URL/token reports feed the same core source mapping, and
raw URL flag visits stay in the central post-success phase. Provider class/type
normalization still precedes flag-value assertion, and URL/token/type validation
remains deferred to the client. The Go workdir fallback uses the generated
constant. The bundled Worker's omitted-field HTTP defaults remain a separate
protocol contract because the Go client supplies its resolved workdir explicitly.
The distinct Cloudflare Sandbox provider keeps its own declaration and rules.

Cloudflare Sandbox's five fields include six YAML inputs: trusted `bridgeUrl`
followed by its trusted `url` alias, an optional trusted token without a flag,
and ordinary workdir, timeout, and forget-missing values. Explicit alias empty
overrides the primary; null/omission does not. All allowed strings retain
presence-based clearing. File/env timeout errors retain earlier mutations and
precede later boolean application. No provenance report is added where the
provider had none. Validation order, optional authentication, timeout zero,
raw create workdir, and the dedicated `/workspace` descendant rule are unchanged.
Only the Go workdir fallback shares the generated default; external bridge and
bundled Worker protocol defaults remain separate.

E2B's six strings include an environment-only API key, five nonempty-only YAML
bindings, five flags, and three environment aliases. Accepted key/API URL/domain
reports feed the existing source policy, with URL and domain visits still marked
centrally after successful flag application. The generated constants also supply
the eight configured/default-chain consumers in client, normalized claims,
bridge/preview domains, acquisition, core template display, and workdir resolution.
Their raw-empty versus trimmed-empty differences remain intact. Raw scope and
routing, user-home roots, and the fixed missing-remote-template display fallback
remain separate owners. Upload and lifecycle code are not changed by this binding
migration.

Blaxel's eleven fields retain trusted endpoint/workspace file input, an
environment-only API key, mixed ignored-empty/presence-based YAML strings, and
their existing flag/validation order. Only MemoryMB uses the tolerant environment
integer mode; its file negatives remain eager, while exec timeout stays strict.
No provenance reporting is added where none existed. The seven configured
default consumers share generated values without changing their normalization.
API versions, lifecycle budgets, memory service defaults, upload and retry policy
remain separate owners.

Azure Dynamic Sessions declares all five fields, including legacy Pool without
a flag. Its timeout uses positive-only file admission and tolerant environment
parsing; neither moves validation or short-circuits the configured-positive,
TTL-positive, final-default timeout chain. Endpoint reports and central visits
retain core provenance policy. API version and workdir share their Go defaults;
Azure routing, native authentication and session behavior stay with their
existing owners.

SmolVM declares all eight fields, including its environment-only three-name key
chain. CPU and memory retain positive-only file admission and tolerant environment
parsing; explicit flags and validation order remain separate. Endpoint input
reports and central flag visits retain existing provenance ownership. Its six
configured fallback consumers share constants, while raw-empty network behavior,
fixed mount/upload roots, endpoint trust, and lifecycle remain unchanged.

Semaphore declares all six string fields without filling its raw empty defaults.
Machine, OS image, and idle timeout share three fallback constants across flag
registration and their existing acquisition, display, and duration helpers.
Host/token reports and central host visits preserve source policy; token has no
flag. Host/project validation, job identity, SSH, and lifecycle stay outside the
generator.

Tensorlake declares all fourteen fields, including positive-only YAML CPU floats
and three positive-only file integers with tolerant environment parsing. API-key
and API-URL input reports and central URL flag visits retain their existing
ownership. Three configured fallback consumers share constants; native namespace
pinning, omitted image/snapshot/nonpositive sizing, native CLI identity checks,
and lifecycle remain separate.

Orgo declares all seven fields while preserving its raw primary-environment,
configured-key, vendor-environment ordering. The backend factory owns effective
defaults; the client no longer contains unreachable ambient API-base fallbacks.
Its later key resolution remains unchanged because raw configuration admission
and trimmed runtime resolution are different reachable stages. Defaulting and
claim-scope consumers share constants without changing their normalization.

OpenComputer declares all eight fields, with four presence-based file integers,
an env/flag-only API URL whose raw default remains empty, and CLI-only
ForgetMissing. Its API key remains outside Crabbox config. Two configured
fallback consumers share constants; external OC-file resolution, the single
client-owned built-in URL, request-level timeout fallback, and lifecycle remain
unchanged.

The generator accepts only these seven exact source grants. Credential handling,
destination validation and provenance, provider aliases, and provider selection
policy stay handwritten. A declared environment alias copies the existing string
fallback only; it does not define credential forwarding or destination authority.
Do not mark a sensitive field as repo-safe just to make generation succeed.

Remaining providers can be considered individually after their existing
contracts are captured. This pilot does not mandate converting the full catalog
or moving provider types out of core.
