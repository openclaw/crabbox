# Release Engineering

Crabbox releases are local-produced, draft-first, and explicitly authorized. One explicit
full release/publish request authorizes the complete normal sequence:
preparation, tagging, build and signing, private draft creation and upload,
native dispatch and proof, publication, ordinary Homebrew update, independent
public-download/native verification, public Go installation and installed-Homebrew
smokes, and closeout, without renewed chat approval at each stage. Narrow
requests stay narrow: a request to build or verify a candidate does not authorize
publication.

The original explicit release request is the authorization. GitHub events alone
are not authorization: no tag push, repository dispatch, retry, or verifier run
grants permission to release. Before publication, advance sequentially only as
each technical gate passes. Post-publication smokes are independent of tap
dispatch. Identity binding, credential isolation, immutability, exact frozen
inputs, immediate publication readbacks, and cancellation boundaries remain
mandatory. No additional approval-ruleset configuration or administrative
writer freeze is required by the release contract.

Maintainers keep user-visible fixes and features in `CHANGELOG.md` under
`Unreleased` as work lands, with full PR links and contributor thanks. Release
preparation finalizes those accumulated entries into the versioned, dated release
section. Later work starts a new `Unreleased` section on `main`; it does not change
the frozen tagged source or published release notes.

## Trust Anchors

