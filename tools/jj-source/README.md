# Native JJ source helper

This package reconstructs the native source helper from pinned upstream JJ and
gix archives. It is development tooling for the native-source integration.
The candidate CLI integrates `sync.source=jj`; release publication and full platform
acceptance are still pending. This tooling does not publish artifacts or change
release/signing policy.

`manifest.json` pins the upstream JJ commit/version, both archive checksums,
patches, helper files, and the complete prepared source tree. The gix archive
checksum comes from upstream JJ's locked dependency. Other Cargo dependencies
remain locked. The old experimental helper programs and Cargo's `.cargo-ok`
cache marker are not packaged.

## Prepare and build

Use Node.js, Git, tar, Go, and the Rust toolchain required by the pinned JJ source.
Go's standard executable readers perform static format checks; the input helper
is never run by those checks. The builder checks Go availability before Cargo.
Download the two public archives from the URLs in `manifest.json`; downloads
need no credentials. The materializer performs no network requests.

```sh
node tools/jj-source/materialize.mjs \
  --jj-archive /path/to/jj-source.tar.gz \
  --gix-archive /path/to/gix-0.87.1.crate \
  --output /path/to/new-source-directory
```

The destination must not exist. Archive and package hashes are checked before
creation. A temporary, private Git metadata directory owns patch application;
the source never inherits an enclosing repository's index or Git configuration.
Preparation and runtime fixtures use owned empty Git config files rather than
null-device paths, whose handling differs between Windows Git ports.
Failed preparation removes only directories created by this invocation and
reports cleanup failures. Successful preparation prints a JSON source receipt.

From the prepared source directory, build the helper without release credentials:

```sh
cargo build --locked --offline -p jj-cli --bin crabbox-jj-source \
  --no-default-features --features git
```

The helper is a production binary under `cli/src/bin/crabbox-jj-source`, not a
Cargo example. Example builds activate the CLI's self dev-dependency and testing
features; do not use `--example`, tests or `--all-targets` for release inputs.
The pinned CLI dependency explicitly enables Tokio's `net` feature for its I/O
runtime initializer; Unix process support alone does not supply that API on
Windows. The helper's requested package feature remains `git`.

`--offline` requires the locked Cargo dependencies to be cached first. Release
optimization and cross-target compilation use Cargo's normal flags; a source
receipt is not a signing, native-platform, or bit-for-bit binary reproducibility
claim. The source-tree digest covers sorted UTF-8 relative file paths and file
content hashes plus upstream symlink targets, not host filesystem modes or
timestamps. File paths and symlink targets encode native separators as `/`
without resolving the targets. Archive extraction must preserve those upstream
symlinks.

Run the materializer integration test with the same downloaded archives:

```sh
CRABBOX_TEST_JJ_ARCHIVE=/path/to/jj-source.tar.gz \
CRABBOX_TEST_GIX_ARCHIVE=/path/to/gix-0.87.1.crate \
node --test tools/jj-source/materialize.test.mjs
```

## Build an installation pair

For a complete native build and attribution handoff, use:

```sh
node tools/jj-source/produce.mjs --source /path/to/prepared-source \
  --output /path/to/new-native-build
```

This performs the release-profile build, captures fresh Cargo package metadata
under the same isolated build environment, collects observed-build attribution,
and assembles the verified four-file `bundle/`. Private `build.json` and
`metadata.json` remain outside that bundle. It neither signs nor publishes, and
unresolved attribution remains explicit. Dependencies must already be cached for
the target: compilation and metadata resolution both run locked and offline.
Failed production removes only its new work directory; an existing destination
is never replaced.

The native-host builder verifies the complete prepared source before and after
Cargo, runs the helper's version/capability check, and writes a new installation
directory containing the helper and `crabbox-jj-source.json`:

```sh
node tools/jj-source/build.mjs --source /path/to/prepared-source \
  --output /path/to/new-install-directory --profile dev
```

The builder's JSON report also records observed Cargo compiler-artifact package
IDs, target kinds and features, including cached artifacts. These are private
build-input evidence (path-package IDs can contain local paths), not an exact
linkage inventory. Join those IDs to fresh Cargo metadata for package/license
facts; metadata's broader resolved feature graph is not the build selector.
The installed two-file pair and its receipt schema are unchanged.

The default profile is `release`; neither profile signs, publishes, nor installs
anything into the user's PATH. Builds are offline and use an isolated temporary
HOME while retaining the original effective Cargo and Rustup homes. Output and
temporary build state must be outside the prepared source. Run on the intended
native host with its Rust/linker toolchain; platform mappings are not evidence
that every target has been qualified.

A trusted release or local build/install owner places both companion files beside
the corresponding Crabbox executable. Runtime lookup resolves the executable's
real location and checks its sibling receipt, source identity, host target, and
binary digest. It never discovers the helper through a checkout or PATH. The Go
checker embeds this package's `manifest.json`, so there is no separately maintained
source-hash constant.

