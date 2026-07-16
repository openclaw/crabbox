# Release Engineering

Crabbox releases are local-produced, draft-first, and serialized. Building a
candidate, uploading a private draft, verifying it, publishing it, and updating
Homebrew are distinct gates. Approval for one gate does not authorize the next.
No tag push, repository dispatch, retry, or verifier run may collapse those
boundaries.

## Trust Anchors

> **v0.37.0 safety stop:** `release/records/v0.37.0.json` preserves tag object
> `d3e0da6a0355372bb3600ef9f2360983acd8272e` and source commit
> `99c82134c62e0da795b6165efa6affe7140c20dd`, but marks publication blocked.
> That tagged helper ad-hoc re-signs its embedded VMD before execution and
> therefore destroys the required Foundation Developer ID/notarization trust.
> Do not move the tag or weaken the verifier. The runtime fix requires a new
> signed release tag.

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
repository ruleset. One active no-bypass branch ruleset must cover the default
branch and require deletion/non-fast-forward protection, code-owner pull
requests with stale-review dismissal, last-push approval and at least one
approval, plus strict required status checks. A separate active no-bypass tag
ruleset must cover every `refs/tags/v*` release tag and prevent deletion and
updates. Before publication, an administrator must also freeze all
default-branch, tag, and Releases API writers for the non-atomic final gate.

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

Let that token-free verification process exit. Do not invoke draft creation
from the same wrapper or ancestor environment as candidate execution. Stop
here. Draft creation is the first remote mutation and needs its own
authorization. It performs protected static verification only; the final
positional argument is a deliberate exact-tag confirmation:

```sh
DRAFT_OUTPUT=$(scripts/create-release-draft.sh \
  "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT" \
  "$PWD/dist-release" "$TAG")
printf '%s\n' "$DRAFT_OUTPUT"
RELEASE_ID=$(printf '%s\n' "$DRAFT_OUTPUT" | \
  sed -n 's/^Created immutable private draft release_id=\([0-9][0-9]*\) tag=.*$/\1/p')
test -n "$RELEASE_ID"
```

Stop again. With separate authorization, dispatch the protected verifier for
that numeric draft. Capture the numeric ID of this exact run as
`DRAFT_VERIFIER_RUN_ID`, then require both native jobs to succeed:

```sh
gh workflow run release-assets.yml \
  --repo openclaw/crabbox \
  --ref main \
  -f release_id="$RELEASE_ID" \
  -f tag="$TAG" \
  -f tag_object="$TAG_OBJECT" \
  -f tag_commit="$TAG_COMMIT" \
  -f verifier_commit="$VERIFIER_COMMIT" \
  -f draft=true

: "${DRAFT_VERIFIER_RUN_ID:?set to the numeric ID of that exact draft run}"
gh run watch "$DRAFT_VERIFIER_RUN_ID" \
  --repo openclaw/crabbox --exit-status
```

Stop for the publication gate. Publication takes the exact successful draft
run ID and repeats the tag as its explicit confirmation:

```sh
CRABBOX_RELEASE_SERIALIZATION_CONFIRMED="$TAG:$RELEASE_ID" \
scripts/publish-release.sh \
  "$RELEASE_ID" "$TAG" "$TAG_OBJECT" "$TAG_COMMIT" \
  "$VERIFIER_COMMIT" "$DRAFT_VERIFIER_RUN_ID" "$TAG"
```

Stop again. Dispatch a new native run against the published state; do not reuse
the draft proof. Capture and wait for the exact new run:

```sh
gh workflow run release-assets.yml \
  --repo openclaw/crabbox \
  --ref main \
  -f release_id="$RELEASE_ID" \
  -f tag="$TAG" \
  -f tag_object="$TAG_OBJECT" \
  -f tag_commit="$TAG_COMMIT" \
  -f verifier_commit="$VERIFIER_COMMIT" \
  -f draft=false

: "${PUBLIC_VERIFIER_RUN_ID:?set to the numeric ID of that exact public run}"
gh run watch "$PUBLIC_VERIFIER_RUN_ID" \
  --repo openclaw/crabbox --exit-status
```