Treat a pushed version tag as public source publication even when no GitHub
Release exists. Go module proxies may already have cached its source, and the
public checksum database preserves that content identity. Never move a version
tag to include newer code or rely on deleting a tag or clearing a local cache to
remove the public version. Complete the exact tagged release or publish the
changes under a new version, preserving the original changelog section. See
[Go's publishing guidance](https://go.dev/doc/modules/publishing) and the
[module mirror FAQ](https://proxy.golang.org/#faq).

> **v0.37.0 safety stop:** `release/records/v0.37.0.json` preserves tag object
> `d3e0da6a0355372bb3600ef9f2360983acd8272e` and source commit
> `99c82134c62e0da795b6165efa6affe7140c20dd`, but marks publication blocked.
> That tagged helper ad-hoc re-signs its embedded VMD before execution and
> therefore destroys the required Foundation Developer ID/notarization trust.
> Do not move the tag or weaken the verifier. The runtime fix requires a new
> signed release tag.

> **Signer registration:** GitHub evaluates SSH tag-signature verification at
> push time only. Register the tagging machine's SSH key as a GitHub account
> signing key *before* creating the tag, and confirm the pushed tag reports
> `verification.verified == true` before producing any candidate. A tag pushed
> under an unregistered key is permanently unverified; recovering requires
> replacing the tag under temporarily lifted tag-ruleset enforcement and
> rebinding the release record's `tagObject`.

A release begins with an annotated signed `vMAJOR.MINOR.PATCH` tag and two
captured immutable Git identities:

- the tag-object ID;
- the peeled source-commit ID.

The signature must verify against the public signer policy checked into the
repository. Fetch the remote tag into a private ref, compare its exact object
and commit IDs with the captured values, and require the source commit to be an
ancestor of the freshly fetched protected `main`.

Release hardening can land after an already valid tag. In that case, preserve
the signed tag and its source commit exactly; never move, recreate, or force-push
the tag to include verifier changes. Record a separate protected verifier
commit. The verifier runs from that exact default-branch workflow commit while
the candidate is built from the captured tag commit in a separate tree.

Protected release workflow and verifier files are trusted code. Check them out
at the workflow SHA with persisted credentials disabled. Candidate files do not
choose the release configuration, signer allowlist, inventory, notes extractor,
verifier, publisher, or Homebrew updater.

The publishing checkout must have `HEAD` exactly equal to the protected verifier
commit, and every release-policy or executable tooling file must match that
commit with no staged, unstaged, or untracked replacement. Fetch every detailed
repository ruleset. Pull-request approval policy is independent of publication:
no particular approval ruleset or release-team bypass is required, and existing
GitHub merge protections still apply. An active no-bypass branch ruleset must enforce
deletion and non-fast-forward protection. The default branch must also be
covered by the no-bypass OpenClaw organization workflow
`.github/workflows/crabbox-release-check.yml` from
`openclaw/release-workflows` repository ID `1304559357` at
`refs/heads/main`. That protected external workflow owns the credential-free
macOS release snapshot check, so a Crabbox pull request cannot redefine the
check that gates its own merge. A separate active no-bypass tag ruleset must
cover every `refs/tags/v*` release tag and prevent deletion and updates.

GitHub omits ruleset bypass actors from the ordinary workflow token. Configure
`CRABBOX_RULESET_READ_TOKEN` as a fine-grained repository secret scoped only to
this repository with Administration read-only permission. It is exposed only
to the protected guard's ruleset-read step; no asset inspection, native
verification, or candidate-execution step receives it.

## Local Candidate Production

Ordinary snapshots, CI, Linux builds, and Windows builds are credential-free.
Production macOS packaging runs locally on a trusted Mac through the shared
managed-keychain release wrapper. Signing keys and notary credentials remain in
the local keychain/approved secret store and never enter GitHub Actions or Git.

Before GoReleaser runs, the credential-free producer calls
`scripts/verify-go-install.sh` with the actual release tag and peeled source
commit. That gate constructs a complete read-only local module proxy from the
exact commit and verifies a cold, version-suffixed `go install` outside the
checkout. It must pass before candidate production begins.

Run the credential-free producer first and capture its printed manifest digest:

```sh
scripts/build-release-candidate.sh \
  vX.Y.Z <tag-object> <source-commit> dist-release-unsigned
```

The producer atomically writes a private `.components/candidate-manifest.json`
beside the unsigned inputs. It binds the signed tag identity, protected verifier
commit and release configuration, exact SHA-256, size, and mode of all six
archives and the raw VMD, plus the actual Go, GoReleaser, Swift, Xcode, macOS,
and architecture facts. Treat the printed SHA-256 as a separate handoff value;
do not re-read or infer it from a replaceable candidate directory.

Pass that exact digest as the required fourth argument to the local signing
wrapper. The packager stages the complete candidate into a private directory,
recomputes every manifest-bound fact before it touches the signing key, and
fails if the explicit digest differs:
The operator sequence below additionally computes the package script digest
from the protected verifier commit and makes the managed wrapper execute a
literal pre-secret digest gate before the repository script receives signing
or notary credentials.

Sign each thin macOS executable with its fixed identifier and this exact
authority:

```text
Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)
```

The signed set includes `crabbox` for `darwin/arm64` and `darwin/amd64`,
`crabbox-apple-vm-helper` for `darwin/arm64`, and its embedded
`crabbox-apple-vm-vmd` payload. The VMD uses identifier
`org.openclaw.crabbox.apple-vm-vmd` and the exact tracked virtualization/network
entitlements. Each executable must have Team ID `FWJYW4S8P8`, the expected
designated requirement and architecture, hardened runtime, and a secure
timestamp. Submit each raw binary with `notarytool --wait`, require an
`Accepted` result and distinct valid submission ID, then perform the online
raw-binary check before creating the archive:

The notary profile must live in the same managed, passwordless release
keychain as the Foundation signing identity. The signer passes that keychain
explicitly so headless release hosts never fall back to a locked login
keychain.

```sh
codesign --verify --strict --check-notarization -R=notarized <binary>
```

Raw command-line binaries cannot be stapled. `stapler` and an `spctl` result are
not substitutes for the online `codesign` check. The Apple VM helper's embedded
VMD is also an executable trust path: packaging must freeze and verify that
payload, and runtime extraction must not replace an accepted Developer ID trust
decision with an ad-hoc signature. Official packaging compiles the helper with
`vmdembed,vmdrelease`; a bare `vmdembed` remains a credential-free development
build and is not publishable. Protected native verification exports the exact
embedded bytes, matches their provenance digest, and independently checks their
signature, entitlements, hardened runtime, timestamp, and online notarization.

The signing wrapper never runs candidate code while its managed keychain or
notary profile is available. It signs the token-free producer outputs, embeds
the accepted VMD, compiles without release credentials, and stops after static
packaging proof. Run `scripts/verify-release.sh` only after the signing wrapper
has returned and removed its credentials; draft creation repeats that clean
verification before any GitHub token is used.

## Immutable Release Record

For version `X.Y.Z`, the uploaded GitHub asset set is exactly these eight files:

| Asset | Exact archive members or purpose |
| --- | --- |
| `crabbox_X.Y.Z_darwin_amd64.tar.gz` | `crabbox` |
| `crabbox_X.Y.Z_darwin_arm64.tar.gz` | `crabbox`, `crabbox-apple-vm-helper` |
| `crabbox_X.Y.Z_linux_amd64.tar.gz` | `crabbox` |
| `crabbox_X.Y.Z_linux_arm64.tar.gz` | `crabbox` |
| `crabbox_X.Y.Z_windows_amd64.zip` | `crabbox.exe` |
| `crabbox_X.Y.Z_windows_arm64.zip` | `crabbox.exe` |
| `checksums.txt` | Canonical SHA-256 records for the six platform archives and `provenance.json` |
| `provenance.json` | Schema-pinned source, toolchain, signing, notarization, archive, and checksum provenance |

GitHub's generated source links are not uploaded assets and do not change the
count. Reject missing, duplicate, renamed, zero-byte, or extra uploaded assets.
Archive member names and counts are exact; no implicit documentation files or
unlisted executables are allowed.

`provenance.json` binds the repository, version, signed tag-object ID, peeled
source commit, protected verifier commit, exact candidate-manifest digest and
seven producer inputs, separate producer and packager toolchain facts, macOS
identifiers, Team ID and authority, native architectures, notarization
submissions, archive members, and the name, size, and SHA-256 of each payload it
describes. Its own
name, size, digest, upload timestamp, and unique GitHub asset ID are captured in
the immutable draft proof after upload. `checksums.txt` and provenance must not
form a self-referential digest cycle.

The GitHub record is exactly one draft selected by numeric release ID, with:

- tag and title `vX.Y.Z`;
- `draft=true` and `prerelease=false`;
- body byte-for-byte equal to the canonical `CHANGELOG.md` section extracted
  from the tagged source;
- exactly the eight assets above, each with a unique numeric ID, positive size,
  and matching SHA-256 digest.

Freeze that record before verification. Later gates compare the release ID,
state, the API's non-authoritative `target_commitish`, title, notes digest and bytes, asset IDs, names, sizes, digests,
and update timestamps. A mismatch blocks progress; it never triggers deletion
or replacement.

GitHub ignores `target_commitish` when the signed tag already exists. It is
therefore frozen only as release-record metadata, never used as source identity.
The verified annotated tag object and its peeled commit are the authoritative
source binding.

## Operator Command Sequence

Run from a clean Crabbox repository. Replace `vX.Y.Z` only with a new signed tag
whose protected record says `ready`; `v0.37.0` deliberately fails the first
publishability check. Preserve the captured values for every later command:

```sh
set -euo pipefail
TAG=vX.Y.Z
TAG_OBJECT=$(git rev-parse "refs/tags/$TAG")
TAG_COMMIT=$(git rev-parse "refs/tags/$TAG^{commit}")
VERIFIER_COMMIT=$(git rev-parse HEAD)
WORKFLOW_COMMIT=$VERIFIER_COMMIT

DEFAULT_BRANCH=main \
RELEASE_TAG="$TAG" \
EXPECTED_TAG_OBJECT="$TAG_OBJECT" \
EXPECTED_TAG_COMMIT="$TAG_COMMIT" \
TRUSTED_HEAD="$VERIFIER_COMMIT" \
REQUIRE_PUBLISHABLE=1 \
  scripts/verify-release-source.sh
```

The credential-free producer may run without the signing wrapper. Production
packaging must run on Apple Silicon through the shared managed-keychain wrapper,
with its approved local codesign/notary configuration already loaded. The
wrapper returns and removes its credentials before candidate execution:

```sh
BUILD_OUTPUT=$(scripts/build-release-candidate.sh \
  "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" "$PWD/dist-release-unsigned"
)
printf '%s\n' "$BUILD_OUTPUT"
CANDIDATE_MANIFEST_SHA256=$(printf '%s\n' "$BUILD_OUTPUT" | \
  sed -n 's/^Candidate manifest SHA-256: //p')
test "${#CANDIDATE_MANIFEST_SHA256}" -eq 64

test "$(git remote get-url origin)" = https://github.com/openclaw/crabbox
test "$(git ls-remote origin refs/heads/main | awk '{print $1}')" = "$VERIFIER_COMMIT"
PACKAGE_SCRIPT_SHA256=$(git --no-pager show \
  "${VERIFIER_COMMIT}:scripts/package-release.sh" | shasum -a 256 | awk '{print $1}')
test "$(shasum -a 256 scripts/package-release.sh | awk '{print $1}')" = \
  "$PACKAGE_SCRIPT_SHA256"

../agent-scripts/skills/release-mac-app/scripts/mac-release \
  codesign-run --with-package-secrets -- \
  /bin/bash -c '
    set -euo pipefail
    root=$1
    verifier_commit=$2
    script=$3
    expected=$4
    shift 4
    git=(/usr/bin/git -c core.fsmonitor=false -c core.untrackedCache=false)
    [[ "$("${git[@]}" -C "$root" rev-parse HEAD)" == "$verifier_commit" ]]
    [[ -z "$("${git[@]}" -C "$root" status --porcelain --untracked-files=all)" ]]
    [[ "$("${git[@]}" -C "$root" remote get-url origin)" == \
      https://github.com/openclaw/crabbox ]]
    [[ "$("${git[@]}" ls-remote https://github.com/openclaw/crabbox \
      refs/heads/main | /usr/bin/awk "{print \$1}")" == "$verifier_commit" ]]
    actual=$(/usr/bin/shasum -a 256 "$script")
    actual=${actual%% *}
    [[ "$actual" == "$expected" ]]
    exec /bin/bash "$script" "$@"
  ' crabbox-protected-package \
  "$PWD" "$VERIFIER_COMMIT" \
  "$PWD/scripts/package-release.sh" "$PACKAGE_SCRIPT_SHA256" \
  "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" \
  "$CANDIDATE_MANIFEST_SHA256" \
  "$PWD/dist-release-unsigned" "$PWD/dist-release"

VERIFY_HOME=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-release-verify.XXXXXX")
env -i \
  CRABBOX_VERIFY_EXEC_ARCH="$(uname -m)" \
  CRABBOX_VERIFY_MODE=execute \
  HOME="$VERIFY_HOME" LANG=C LC_ALL=C PATH="$PATH" TMPDIR="$VERIFY_HOME" \
  scripts/verify-release.sh \
    "$TAG" "$PWD/dist-release" \
    "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT"
```

Let that token-free verification process exit successfully. Do not invoke draft
creation from the same wrapper or ancestor environment as candidate execution.
Continue under the original full release authorization. Draft creation is the
first remote mutation and performs protected static verification only; the
final positional argument is a deliberate exact-tag confirmation:

```sh
DRAFT_OUTPUT=$(scripts/create-release-draft.sh \
  "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT" \
  "$PWD/dist-release" "$TAG")
printf '%s\n' "$DRAFT_OUTPUT"
RELEASE_ID=$(printf '%s\n' "$DRAFT_OUTPUT" | \
  sed -n 's/^Created immutable private draft release_id=\([0-9][0-9]*\) tag=.*$/\1/p')
test -n "$RELEASE_ID"
```

After draft creation succeeds, dispatch the protected verifier for that numeric
draft under the same release authorization. Capture the numeric ID of this
exact run as `DRAFT_VERIFIER_RUN_ID`, then require both native jobs to succeed:

```sh
gh workflow run release-assets.yml \
  --repo openclaw/crabbox \
  --ref main \
  -f release_id="$RELEASE_ID" \
  -f tag="$TAG" \
  -f tag_object="$TAG_OBJECT" \
  -f tag_commit="$TAG_COMMIT" \
  -f verifier_commit="$VERIFIER_COMMIT" \
  -f workflow_commit="$WORKFLOW_COMMIT" \
  -f draft=true

: "${DRAFT_VERIFIER_RUN_ID:?set to the numeric ID of that exact draft run}"
gh run watch "$DRAFT_VERIFIER_RUN_ID" \
  --repo openclaw/crabbox --exit-status
```

After the native draft proof succeeds and the publication checks below pass,
continue to publication. Publication takes the exact successful draft run ID,
its protected workflow commit, and repeats the tag as its explicit
confirmation. The seven-argument compatibility form is only for historical
runs where `WORKFLOW_COMMIT` exactly equals `VERIFIER_COMMIT`:

```sh
scripts/publish-release.sh \
  "$RELEASE_ID" "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" \
  "$VERIFIER_COMMIT" "$WORKFLOW_COMMIT" "$DRAFT_VERIFIER_RUN_ID" "$TAG"
```

Publication establishes eligibility for the ordinary tap updater. The publisher
has finished at this point; Homebrew is an explicit, separately retryable
operator step under the original release authorization, not automatic dispatch
from Crabbox. Do not consult or await public native or public Go smoke results
before this handoff.

Use only `TAG` and normal tap dispatch authorization from any trusted operator
checkout or session; no retained build directory or source/provenance identities
are needed. This reads the published metadata and selects the four canonical
archive digests. The ordinary updater independently downloads the archives and
compares their hashes. Full-artifact validation belongs to the separate
installed-Homebrew smoke, not this handoff.

<!-- ordinary-tap-handoff -->

```sh
(
  set -euo pipefail
  : "${TAG:?}"
  [[ "$TAG" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] || exit 1
  ASSETS_JSON=$(curl --disable --fail --silent --show-error --location --retry 3 \
    "https://api.github.com/repos/openclaw/crabbox/releases/tags/$TAG" |
    jq -ce --arg tag "$TAG" '
      (if .tag_name == $tag and .draft == false and .prerelease == false and
         .immutable == true and (.assets | type) == "array"
      then .assets else error("expected immutable public release") end) as $assets |
      ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"] |
      map(. as $target | "crabbox_\($tag[1:])_\($target).tar.gz" as $name |
        [$assets[] | select(.name == $name)] as $matches |
        if ($matches | length) == 1 and $matches[0].state == "uploaded" and
           ($matches[0].digest | type) == "string" and
           ($matches[0].digest | test("^sha256:[0-9a-f]{64}\\z"))
        then {key: $target, value: {name: $name, sha256: $matches[0].digest[7:]}}
        else error("missing, duplicate, or invalid archive: " + $name) end) |
      from_entries
    ')
  gh workflow run update-formula.yml \
    --repo openclaw/homebrew-tap --ref main \
    -f formula=crabbox -f tag="$TAG" -f repository=openclaw/crabbox \
    -f assets="$ASSETS_JSON"
)
```

The tap's existing updater downloads and hashes all four named archives and
preserves maintained formula content. Use the ordinary `assets` contract, not
`verified-hashes-v1`: no source-object/request-ID ledger or provenance commit is
needed, and an already-current tap is success. Observe the dispatched tap run
separately; dispatch success alone does not prove the update finished.

If the tap update fails, retry this same handoff after resolving the failure.
It also accepts an already-completed update. Never rebuild, repackage, create a
draft, or invoke the publisher to retry Homebrew. Retrying needs only `TAG` and
normal tap dispatch authorization, even from a different trusted operator
session. Generic tap reconciliation remains a valid independent fallback for
public stable releases.
A checksum or identity mismatch is an incident to investigate, not a reason to
replace immutable release assets.

Public native and public Go installation checks below are independent channel
smokes. They report health, not a second readiness or approval gate for the
published release or tap dispatch.

For the independent public native smoke, dispatch a new run against the published
state; it produces no downstream approval artifacts. `VERIFIER_COMMIT` remains
the immutable candidate-provenance commit. Bind the new run separately to the current protected
workflow commit, which must descend from that verifier, then capture and wait
for the exact run:

```sh
WORKFLOW_COMMIT=$(git rev-parse HEAD)
test "$(git ls-remote origin refs/heads/main | awk '{print $1}')" = "$WORKFLOW_COMMIT"
git merge-base --is-ancestor "$VERIFIER_COMMIT" "$WORKFLOW_COMMIT"
git merge-base --is-ancestor "$TAG_COMMIT" "$VERIFIER_COMMIT"

gh workflow run release-assets.yml \
  --repo openclaw/crabbox \
  --ref main \
  -f release_id="$RELEASE_ID" \
  -f tag="$TAG" \
  -f tag_object="$TAG_OBJECT" \
  -f tag_commit="$TAG_COMMIT" \
  -f verifier_commit="$VERIFIER_COMMIT" \
  -f workflow_commit="$WORKFLOW_COMMIT" \
  -f draft=false

: "${PUBLIC_VERIFIER_RUN_ID:?set to the numeric ID of that exact public run}"
gh run watch "$PUBLIC_VERIFIER_RUN_ID" \
  --repo openclaw/crabbox --exit-status
```

Independently smoke-test the public source-install channel from the public Go
module proxy, not from a checkout, local proxy, or direct VCS fallback. Use fresh state and
require the exact public tag, fork dependency, replacement-free build metadata,
version, and help surfaces:

```sh
PUBLIC_GO_INSTALL=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-public-go-install.XXXXXX")
mkdir -m 700 \
  "$PUBLIC_GO_INSTALL/home" "$PUBLIC_GO_INSTALL/gopath" \
  "$PUBLIC_GO_INSTALL/gomodcache" "$PUBLIC_GO_INSTALL/gocache" \
  "$PUBLIC_GO_INSTALL/bin" "$PUBLIC_GO_INSTALL/tmp" "$PUBLIC_GO_INSTALL/work"
(
  cd "$PUBLIC_GO_INSTALL/work"
  env -i \
    GOBIN="$PUBLIC_GO_INSTALL/bin" GOCACHE="$PUBLIC_GO_INSTALL/gocache" \
    GOENV=off GOMODCACHE="$PUBLIC_GO_INSTALL/gomodcache" \
    GOPATH="$PUBLIC_GO_INSTALL/gopath" GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org GOTOOLCHAIN=local GOWORK=off \
    HOME="$PUBLIC_GO_INSTALL/home" PATH="$PATH" TMPDIR="$PUBLIC_GO_INSTALL/tmp" \
    go install "github.com/openclaw/crabbox/cmd/crabbox@$TAG"
)
GOTOOLCHAIN=local go version -m -json "$PUBLIC_GO_INSTALL/bin/crabbox" >"$PUBLIC_GO_INSTALL/build.json"
jq -e --arg version "$TAG" \
  --arg forkVersion v6.0.3-0.20260817142523-966654abed4a '
  .Path == "github.com/openclaw/crabbox/cmd/crabbox" and
  .Main.Path == "github.com/openclaw/crabbox" and
  .Main.Version == $version and .Main.Replace == null and
  ([.Deps[] | select(
    .Path == "github.com/steipete/jsonschema/v6" and
    .Version == $forkVersion and .Replace == null
  )] | length == 1) and
  ([.Deps[] | select(.Replace != null)] | length == 0) and
  ([.Deps[] | select(.Path == "github.com/santhosh-tekuri/jsonschema/v6")] | length == 0)
' "$PUBLIC_GO_INSTALL/build.json"
test "$(cd "$PUBLIC_GO_INSTALL/work" && "$PUBLIC_GO_INSTALL/bin/crabbox" --version)" = "${TAG#v}"
(cd "$PUBLIC_GO_INSTALL/work" && "$PUBLIC_GO_INSTALL/bin/crabbox" --help >/dev/null 2>&1)
(cd "$PUBLIC_GO_INSTALL/work" && "$PUBLIC_GO_INSTALL/bin/crabbox" run --help >/dev/null 2>&1)
```

After the tap update, smoke-test installation on both native Apple Silicon and
native Intel. Download the fixed canonical public inventory (never URLs taken
from formula text or arbitrary release metadata):

```sh
[[ "$TAG" =~ ^v[0-9]+[.][0-9]+[.][0-9]+$ ]] || exit 1
PUBLIC_ASSETS=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-public-assets.XXXXXX")
while IFS= read -r asset; do
  curl --disable --fail --location --retry 3 \
    --output "$PUBLIC_ASSETS/$asset" \
    "https://github.com/openclaw/crabbox/releases/download/$TAG/$asset"
done < <(scripts/release-config.sh assets "$TAG")
```

Remove every GitHub, Actions, Homebrew, signing, notary, and secret-store
credential from the environment. Then run the downstream verifier in a new
credential-free shell.

The launcher captures absolute Homebrew, Node, and Go executable paths before
scrubbing the environment, then preserves only those tool directories plus the
macOS system paths in the child `PATH`. Tap setup and formula evaluation run
inside that child with a fresh `HOME` and cache.

```sh
HOMEBREW_TOOLING_COMMIT=$(git rev-parse HEAD)
case "$(git remote get-url origin)" in
  https://github.com/openclaw/crabbox | https://github.com/openclaw/crabbox.git) ;;
  *) false ;;
esac
REMOTE_MAIN=$(git ls-remote https://github.com/openclaw/crabbox \
  refs/heads/main | awk '{print $1}')
git -c fetch.writeCommitGraph=false fetch --quiet --no-tags \
  https://github.com/openclaw/crabbox "$REMOTE_MAIN"
git merge-base --is-ancestor "$HOMEBREW_TOOLING_COMMIT" "$REMOTE_MAIN"
git merge-base --is-ancestor "$VERIFIER_COMMIT" "$HOMEBREW_TOOLING_COMMIT"
git merge-base --is-ancestor "$TAG_COMMIT" "$VERIFIER_COMMIT"
CRABBOX_VERIFY_TOOLING_COMMIT="$HOMEBREW_TOOLING_COMMIT" \
scripts/verify-homebrew-release.sh \
  "$TAG" "$PUBLIC_ASSETS" \
  "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT" \
  "$RELEASE_ID"
```

The six-argument verifier first fetches the immutable public release by numeric
ID without authentication and compares its exact notes, inventory, asset IDs,
sizes, and digests with the supplied bytes. Protected static `verify-release.sh`
validation precedes Homebrew. There is no public verifier run ID, proof ZIP,
witness, or post-candidate API comparison.

Tap maintainers own executable formulae. `brew info --json=v2 --formula
openclaw/tap/crabbox` evaluates that trusted code only in the credential-free
environment; its structured metadata must report the exact formula name, full
name, tap, stable version, selected native URL, and checksum. Harmless maintained
formula changes and interpolation are accepted. This metadata check is not a
Ruby sandbox. All-four URL/hash maintenance belongs to the ordinary tap updater.

The verifier performs a fresh public fetch, install or reinstall, exact
archive-to-install byte comparison, native architecture and Foundation signature
and online notarization checks, `brew test`, exact version execution, and Apple
Silicon helper `vmd-info`. The helper must be present only on arm64 and report
the provenance-bound VMD trust marker. No raw candidate execution is needed
before this installed-binary smoke. Protected downstream tooling remains clean
at the explicit tooling commit, descending from the immutable provenance
verifier, before and after installation.

The hosted `Verify Homebrew Release` workflow (`verify-homebrew.yml`) accepts
`tag`, `tag_object`, `source_commit`, `verifier_commit`, and `release_id`. It uses
protected-default tooling and anonymous fixed-repository downloads on both
native architectures, then runs the same six-argument verifier. It never writes
to the tap or supplies candidate-accessible credentials. Rerun this installation
smoke independently after a smoke failure; do not re-enter publication.

## Serialized Gates

These gates order this operator's work; they do not establish a global writer
lock or require an administrative freeze.

### 1. Create the private draft

After local package verification succeeds, continue under the original release
authorization to create one private draft and upload only the frozen eight
files. Capture the numeric release ID and every asset ID from the response.
Re-download into a fresh directory and prove that the remote draft matches the
local record exactly.

Static Go build-info inspection uses the installed local toolchain without
switching or downloading one; the binary's recorded toolchain and build settings
must still match the release contract. Draft creation removes its fresh private
verification home, making read-only cache directories writable without following
symlinks or changing operator caches. It prints success only after cleanup
succeeds. Cleanup errors retain diagnostics and the temporary path, fail the
command, and preserve any earlier verification, creation, or readback exit code.
A cleanup failure does not undo draft creation; inspect the existing record
read-only before considering any further action.

### 2. Verify the draft natively

Dispatch the protected-default verifier with the tag, tag-object ID, source
commit, numeric draft ID, and protected workflow commit. Both native static and
both dependent native execution jobs are required:

- native Apple Silicon verifies the arm64 CLI and helper;
- native Intel verifies the amd64 CLI.

Use the release-capable API token only in a dedicated no-checkout job that reads
the exact numeric release, downloads the captured asset IDs without inspecting
or executing them, and freezes one opaque Actions artifact. Native static and
execution jobs have read-only repository permissions, download that immutable
artifact, and receive no release, Homebrew, signing, notary, runtime, or OIDC
credentials. The verifier fails if any prohibited credential remains.

Each job verifies the frozen inventory, checksums, provenance, exact archive
shape, Go build information, source revision and clean-build flag, thin native
architecture, Foundation signature, hardened runtime, secure timestamp, and
online notarization. Protected tooling statically locates the one provenance-
matched embedded VMD Mach-O without executing the helper, then independently
verifies it. Static jobs freeze the two immutable proof artifacts first.
Candidate-controlled code runs only in dependent clean jobs: the arm64 helper
reports release trust policy version 1, and native `crabbox --version` runs
last. Overall workflow success binds execution to the already frozen proofs.

### 3. Publish explicitly

Continue to the publication checks after both native draft jobs succeed.
Immediately before mutation, fetch the remote tag, protected workflow head,
draft metadata, notes, and every asset record again. Require byte-for-byte
equality with the frozen proof and require the successful native markers to
refer to that exact state.

Enable organization-enforced release immutability for this repository before
the publication gate. The publisher checks the live setting before its sole
PATCH, and the publication response plus every public verifier must report
`immutable=true`. A repository-only or disabled setting blocks publication
before mutation.

The protected native verifier uses a non-cancelling concurrency key scoped to
the immutable numeric release. GitHub Actions retains at most one pending run
per key, so the operator dispatches exactly once; different releases
do not cancel one another. This cannot lock direct Releases API edits from
another token or administrator. GitHub's
documented Update-a-release endpoint does not provide a conditional `If-Match`
publication operation, so the final GET plus PATCH is not an atomic
compare-and-swap. No administrative freeze or serialization attestation is
required. Another writer can still race the final read and publication; this
workflow does not claim exclusive-writer guarantees.

The publisher prepares its request body first, repeats the immutable numeric-ID
draft read and comparison immediately before the sole PATCH, and repeats all
protected-source and public-record checks afterward. A detected post-PATCH race is a publication
incident to report; it is not permission to delete, rewrite, or republish.

Publish with one draft-state transition. Do not rebuild, re-upload, rename,
replace, or delete assets; do not edit notes; do not update Homebrew in the same
operation.

### 4. Update and prove Homebrew

Publication establishes eligibility. Dispatch the ordinary tap update using the
TAG-only four-target handoff above, without waiting for either public native
or Go smoke. The updater maintains all four URL/hash routes; retry only this
step after an update failure. An already-current update succeeds. After the tap
finishes, run the independent installed-Homebrew smoke on both native macOS
architectures. Neither smoke failure requires rebuilding or republishing.

### 5. Independent public channel smokes

Dispatch `release-assets.yml` with `draft=false` for anonymous downloads and
native verification. Static verification still precedes isolated execution,
using the same opaque `release-input` transport; public mode emits no
`verified-assets-*` approval artifacts. Draft mode still freezes exactly
`release-input`, `verified-assets-arm64`, and `verified-assets-x86_64` for the
publisher, with unchanged proof schemas.

Run the public Go installation procedure independently with fresh proxy-only
state. It proves the actual published tag is remotely resolvable, unlike the
preproduction hermetic fixture. Do not substitute a checkout, `replace`,
pseudo-version, local proxy, or `,direct` fallback. Record channel smoke failures
and retry the affected check, not production or publication.

## Cancellation And Recovery

Cancellation stops this operator’s remaining publication and tap actions. It
cannot stop independent tap reconciliation of an already-public release; a
global stop requires separate administrative intervention. Inspect the
workflow, exact draft/public release ID and state, uploaded asset IDs, and
Homebrew tap commit read-only. Record whether anything escaped, but make no
corrective mutation under the cancelled gate.

Never delete a partial draft or release based on its tag, body, or incomplete
inventory. Never overwrite assets, rewrite or recreate the signed tag,
redispatch a release, publish, or update Homebrew to "clean up" a cancelled
attempt. Preserve the evidence. Explicit cancellation requires renewed direction
authorizing the next mutation before resuming the serialized sequence.

Before publication, a failed gate or uncertain state also stops release writes. Resolve the blocker and
re-establish the exact frozen state and required proofs before continuing.
Normal continuation after successful checks uses the original release
authorization; it does not require another chat approval.

For closeout, record the published release, tap result, and independent smoke
results, including outstanding failures. Verify the published notes match the
finalized release section in the changelog. Keep later user-visible changes in
`Unreleased` on `main`; never change the frozen tagged source, published release
notes, or release versions as part of downstream retries. Leave the intended
checkout clean and synchronized after authorized release commits are complete.