The receipt detects installation mismatches. It is not a signature and does not
authenticate arbitrary software supplied by an untrusted local writer. Release
signing, published-artifact verification, notices, and native platform acceptance
remain separate requirements.
Protocol paths use the compatible native spelling for ordinary canonical Windows
roots. Paths that cannot be safely simplified retain their spelling, and the
managed-state transfer guard continues to reject unsupported device paths.

Run the portable source smoke with a separately built stock JJ executable
and a candidate CLI installed beside its native companion pair:

```sh
node tools/jj-source/smoke.mjs --jj /path/to/stock/jj \
  --crabbox /path/to/installed/crabbox --output /path/to/new-smoke-directory
```

It creates a disposable non-colocated workspace with UTF-8 files, a recorded
executable, and a relative symlink. A stock JJ checkout supplies the host-native
materialization oracle: recorded export must match its bytes, kinds, link targets,
and modes, with native Windows attributes recorded and compared on Windows.
Recorded and live installed CLI plans are checked after ordinary working-file
changes. The smoke requires Go to build a standard-library-only file-info oracle:
plan size estimates must match Go's native `Lstat` sizes on the stock checkout
and live files, separately from exact content and symlink-target checks. Node's
libuv uses symlink-target lengths on Windows, unlike Go's filesystem size field.
Source/admin contents, kinds, modes, and last-write timestamps must remain
unchanged during reads. The output retains the synthetic fixture and receipts;
reported capability limits do not silently skip paths. This smoke does not
establish remote SSH lifecycle, signing, or full release acceptance.

The `Native JJ Qualification` workflow runs six native Darwin/Linux/Windows
architecture combinations on pull requests that change the source or CLI. It
uses `qualify.mjs --target OS_ARCH --output NEW_DIRECTORY` to prepare the pinned
source, fetch locked dependencies, run the focused Rust test, build pristine stock
JJ from the original pinned archive, produce the companion bundle, and run the
source smoke with a fresh native CLI. The stock oracle uses `git,tokio/net`
without candidate patches; its source and build features are recorded separately.
Before compilation, the workflow checks native archive listing and attribution
reads against the already verified extracted gix files. Listing line endings are
decoded separately from member names; file contents remain byte-exact.
Node, hardware, Rust host, Go host/target, and Windows compiler
selection are checked rather than inferred from runner labels. The workflow
retains a permission-preserving bundle archive and bounded receipts, not private
Cargo metadata or credential-bearing runner state. It does not publish releases
or replace the separate lifecycle, attribution, and signing gates.
On failure, qualification retains architecture facts and, when
available, a relative-path/file-digest inventory of the pinned source for
comparison across hosts, plus the synthetic smoke's captured context/export
responses, plans, and native file-size observations. Failed source identity checks still remove their
new source directory; diagnostics do not authorize mismatched build inputs.

The resulting `crabbox-jj-source` speaks the versioned protocol used by the Go
adapter. The installed-sibling checker, environment projection and candidate plan/run
routing are implemented, with native macOS source/SSH lifecycle proof and
independent source-blind behavior evaluation. Unresolved attribution and remaining native
platforms and final release qualification remain before publication.

The GNU Linux helper is dynamically linked, not a musl/Alpine binary. The current
x86-64 and ARM64 candidates were built and exercised in native Ubuntu 22.04
userspace; both ELF inventories require glibc symbols through 2.34, the standard loader, libc,
libm and libgcc. This is a measured build baseline, not a minimum-kernel or
all-distribution compatibility claim. Release qualification must retain the
actual interpreter, linked-library and symbol-version inventory alongside native
runtime proof; a successful ELF format check alone does not establish that floor.

The local reader preserves native JJ configuration inputs and confirmed Git
config-file/indexed overrides, worktree/shallow/namespace/replacement settings,
and object-cache limits. Native libraries still interpret those inputs; Go does
not reinterpret Git configuration. Unrelated provider/broker credentials and
loader, trace, editor/pager, SSH and HTTP environment controls are not inherited.
Arbitrary conditional environment settings use the parent boolean protocol.
Explicit VCS configuration values can be private and stay local; this projection
is not a prohibition on configuration keys stored inside native config files.
Only the helper's read-only source operations are reachable.

## Upstream notices

JJ is copyright The Jujutsu Authors and licensed under Apache-2.0. gix is part of
gitoxide and is licensed under MIT OR Apache-2.0. Their license and notice files
remain in the prepared upstream source. Keep the corresponding notices with
redistributed source and binaries.

## Recorded snapshots

Recorded preparation selects paths from the captured native tree inventory,
using the shared manifest matcher. It does not use today's file existence or
kind as authority for historical files. Configured/canonical managed-state
namespaces remain excluded without traversing current candidate-parent aliases.
Current Crabbox exclusion rules are read and rechecked in the original source
context, not loaded from historical staging.

The payload and native checkout state each have a staging cleanup owner. Native
export must produce every selected manifest member; rendered size guardrails and
content hashing run against staging. Checkout state is removed before the accepted
payload is returned. No current live bytes are compared to historical bytes.
Empty selections remain valid. Transport integration must apply the normal
deletion guardrails to them as well.