Only after that public run and a separate tap-update authorization, update the
formula with the four verified public archive hashes. Generate the only
accepted Ruby program with protected tooling (the four digest arguments are
Darwin amd64, Darwin arm64, Linux amd64, and Linux arm64, in that order):

```sh
node scripts/render-homebrew-formula.mjs \
  "$TAG" "$DARWIN_AMD64_SHA256" "$DARWIN_ARM64_SHA256" \
  "$LINUX_AMD64_SHA256" "$LINUX_ARM64_SHA256" >crabbox.rb
```

After the separately authorized tap commit, on both a clean
native Apple Silicon host and a clean native Intel host, download all eight
assets through their public URLs. While the GitHub token is still available,
download the two proof ZIPs from the exact public verifier run by immutable
artifact ID. The later verifier re-fetches their public metadata without a
token and requires each local ZIP to match GitHub's SHA-256 artifact digest:

```sh
PUBLIC_ASSETS=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-public-assets.XXXXXX")
while IFS= read -r asset; do
  curl --fail --location --retry 3 \
    --output "$PUBLIC_ASSETS/$asset" \
    "https://github.com/openclaw/crabbox/releases/download/$TAG/$asset"
done < <(scripts/release-config.sh assets "$TAG")

PUBLIC_PROOFS=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-public-proofs.XXXXXX")
PUBLIC_ARTIFACTS=$(mktemp "${TMPDIR:-/tmp}/crabbox-public-artifacts.XXXXXX")
gh api --method GET \
  "repos/openclaw/crabbox/actions/runs/$PUBLIC_VERIFIER_RUN_ID/artifacts?per_page=100" \
  >"$PUBLIC_ARTIFACTS"
for arch in arm64 x86_64; do
  ARTIFACT_ID=$(jq -er --arg name "verified-assets-$arch" '
    [.artifacts[] | select(.name == $name)] |
    if length == 1 then .[0].id else error("ambiguous proof artifact") end
  ' "$PUBLIC_ARTIFACTS")
  gh api --method GET \
    --header 'Accept: application/vnd.github+json' \
    "repos/openclaw/crabbox/actions/artifacts/$ARTIFACT_ID/zip" \
    >"$PUBLIC_PROOFS/verified-assets-$arch.zip"
done
```

Remove every GitHub, Actions, Homebrew, signing, notary, and secret-store
credential from the environment. Then run the downstream verifier in a new
credential-free shell:

On an ephemeral runner, materialize the public tap after removing credentials
and before starting the verifier:

```sh
HOMEBREW_NO_AUTO_UPDATE=1 brew tap openclaw/tap
```

The launcher captures absolute Homebrew, Node, and Go executable paths before
scrubbing the environment, then preserves only those tool directories plus the
macOS system paths in the child `PATH`.

Hosted native runners also place a credential-free `curl` retry wrapper in that
trusted tool directory. It keeps the frozen verifier's public GitHub API reads
bounded to 15 minutes while tolerating transient shared-runner 403/rate-limit
responses. Stdout responses are staged in a curl-owned temporary file and
emitted only after success, so retries cannot append to partial JSON. The wrapper
never adds an authorization header or skips a pre/postflight read.

```sh

scripts/verify-homebrew-release.sh \
  "$TAG" "$PUBLIC_ASSETS" \
  "$TAG_OBJECT" "$TAG_COMMIT" "$VERIFIER_COMMIT" \
  "$RELEASE_ID" "$PUBLIC_VERIFIER_RUN_ID" "$PUBLIC_PROOFS"
```