## Native artifact handoff

`artifacts.mjs` owns the six native targets and the installation receipt shared
with the builder. `verifyReleaseInputs(directory)` accepts exactly six target
subdirectories (`darwin_amd64`, `darwin_arm64`, `linux_amd64`, `linux_arm64`,
`windows_amd64`, `windows_arm64`), each containing exactly its native helper,
`crabbox-jj-source.json`, `crabbox-jj-source.NOTICES.txt`, and `attribution.json`.
The runtime pair remains the helper and receipt; `verifyReleaseBundle` owns the
four-file distribution inventory. Inputs must have release-profile receipts matching the
pinned source, target triple and file digest. Static checks use Go's standard
Mach-O, ELF and PE readers; they do not execute the binaries or prove their
source provenance, operating-system compatibility or native runtime behavior.

The protected release packager must copy and sign the frozen Darwin helper,
verify that signed copy through the existing signature/notarization policy,
then call `finalizeDarwinReleaseBundle` with the original bundle and signed copy.
It writes a new bundle, updates only the installed binary digest, copies the
notice artifacts unchanged, checks that the original input is unchanged, and
returns both original and finalized records. Notices remain bound to the original
unsigned build receipt, not the signed binary digest. Preserve
both records in release provenance. Rebinding a receipt is not authentication
and cannot establish that an independently supplied binary came from the build.
Never carry an unsigned-byte receipt unchanged across signing.

`artifacts.mjs stage-set --input BUILDS --output NEW_DIRECTORY --manifest
TAG_SOURCE_MANIFEST` copies only those twenty-four files into an owned staging tree,
verifies the result and rechecks the original input. Existing destinations are
never replaced. The protected producer and packager now use this handoff; their
schema-2 candidate and provenance records retain all six original build records.
The optional manifest argument is data from the caller's independently verified
source tag, not authority supplied by the helper receipts. Release receipts use
the canonical builder JSON encoding so public facts bind their original bytes.

Use the same assembly owner after each native build and notice collection:

```sh
node tools/jj-source/artifacts.mjs assemble-bundle --input /path/to/native-pair \
  --notices /path/to/collected-notices --output /path/to/builds/linux_arm64
```

The destination must be new and outside both inputs. Assembly verifies the pair
and its original-build attribution, copies the four known files, verifies the
result, and rechecks both inputs. It does not sign artifacts or resolve missing
attribution. Once all six target bundles are present, `verify-set` validates the
complete producer handoff.

Binary/receipt/notice archive assembly, original-to-final provenance binding and
native signature checks are wired into the packager and verifiers. The
runtime-pair checker allows other members only when the caller
already owns archive or installation inventory checks. Actual native execution
remains after all static checks and before the final CLI version smoke.

The [Homebrew companion installation change](https://github.com/openclaw/homebrew-tap/pull/59)
is landed. Its real macOS ARM64 archive installation, reinstall, linking and
native companion lookup were verified in an isolated Homebrew prefix using
development artifacts; published release signing and other platforms were not
established by that test.

Unresolved attribution and full platform acceptance
remain unfinished. Existing credential, signing
and publication gates are unchanged. Schema 1 remains supported for immutable
Go-only source tags without a native manifest.

## Collect observed build attribution

Run notice collection on the native build host, retaining its Cargo cache and
prepared source. Supply the builder's JSON report, fresh Cargo metadata for the
same source/target, and the exact native pair:

```sh
node tools/jj-source/notices.mjs --source /path/to/prepared-source \
  --build-report /path/to/build-report.json --metadata /path/to/cargo-metadata.json \
  --pair /path/to/native-pair --output /path/to/new-notice-directory
```

Compiler-artifact IDs select packages; Cargo metadata supplies their package
facts, not feature-selection evidence. The collector checks registry archives
against Cargo-generated lock-format-4 checksums and reads attribution bytes from
those archives, including nested notices and copyright files. Cached manifests
must match the archive. Prepared path packages remain bound to the verified
source tree before and after collection.

`notice-supplements.json` binds supplemental upstream files to the published
crate's repository, VCS commit and package path. Overview documents remain
explicitly distinct from full license texts. Missing material is reported in
both `attribution.json` and the text artifact; it is not replaced with an invented
copyright statement. The report binds the text's SHA-256 and size to the original
helper/source/target identity. `notice-artifacts.mjs` verifies that handoff without
needing the original Cargo cache; it retains unresolved entries rather than
interpreting byte consistency as complete attribution.
Historical notices carry their own source commit and each current package's
separate VCS binding. The verified ancestor MIT notice for `block2`, `objc2`, and
`objc2-encode` preserves known upstream copyright text, but is labeled historical
and does not clear their current attribution gaps.
The text artifact keeps every package/path reference while emitting identical
verbatim bytes once by SHA-256. This is an observed-build attribution inventory,
not an exact list of linked code or legal clearance. Native/system libraries and
unresolved upstream references still need review before a release notice is
finalized. Local paths from build reports do not enter the generated artifacts.