The last command first re-fetches the current public release, workflow run,
workflow, and artifact metadata without authentication. It binds the successful
post-publication run, both digest-pinned native proof ZIPs, their current release
and asset timestamps, and the supplied public bytes before it changes local
Homebrew state. It then performs `brew update`, a fresh install or reinstall,
`brew test`, exact installed-byte and architecture comparison, Foundation
signature and online notarization checks, exact version execution, and Apple
Silicon `vmd-info`, then repeats the unauthenticated public-record and proof
comparison to close the verification window. It does not update the tap. The
tap formula must be byte-for-byte output from the protected
`scripts/render-homebrew-formula.mjs`; any additional Ruby, interpolation, or
format drift is rejected before Homebrew evaluates it. Protected downstream
tooling must remain clean at the verifier commit before and after candidate
execution. The
lower-level `codesign-macos.sh`, `extract-release-notes.sh`,
`release-provenance.mjs`, `validate-release-publication.mjs`,
`verify-go-release-binary.mjs`, and `verify-macos-binary.sh` are internal to the
operator commands above and must not be reordered or invoked as substitute
gates.

## Serialized Gates

### 1. Create the private draft

After local package verification, obtain separate authorization to create one
private draft and upload only the frozen eight files. Capture the numeric
release ID and every asset ID from the response. Re-download into a fresh
directory and prove that the remote draft matches the local record exactly.

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

Publication requires a new explicit gate after both native draft jobs succeed.
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
per key, so the serialized operator dispatches exactly once; different releases
do not cancel one another. This cannot lock direct Releases API edits from
another token or administrator. GitHub's
documented Update-a-release endpoint does not provide a conditional `If-Match`
publication operation, so the final GET plus PATCH is not an atomic
compare-and-swap. Fail closed unless the administrative freeze above is active,
then acknowledge the exact draft with
`CRABBOX_RELEASE_SERIALIZATION_CONFIRMED=vX.Y.Z:RELEASE_ID`. The publisher
prepares its request body first, repeats the immutable numeric-ID draft read and
comparison immediately before the sole PATCH, and repeats all protected-source
and public-record checks afterward. A detected post-PATCH race is a publication
incident to report; it is not permission to delete, rewrite, or republish.

Publish with one draft-state transition. Do not rebuild, re-upload, rename,
replace, or delete assets; do not edit notes; do not update Homebrew in the same
operation.

### 4. Verify the public release

Re-fetch the public release by its numeric ID and re-download every asset by its
captured immutable asset ID. Repeat metadata, notes, inventory, checksum,
provenance, native architecture, signature, online notarization, and clean
candidate-execution checks. The successful public verifier run must be newer
than the publication and every release or asset update.

### 5. Update and prove Homebrew

Homebrew mutation needs a final separate authorization and a successful public
verifier proof. Re-download the frozen public assets immediately before the tap
change. Generate four explicit, non-fallthrough formula routes for Darwin
arm64, Darwin amd64, Linux arm64, and Linux amd64; every URL and SHA-256 must
match the public record. Apple Silicon installs the helper; other targets do
not.

On clean native Apple Silicon and Intel hosts, remove release/API credentials
before the first formula evaluation, then run the downstream verifier shown
above. It proves the installed files byte-match their selected archive members,
reports the expected version and architecture, and repeats the Foundation
authority, Team ID, identifier, hardened-runtime, timestamp, and online
notarization checks. Apple Silicon also runs the helper's non-mutating info path.
This bounded verifier is the installed-binary smoke; it does not create a lease.
Only both native proofs complete the Homebrew gate.

The Homebrew workflow itself runs from the current protected `main` commit,
while its verification checkout remains pinned to the release record's
protected verifier commit. This keeps published provenance immutable while
allowing a protected workflow-only repair to restore the downstream proof.

## Cancellation And Recovery

Cancellation stops all publication and tap actions immediately. Inspect the
workflow, exact draft/public release ID and state, uploaded asset IDs, and
Homebrew tap commit read-only. Record whether anything escaped, but make no
corrective mutation under the cancelled gate.

Never delete a partial draft or release based on its tag, body, or incomplete
inventory. Never overwrite assets, rewrite or recreate the signed tag,
redispatch a release, publish, or update Homebrew to "clean up" a cancelled
attempt. Preserve the evidence and resume only after a new serialized gate
explicitly authorizes the next mutation.

After the public release, public verifier, and both native Homebrew install
proofs succeed, add the next patch `Unreleased` section to `CHANGELOG.md`,
commit and push it, wait for exact-head CI, pull with `--ff-only`, and leave
`main` clean and synchronized.
