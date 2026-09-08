# Image Bake Runbook

Read this when you:

- bake a new AWS image (AMI) for Crabbox leases;
- promote or roll back the default AWS image;
- prepare a desktop or browser image for UI QA;
- decide whether some state belongs in the image or in a warm lease.

This runbook is for trusted operators. Image commands require coordinator admin
auth (`configuredAdminCoordinator`) and create provider-side artifacts that cost
money until you clean them up.

## How image selection works

Crabbox boots a lease from a provider image. For AWS, `crabbox image promote`
registers an AMI as the default for a given **target**, **architecture**, and
**region**, so ordinary brokered leases pick it up automatically. Promotions are
scoped: a macOS promotion is only selected by `target=macos` leases and never
replaces the Linux or Windows default.

Two ways to point a lease at a specific image during testing:

- Set the provider override env var, for example `CRABBOX_AWS_AMI=ami-...`, to
  boot one candidate without touching the promoted default.
- Promote the AMI so every matching brokered lease uses it.

The lifecycle below moves stable setup *into* the image and keeps per-lease
bootstrap small, which is what produces fast boots.

## What to bake (and what not to)

Bake **machine capabilities** that are stable across runs:

- current OS security updates;
- SSH, Git, rsync, curl, jq, and the readiness helpers;
- TigerVNC / slim XFCE for resize-capable desktop leases;
- Chrome or Chromium for browser leases;
- `ffmpeg`, `ffprobe`, `scrot`, `xdotool`, and other capture helpers;
- Node 24, npm, corepack, pnpm;
- Docker Engine plus the Compose and buildx plugins where the platform supports
  them;
- build-essential, Python, and common native-addon headers;
- empty cache directories such as `/var/cache/crabbox/pnpm`.

Never bake **scenario state**:

- secrets, tokens, or provider credentials;
- browser profiles, cookies, chat/OAuth sessions, or login state;
- source checkouts, `node_modules`, `dist`, PR artifacts, screenshots, or
  videos;
- operator notes or one-off debugging files.

Bake OS patches, developer tools, Docker, browser bits, cache directories,
service enablement, and first-run suppression. Leave repository checkouts,
lockfile-specific installs, login state, and secrets to the warm lease.

## Naming

Use names that make owner, purpose, target, architecture, and UTC bake time
human-auditable in the AWS console:

```text
crabbox-linux-desktop-browser-YYYYMMDD-HHMM
crabbox-linux-devtools-YYYYMMDD-HHMM
crabbox-windows-devtools-YYYYMMDD-HHMM
crabbox-macos-arm64-YYYYMMDD-HHMM
```

## Bake a Linux candidate AMI by hand

The steps below are the manual path. For generic developer images, prefer the
guarded wrapper in [Developer-image wrappers](#developer-image-wrappers).

### 1. Warm a source lease

```bash
crabbox warmup \
  --provider aws \
  --class standard \
  --desktop \
  --browser \
  --ttl 2h \
  --idle-timeout 30m
```

Capture the lease id from the output and use the canonical `cbx_...` id for
image commands, not only the friendly slug.

### 2. Verify the source lease

```bash
crabbox run \
  --provider aws \
  --id <cbx_id> \
  --no-sync \
  --shell -- \
  'set -euo pipefail
   command -v ssh
   command -v git
   command -v rsync
   command -v jq
   command -v node
   command -v pnpm
   command -v ffmpeg
   command -v scrot
   command -v Xtigervnc
   command -v tigervncpasswd
   command -v google-chrome || command -v chromium || command -v chromium-browser
   test -d /work/crabbox
   sudo mkdir -p /var/cache/crabbox/pnpm
   sudo chmod 1777 /var/cache/crabbox /var/cache/crabbox/pnpm'
```

### 3. Create the candidate image

```bash
crabbox image create \
  --id <cbx_id> \
  --name crabbox-linux-desktop-browser-YYYYMMDD-HHMM \
  --wait \
  --json
```

`--wait` polls until the provider image reports `available` (timeout
`--wait-timeout`, default 45m); `--no-reboot` is on by default to avoid
rebooting the source AWS instance during AMI capture. Keep the JSON output and
record at least the AMI id, name, source lease id, creation time, and operator.

## Smoke the candidate before promotion

Boot the candidate explicitly with the provider image override:

```bash
CRABBOX_AWS_AMI=ami-1234567890abcdef0 \
crabbox warmup \
  --provider aws \
  --class standard \
  --desktop \
  --browser \
  --ttl 30m \
  --idle-timeout 10m
```

Run a smoke on the candidate:

```bash
crabbox run \
  --provider aws \
  --id <candidate-cbx_id-or-slug> \
  --no-sync \
  --shell -- \
  'set -euo pipefail
   echo image-smoke-ok
   uname -srm
   command -v node
   command -v pnpm
   command -v ffmpeg
   command -v scrot
   command -v google-chrome || command -v chromium || command -v chromium-browser
   test -d /work/crabbox'
```

For desktop or browser images, also capture a real desktop proof:

```bash
crabbox screenshot --provider aws --id <candidate-cbx_id-or-slug> --output /tmp/crabbox-image-smoke.png
```

Do not promote if SSH readiness, browser startup, screenshot capture, or any
tool check fails.

## Promote

Promote only after a candidate smoke passes:

```bash
crabbox image promote ami-1234567890abcdef0 --json
```

Then confirm a normal brokered lease, with no override, uses the promoted image:

```bash
crabbox warmup \
  --provider aws \
  --class standard \
  --desktop \
  --browser \
  --ttl 30m \
  --idle-timeout 10m

crabbox run \
  --provider aws \
  --id <new-cbx_id-or-slug> \
  --no-sync \
  --shell -- \
  'echo promoted-image-smoke-ok && command -v ffmpeg && command -v node'
```

Keep the previous promoted AMI available until at least one normal brokered
lease and one relevant QA lane pass on the new image.

When promoting an AMI that was **not** created through `crabbox image create`,
pass the scope explicitly so it lands in the right slot — for example
`--target macos --region us-east-1`, plus `--architecture` and `--type` when the
provider cannot infer them. Use `--os` to scope a promoted Linux AMI to a
portable selector such as `ubuntu:26.04`.

## Roll back

For transactional publisher runs, restore the exact captured aliases from the
promotion receipt:

```bash
crabbox image promote ami-failed --restore-receipt promotion.json --json
```

Run the normal brokered smoke again. Do not delete the failed AMI immediately;
keep it long enough to inspect tags, logs, and source-lease details.

## Cleanup

Promotion does not delete old AMIs or EBS snapshots. Cleanup is a provider
operator task:

- keep the current promoted AMI;
- keep the previous known-good AMI until the new one has real QA proof;
- deregister stale failed or candidate AMIs after investigation, e.g.
  `crabbox image delete <ami-id> --provider aws --region <region>`;
- delete their orphaned EBS snapshots in the AWS account.

Do not treat Crabbox coordinator state as the source of truth for old image
storage costs. Check AWS directly.

## Fast Snapshot Restore (cold-start tuning)

An AMI alone is not always enough for low cold-start variance on AWS. EBS
snapshots hydrate lazily by default, so new regions or availability zones can
still pay first-read penalties. For hot lanes:

- keep launch capacity in the same region as the promoted AMI;
- track the wrapper timing logs (below);
- enable AWS Fast Snapshot Restore (FSR) on the backing snapshots only in the
  availability zones where the image must boot immediately.

Enable FSR at promotion time, or check status afterward:

```bash
crabbox image promote ami-1234567890abcdef0 \
  --fast-snapshot-restore \
  --fsr-az us-west-2a \
  --fsr-az us-west-2b \
  --json

crabbox image fsr-status ami-1234567890abcdef0 --region us-west-2 --json
```

FSR is AWS-only. Treat snapshot warmup as a separate provider-cost decision; do
not enable it casually for every candidate.

## GitHub Actions publication

Merging an installer or image-wrapper change updates the source recipe only. It
does not create or promote an AWS image. Actual publication is a successful
`Publish Developer Image` workflow run, which executes the same source smoke,
candidate smoke, promotion, and promoted-image smoke described below.

Configure the `image-publisher` GitHub environment with:

- the `CRABBOX_COORDINATOR` environment variable;
- the `CRABBOX_COORDINATOR_ADMIN_TOKEN` environment secret;
- `CRABBOX_ACCESS_CLIENT_ID` and `CRABBOX_ACCESS_CLIENT_SECRET` environment
  secrets when the coordinator is behind Cloudflare Access.

Add required reviewers to that environment so paid image creation and
fleet-wide promotion need explicit administrator approval. Dispatch one
platform at a time from the protected default branch:

```bash
gh workflow run devtools-image-publish.yml \
  --ref main \
  -f target=linux \
  -f region=eu-west-1

gh workflow run devtools-image-publish.yml \
  --ref main \
  -f target=windows \
  -f region=eu-west-1

gh workflow run devtools-image-publish.yml \
  --ref main \
  -f target=macos \
  -f region=eu-west-1 \
  -f macos_host=use-existing
```

Use `macos_host=allocate` only when no suitable EC2 Mac Dedicated Host is
available. Unmeasured publication uploads its complete mint logs and macOS
lifecycle evidence as a 30-day diagnostic Actions artifact. These diagnostics
are not sanitized public proof. Measured Linux publication initializes one
fixed-path `crabbox-devtools-image-proof/v2` outcome immediately after the
protected checkout, then atomically replaces it after wrapper rollback and
cleanup. The allowlisted outcome uploads on success or failure; private
evidence, receipts, and diagnostics are never copied into the artifact
directory. Candidate failure leaves the default unchanged. Publication is
serialized per target; promotion atomically captures the current scoped
default, and promoted-image smoke failure attempts a compare-and-swap restore.
If another operator promotes a newer image first, rollback fails visibly rather
than overwriting it. Publisher rollback explicitly authorizes retiring the
exact failed catalog revision so capability-aware leases cannot select it;
generic stale compare-and-swap requests leave the catalog unchanged.

## Developer-image wrappers

For generic AWS Linux and Windows developer AMIs, use the guarded wrapper
instead of hand-running the prep and image commands:

```bash
scripts/mint-aws-devtools-image.sh --target linux
scripts/mint-aws-devtools-image.sh --target windows
```

The default is a no-spend plan that prints what it would do and stops. Add
`--run` only when the selected AWS account, region, quotas, and image name are
correct:

```bash
scripts/mint-aws-devtools-image.sh \
  --target linux \
  --region us-west-2 \
  --class standard \
  --type m7i.large \
  --run

scripts/mint-aws-devtools-image.sh \
  --target windows \
  --region us-west-2 \
  --class standard \
  --type m7i.large \
  --windows-mode normal \
  --run
```

The wrapper captures its candidate through `crabbox checkpoint create`, so the
same source/candidate proof works with direct AWS credentials and with an
admin-authenticated broker. Promotion updates broker-managed image defaults;
use `--no-promote` when validating a direct-only AWS configuration.

Enable FSR for hot lanes that need lower first-boot variance, in the AZs you
actually launch from:

```bash
scripts/mint-aws-devtools-image.sh \
  --target windows \
  --region us-west-2 \
  --type m7i.large \
  --fast-snapshot-restore \
  --fsr-az us-west-2a \
  --fsr-az us-west-2b \
  --run
```

### What the prep scripts install

- **Linux** (`scripts/install-linux-developer-tools.sh`): common CLI/build
  tooling, GitHub CLI, Node 24.19.0 on x86_64, corepack/pnpm, TruffleHog 3.95.9, Chrome or
  Chromium for browser lanes, desktop/VNC helpers, Docker Engine, Compose,
  buildx, and a small default Docker image set. TruffleHog archives are pinned
  to reviewed SHA-256 digests for amd64 and arm64. NodeSource, Docker, and
  Chrome APT repositories use scoped keyrings whose primary fingerprints are
  checked before installation; primary-key rotations require a reviewed code
  update and a fresh image-bake smoke. Failed TruffleHog, NodeSource, or Docker
  verification stops image preparation; failed Google verification skips
  Chrome and tries the distro Chromium package. The installer explicitly
  installs every `linux-minimal` package (`ca-certificates`, `curl`, `git`,
  `jq`, `openssh-server`, `rsync`, `tmux`, and `util-linux`) plus the generic
  builder packages (`build-essential`, `git-lfs`, `pkg-config`, `python3`, and
  `python3-venv`). The standalone generated readiness producer verifies every
  functional probe, including creating and checking a disposable pip-enabled
  virtual environment, before atomically emitting the strongest supported profile.
- **Windows** (`scripts/install-windows-developer-tools.ps1`): common CLI/build
  tooling, GitHub CLI, Node 24, corepack/pnpm, TruffleHog 3.95.9, and Windows
  Server container support with Docker Engine. It deliberately avoids Docker
  Desktop because headless image bakes should not depend on a user-session
  desktop app or Docker Desktop licensing. The Chocolatey package, Node MSI,
  TruffleHog archive, and Docker Engine archive are pinned to reviewed SHA-256
  digests and verified before privileged installation or extraction.
- **Windows WSL2**: the shared Windows bootstrap installs the checksum-pinned
  Linux TruffleHog 3.95.9 binary inside the managed WSL distro. This happens
  during environment setup and does not require autoreview-time installation.

Linux preparation retains `cloud-init clean --logs --seed` while preserving
the running source's completed initialization. Using the distro's isolated
`/usr/bin/python3`, it requires cloud-init to report `done` and its configured
runtime directory to be on `tmpfs`, outside the cleaned disk cache. Both
existing completion records are atomically copied there with their original
ownership and modes before cleaning. The source can then pass the subsequent
readiness and smoke commands; a new boot must produce its own completion facts.
Missing cloud-init is a no-op. Incomplete initialization, unsafe runtime storage,
or preservation/cleanup errors stop preparation before sync and version output.
Native checkpoint preparation and the wrapper's reboot-enabled capture remain
unchanged.

Windows developer bakes are headless by default for faster boot and fewer
desktop-bootstrap moving parts. Pass `--desktop` only when the image must back
interactive desktop leases. Windows container support can require one reboot
before Docker starts; the wrapper detects the prep script's reboot marker,
reboots the source lease, waits for Crabbox readiness, reruns the prep script to
pull the configured Docker images, and only then runs the source smoke and AMI
capture.

### Linux public toolchain archives

The x86_64 recipe installs the checksum-pinned upstream Node 24.19.0 archive,
including its bundled Corepack 0.35.0, at
`/opt/hostedtoolcache/node/24.19.0/x64`. The sibling `x64.complete` marker is
written only after executable checks. This matches the GitHub tool-cache
`$RUNNER_TOOL_CACHE/node/<version>/<architecture>` layout; it does not bind a
runner to that root. Crabbox's GitHub runner defaults to
`$HOME/actions-runner/_work/_tool`, and local Actions uses a disposable
per-lease tools directory. Consumers must explicitly select the baked root or
use the verified archives. Completion markers are availability hints, not
authentication.

Before cache or network preparation, the pinned Node route checks all six
public aliases: `node`, `npm`, `npx`, `corepack`, `pnpm`, and `pnpx`. It repeats
the check before replacing the image toolcache slot. Each alias must be absent
or an absolute symlink to its same-named binary in the exact Node 24.19.0 x64
slot; dangling matching links are allowed. Files, directories, and other link
targets stop the bake with a resolve-before-rebake diagnostic. Relative aliases,
including those from earlier unshipped builder revisions, require operator
resolution rather than automatic ownership inference.

Corepack enables its shims only inside the private staged Node tree. The
installer publishes each public alias using a private temporary symlink and
rename, then prepares the selected pnpm version without running public
`corepack enable`. Existing public `yarn` and `yarnpkg` entries remain untouched.
Each alias replacement is atomic; the six replacements are not one transaction.

Public archives are retained under `/opt/crabbox/toolchain-archives`:

| Filename | Purpose |
| --- | --- |
| `node-v24.19.0-linux-x64.tar.xz` | Node, npm, and bundled Corepack |
| `pnpm-11.22.0.tgz` | pnpm 11.22.0 |
| `pnpm-12.3.4.tgz` | pnpm 12.3.4 JavaScript wrapper |
| `exe.linux-x64-12.3.4.tgz` | pnpm 12.3.4 native executable for glibc Linux x64 |

The SHA-256 Node pin and SHA-512 pnpm pins live in the installer's
`toolchain_archive_spec`. Consumers must carry independently reviewed pins,
copy archives into private staging, validate those exact bytes, and extract
fresh trees. Do not authenticate a cached installation by running `--version`,
reading `.complete`, or trusting Corepack's mutable `.corepack` metadata.
No Corepack-packed bundle is provided: any future packed bundle needs its own
trusted digest before import.

For offline Corepack execution, the smoke extracts authenticated pnpm into a
fresh `$COREPACK_HOME/v1/pnpm/<version>` and then creates compatibility metadata.
pnpm 12 also needs the independently verified native archive's `package/pnpm`
installed as `pnpm-native` in that fresh pnpm directory. The consumer's exact
package-manager pin selects execution; these archives do not change the
installer's pnpm default of 11.1.0. Existing `CRABBOX_LINUX_PNPM_VERSION` and
`CRABBOX_LINUX_NODE_MAJOR` overrides remain supported. Other Node majors and
the existing ARM installer route retain the fingerprint-checked NodeSource
path; this recipe does not add an ARM image.

For a nondefault Node major, the installer selects an exact native-architecture
version from that major's fingerprint-checked NodeSource repository. An explicit
`CRABBOX_LINUX_NODE_MAJOR=22` permits replacing an installed Node 24 package with
Node 22; it does not authorize other downgrades or change the default Node 24
route. APT failure or a failed installed-package version, architecture, or
package-owned binary check leaves the owned links and caches intact.

Only after those checks does the installer recheck and remove its six exact
Node 24.19.0 toolcache symlinks, including dangling ones. Operator files,
nonmatching symlinks, and cached archives and trees remain intact. It clears the
shell command cache and checks normal PATH selection before preparing Corepack.
A conflicting operator-provided Node stops the rebake with a PATH diagnostic
rather than being deleted. The alternate toolchain must provide npm and Corepack;
this does not add packaging for newer Node majors.

The mint wrapper applies this archive contract only when its selected prep
script is the bundled Linux builder. It forwards the existing
`CRABBOX_LINUX_NODE_MAJOR` and `CRABBOX_LINUX_PNPM_VERSION` overrides to that
builder and freezes the same Node-major declaration into each smoke. The smoke
checks the guest's Debian package architecture, not the mint host's architecture.
Only Node major 24 on guest `amd64` requires these archives. ARM guests, other
Node-major overrides, and custom prep scripts retain the existing normal-tool
smoke; their success does not qualify the x86_64 archive recipe. Missing or
corrupt archives cannot disable the required probe for the supported builder.

No repository checkout, project dependency tree, credential, or private
package is added to the public archive cache. Existing dependency-cache keys
and hydration behavior are unchanged.

### Tuning the prebake set

```bash
CRABBOX_LINUX_DOCKER_IMAGES='hello-world ubuntu:24.04 node:24-bookworm'
CRABBOX_WINDOWS_DOCKER_IMAGES='mcr.microsoft.com/windows/servercore:ltsc2022'
CRABBOX_WINDOWS_NODE_VERSION='<version>'
CRABBOX_WINDOWS_NODE_SHA256='<reviewed-64-hex-sha256>'
CRABBOX_WINDOWS_DOCKER_VERSION='<version>'
CRABBOX_WINDOWS_DOCKER_SHA256='<reviewed-64-hex-sha256>'
CRABBOX_LINUX_BROWSER=0
CRABBOX_LINUX_DESKTOP_TOOLS=0
CRABBOX_WINDOWS_INSTALL_DOCKER=0
```

The bundled Node and Docker versions use embedded reviewed digests. Override a
version only together with its matching SHA-256 value; unpaired or malformed
overrides fail before the artifact is downloaded.

### Wrapper behavior

The wrapper defaults to `--class standard` even when an explicit instance type
is given, so bakes do not consume the high-pressure beast class. It proves the
source lease, candidate AMI, and promoted AMI before declaring success unless
`--no-promote` is set, and writes warmup timing logs under
`.crabbox/image-mint-<image-name>-*.log.*` with a per-invocation suffix. Each
warmup prints its exact `log=` path. Candidate proof requires
`source=explicit`; final proof requires `source=promoted` with the exact AMI ID
created by the run. For Linux, the wrapper stages
`scripts/linux-readiness.generated.sh` before preparation, the installer invokes
that standalone producer after installing its packages, and the wrapper reruns
the producer before any image capture. This proves the canonical, root-owned
`/var/lib/crabbox-readiness/linux.json` manifest and emits the legacy
`/var/lib/crabbox/image-ready` marker only after all profile probes pass. A
missing builder capability downgrades the manifest to `linux-minimal`; a missing
minimal capability stops preparation before AMI capture. The authoritative
readiness directory is root-owned even when `/var/lib/crabbox` belongs to the
runtime user; the marker is only a backwards-compatibility hint, written through
a safe same-filesystem rename and verified before capture. Later managed Linux
boots independently rerun the declared probes under a sanitized system PATH
before skipping baseline APT. Use the timing logs to compare provider request,
network readiness, bootstrap, and end-to-end time before and after each bake.

Linux source, candidate, and promoted smokes require a nonroot user and execute
the normal `pnpm --version` command in that user's existing environment, preserving
its readiness check and first-use cache warming. This normal command is not
used to authenticate cached archives or skip their verification.

For the bundled Node-24/amd64 builder, each smoke additionally revalidates public
archive bytes in private temporary directories. It executes fresh Node and both
pinned pnpm versions, with Corepack network access disabled, installs a local
dependency using `--offline --ignore-scripts`, and loads that dependency. A failed
probe stops the stage; `devtools-smoke-ok` is printed only after the required
checks finish. These offline probes do not replace image-selection,
credential-isolation, rollback, or cleanup qualification.

### Measured Linux publication

Opt in with `--measured --max-p95-runner-total-ms <positive-integer>`.
The workflow exposes the same opt-in as `measured=true` and
`max_p95_runner_total_ms`. It is off by default. Windows, macOS, and the ordinary
three-lease Linux lifecycle do not gain benchmark launches.

The measured plan schedules **12 leases**, not three: three baseline
measurements, three explicit-candidate measurements, three normal
promoted-selection measurements, and the source/candidate/promoted lifecycle
leases. The wrapper prints this plan, the threshold, and the per-lease TTL
before paid work. These are planned successful allocations, not a hard cap:
provider acquisition may retry and add launch attempts. The wrapper does not
enforce an attempt or dollar cap, or estimate prices. Review the extra
allocations and image storage costs before adding `--run`; do not reuse
the separate qualification workflow's three-launch budget.

Choose a positive absolute p95 runner-time cap before the campaign, based on
the operator's acceptance policy. There is no default performance target.
The baseline cohort is evidence-only and does not apply the cap. The explicit
candidate and normal-selection promoted cohorts must both pass it. All three
p95 values are recorded side by side as a descriptive comparison; passing
`bench check` does not establish a speedup or statistical significance.

Measured mode validates the bundled Linux recipe and its exact input hashes
before the first CLI operation. It requires clean source, an explicit region
and instance type, x86_64, desktop/browser capabilities, the bundled prep
script without Linux installer overrides, promotion, and cleanup. Custom prep,
`--keep-lease`, `--no-promote`, and FSR are rejected in this mode.
Set the existing `CRABBOX_OWNER` and `CRABBOX_ORG` selectors for a bounded
administrative lease listing. Before allocation, offline `config show` must
report a managed coordinator with configured user/admin auth, the requested
region, and an empty effective `aws.ami`. Clearing the environment override
does not clear an AMI inherited from config; remove that override first.
Offline config cannot inspect the coordinator's own environment. A
coordinator-side image override is rejected from the first recorded selection,
after stopping that allocation; this is not a no-spend server-side preflight.

Each measurement uses a fresh `run --timing-record`, the same `true` command,
source revision, machine request, region, capabilities, and
`--full-resync --no-hydrate` policy. `--keep --stop-after never --lease-output`
publishes a retained-lease handle before command execution. No `--id` or pool
is supplied. The handle must say `reused=false` and `kept=true`; its lease and
run IDs must match the timing record. The wrapper reads the exact lease from
the existing bounded administrative list, checks actual instance and image
regions, and rejects mixed baseline images or repeated provider instances.
Missing or ambiguous records fail closed; it never guesses a lease from a slug.

The wrapper confirms cleanup with exact-ID `stop` on success or failure before
starting another sample. A failed run keeps its original exit status even if
cleanup also fails. Interruptions recover an already-published retained handle
without waiting for a final timing record. The original runner timing excludes the subsequent evidence
read and cleanup wait, consistently across all cohorts. Warmup timings are not
benchmark samples, and a `--cold` label alone is not evidence of fresh acquisition.
Existing `bench report` owns all three timing distributions. `bench check`
enforces the predeclared p95 policy only for candidate and promoted cohorts.
Missing, mixed, reused, or insufficient observations block promotion; measured
`0ms` sync remains valid.

Candidate lifecycle cleanup and all candidate measurements finish before
transactional promotion. The original promotion receipt remains unchanged.
The baseline image is reconciled with the receipt's captured previous default
before the post-promotion smoke and again before final acceptance.
The normal-selection path clears the environment AMI override, and
all promoted measurements must prove the new image was selected normally.
Rollback remains armed through the final measurements and outcome projection.
Failure attempts the existing receipt-based restore and exact failed catalog
revision retirement, retaining the original failure status. A concurrent
newer promotion causes visible CAS rejection, never an overwrite.

The public `manifest.json` contains only fixed state labels, source,
recipe/policy and opaque promotion-binding digests, numeric counts and
measures, selection states, rollback/cleanup states, and the exit code.
`plannedLeaseCount` is the campaign plan, not an observed provider-attempt
count. The baseline records evidence with `policyApplied=false`; only candidate
and promoted cohorts can report a passed policy. Failed and incomplete
campaigns retain the last strictly validated partial cohorts without exposing
raw errors. Raw records, command text, paths, image/lease identities, provider
metadata, promotion receipts, handles, reasons, and diagnostic logs remain in
the private runner directory and are not uploaded. Full config and
administrative listings are never logged or persisted.

Run the first measured publication as a supervised pilot. The wrapper finalizer
can restore a captured promotion and publish the final outcome after ordinary
errors, `SIGINT`, or `SIGTERM`, but it cannot run after runner loss or
`SIGKILL`. In that case the initialized outcome remains conservative; inspect
the private operator evidence, verify the current promoted default, and use the
captured receipt for an explicit compare-and-swap rollback before retrying.

## macOS images

macOS images use the same `crabbox image create` command, but the source lease
must be an AWS EC2 Mac lease on an allocated Dedicated Host. The host lifecycle
adds extra IAM, quota, and scrubbing constraints, so prefer the guarded scripts.

### Inspect and allocate hosts

```bash
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts list --provider aws --target macos --region eu-west-1
```

If no suitable host is available, dry-run an allocation first:

```bash
crabbox admin hosts allocate \
  --provider aws \
  --target macos \
  --region eu-west-1 \
  --type mac2.metal \
  --dry-run
```

### Resolve IAM before paid allocation

If dry-run reports `UnauthorizedOperation`, update the coordinator AWS identity
with the EC2 Mac host lifecycle policy (see [admin hosts](../commands/admin.md#hosts))
before the real allocation. Confirm the caller identity and print the combined
policy:

```bash
crabbox admin providers identity --provider aws --region eu-west-1 --json > /tmp/crabbox-provider-identity.json
crabbox admin providers policy --provider aws --target macos > /tmp/crabbox-macos-image-policy.json
crabbox admin hosts policy --provider aws --target macos

scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile auto
```

The apply helper dry-runs first. With `--profile auto` it scans local AWS
profiles and selects the one whose account matches the coordinator account. Once
the dry-run targets the right account and target, attach the combined policy and
rerun the preflight:

```bash
scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile <aws-profile> \
  --apply
```

For assumed-role identities, attach the policy to the underlying role name from
the ARN, not the session name. `admin providers identity --provider aws --json`
includes `policyTarget.type` and `policyTarget.name` when Crabbox can derive the
IAM attachment target from the ARN.

The host policy only unblocks Dedicated Host allocation and release. The full
paid lifecycle also needs the baseline AWS provider permissions in
[Infrastructure](../infrastructure.md#aws-ec2) for key pairs, security groups,
macOS `RunInstances`, AMI creation, candidate boot, promotion, snapshot cleanup,
and lease termination. Print the baseline policy with
`crabbox admin providers policy --provider aws`, or the combined provider plus
Dedicated Host policy with `crabbox admin providers policy --provider aws --target macos`.

### No-spend audit bundle

For a single artifact bundle covering identity, IAM policy, quota, allocation
dry-run, profile matching, and quota-request dry-run evidence:

```bash
scripts/macos-coordinator-remediation-audit.sh --region eu-west-1 --type mac2.metal --profile auto
```

The audit writes `summary.json` with `blocked` or `ready-for-paid-smoke`,
artifact-relative evidence paths, blocker names, and exact remediation commands.
After IAM clears, the next useful no-spend blocker is host quota — confirm it
with `crabbox admin hosts quota ...` before any real allocation.

### End-to-end lifecycle smoke

```bash
scripts/macos-image-lifecycle-smoke.sh
```

By default it runs host offering/list/dry-run checks and stops before paid
allocation or lease creation. It continues only when the dry-run JSON reports at
least one availability zone with `ok: true`. Opt in to the paid lifecycle
explicitly:

```bash
CRABBOX_MACOS_ALLOCATE=1 \
CRABBOX_MACOS_PROMOTE=1 \
scripts/macos-image-lifecycle-smoke.sh
```

When allowed to run paid work, the script warms a macOS desktop lease, verifies
SSH/sync/VNC prerequisites and a developer toolchain (Apple developer tools
directory, a macOS SDK via `xcrun`, Swift, Homebrew, Node/npm/corepack/pnpm,
Python 3), starts WebVNC, waits for the portal bridge to report
`connected=true`, collects desktop artifacts, creates a candidate AMI with a
rebooting capture, boots and smokes the candidate, then promotes and smokes the
promoted image when `CRABBOX_MACOS_PROMOTE=1`.

Toolchain gating defaults to Command Line Tools-compatible checks; set
`CRABBOX_MACOS_REQUIRE_XCODE=1` for SwiftPM, app, or SDK lanes that need
Xcode.app. For `mac2*` families the defaults are macOS 14+ and Swift tools 6.0+;
for newer `mac-m*` families the defaults are macOS 15+ and Swift tools 6.2+.
Tune with `CRABBOX_MACOS_REQUIRED_MAJOR` and `CRABBOX_MACOS_REQUIRED_SWIFT_TOOLS`.
Tune the WebVNC bridge wait with `CRABBOX_MACOS_WEBVNC_WAIT_TIMEOUT`,
`CRABBOX_MACOS_WEBVNC_WAIT_INTERVAL`, and the post-start grace period with
`CRABBOX_MACOS_WEBVNC_START_GRACE`.

EC2 Mac Dedicated Hosts have provider-side billing and release constraints. The
script stops each lease's local WebVNC daemon before cleanup, waits for the host
to return to `available` between macOS boots, and releases the host only when
`CRABBOX_MACOS_RELEASE_HOST=1`. Host release is honored for source-only,
candidate-only, and promoted-image runs, but it refuses to release a
pre-existing host unless `CRABBOX_MACOS_RELEASE_EXISTING_HOST=1` is also set.

Every run writes `.crabbox/macos-image-smoke/<image-name>/summary.json` with the
current phase, host id, lease ids, AMI id when available, blocker remediation
commands, and artifact paths, and preserves baseline policy, host
offering/list/dry-run, allocation, image create/promote, host wait, warmup, and
WebVNC evidence under the run's `evidence/` directory. Override the location with
`CRABBOX_MACOS_ARTIFACT_DIR`.

### Operator-specific source prep

If the source lease needs setup before smoking, pass a local prep script:

```bash
CRABBOX_MACOS_SOURCE_PREP_SCRIPT=scripts/install-macos-developer-tools.sh \
CRABBOX_MACOS_ALLOCATE=1 \
scripts/macos-image-lifecycle-smoke.sh
```

The bundled prep script (`scripts/install-macos-developer-tools.sh`) keeps the
image generic: it verifies Command Line Tools by default, or selects an
installed `/Applications/Xcode*.app` developer directory when
`CRABBOX_MACOS_REQUIRE_XCODE=1`. It installs Homebrew when missing, installs
common developer packages (Git, GitHub CLI, jq/yq, ripgrep, fd, ShellCheck,
shfmt, Python, Node 24, pnpm via corepack), installs TruffleHog 3.95.9 from a
reviewed SHA-256-pinned archive, and creates `/usr/local/bin` shims so non-login
SSH commands find those tools after the AMI boots. It does not download Xcode —
install Xcode in a private prep hook first if the base image lacks it. Do not
put Apple credentials, download tokens, or private package mirrors in this
repository or in baked images.

### Generic developer-tools wrapper

```bash
scripts/mint-macos-devtools-image.sh
```

The default is no-spend: it runs coordinator, IAM, offering, quota, host list,
and allocation dry-run checks, writes the lifecycle summary, and stops before
lease creation. The wrapper is stricter than the generic lifecycle: it defaults
to `mac-m4.metal`, macOS 15+, Swift tools 6.2+, and full Xcode.app via
`CRABBOX_MACOS_REQUIRE_XCODE=1`. For an older CLT-only image set
`CRABBOX_MACOS_TYPE=mac2.metal`, `CRABBOX_MACOS_REQUIRED_MAJOR=14`,
`CRABBOX_MACOS_REQUIRED_SWIFT_TOOLS=6.0`, and `CRABBOX_MACOS_REQUIRE_XCODE=0`.

Mint from an already available host, or allow paid allocation when none exists:

```bash
scripts/mint-macos-devtools-image.sh \
  --region us-west-2 \
  --type mac-m4.metal \
  --use-existing

scripts/mint-macos-devtools-image.sh \
  --region us-west-2 \
  --type mac-m4.metal \
  --allocate
```

The wrapper sets `CRABBOX_MACOS_SOURCE_PREP_SCRIPT` to
`scripts/install-macos-developer-tools.sh`, names images with the
`crabbox-macos-devtools-<timestamp>` prefix, promotes the AMI after candidate
proof, and keeps checkpoint fork proof on by default. Use `--no-promote` for a
candidate-only run, `--no-checkpoint` to skip checkpoint fork proof, and
`--release-host` only when the AWS Dedicated Host can be released safely. Even
with an available host, the script stops after preflight unless
`CRABBOX_MACOS_RUN=1` or `CRABBOX_MACOS_ALLOCATE=1` is set.

Stopping or terminating an EC2 Mac instance starts the AWS host scrubbing
workflow. The script waits up to `CRABBOX_MACOS_HOST_WAIT_TIMEOUT` (default `5h`,
because Apple silicon scrubbing can take up to 4.5 hours) before each next macOS
boot; override `CRABBOX_MACOS_HOST_WAIT_INTERVAL` to change the poll interval.

### Manual macOS bake (advanced)

To force allocation and warm a Mac lease directly:

```bash
crabbox admin hosts allocate \
  --provider aws \
  --target macos \
  --region eu-west-1 \
  --type mac2.metal \
  --force

crabbox warmup \
  --provider aws \
  --target macos \
  --type mac2.metal \
  --market on-demand \
  --desktop \
  --ttl 2h \
  --idle-timeout 30m
```

Verify the source lease before creating the AMI:

```bash
crabbox run \
  --provider aws \
  --target macos \
  --id <cbx_id> \
  --no-sync \
  --shell -- \
  'set -euo pipefail
   sw_vers
   command -v ssh
   command -v git
   command -v rsync
   command -v curl
   test -d "$HOME/crabbox"
   test -w "$HOME/crabbox"
   nc -z 127.0.0.1 5900'
```

Then create and promote the candidate:

```bash
crabbox image create \
  --id <cbx_id> \
  --name crabbox-macos-arm64-YYYYMMDD-HHMM \
  --wait \
  --json

crabbox image promote ami-1234567890abcdef0 --target macos --region us-east-1 --json
```

## Pre-merge Linux AWS qualification

The reviewer-gated `Image qualification` workflow qualifies an exact open,
same-repository pull request against the credential-isolated authority described
in [AWS image qualification](../behavior/aws-image-qualification.md). It is
stacked on that authority and is not a general pull-request CI job.

The workflow has separate trust zones:

- `authorize` binds one first attempt to the protected default-branch workflow,
  open same-repository pull request, and exact candidate SHA. The pull request
  base must equal the protected workflow SHA, so a stale candidate must be
  rebased before qualification.
- `build-candidate` is a credentialless job in that protected workflow. It uses
  the protected revision's Go version, Worker lockfile, Wrangler binary, config,
  and other non-source build inputs. Candidate Go modules and non-source Worker
  inputs must be byte-identical to that revision. The job copies only a bounded
  regular-file candidate `worker/src` tree into the protected build root, never
  runs candidate package tooling or hooks, and records the source and protected
  input digests before publishing the one-day manifest-covered artifact. Later
  jobs accept only that artifact ID and digest from the current first-attempt
  protected workflow run.
- `admit` runs without cloud credentials before environment approval. Trusted
  tooling verifies the exact artifact manifest and rejects publishers that do
  not implement injectable CLI delegation, pre-promotion candidate teardown,
  transactional promotion receipts, compare-and-swap rollback, and failed
  revision retirement in the required order.
- `deploy-enroll` is environment-protected. It checks out only protected
  tooling, downloads the exact artifact ID into runner temporary storage,
  revalidates every manifest entry and the admission contract, treats the
  candidate bundle as data, deploys through the Cloudflare API, reads the
  resulting Worker version and settings back, rechecks the pull request and
  build identity, and claims the singleton authority registry.
- `arm` is the last protected job before candidate execution. It rechecks the
  open pull request and artifact, exact final Worker version and binding
  settings, registry claim, and authority attestation. Its execution-manifest
  digest binds the deployed version to the candidate, deployment, authority,
  policy, and enrollment timestamps.
- `execute` receives only the relay URL and its distinct ephemeral executor
  token. Candidate admin/shared tokens stay in the relay. The job receives no
  AWS, Cloudflare, authority-controller, or production credentials.
- `finalize` always runs behind the protected environment. It first persists the
  authority and registry finalization fence, then disables and deletes the
  public relay and verifies its absence before continuing AWS cleanup. It
  deletes the candidate Fleet Durable Object and Worker, verifies absence,
  repeats finalization idempotently, retires the registry record, and deletes
  the transient controller.

The exact live proof seeds the fixed base AMI as the prior default, verifies
that a shared-token request to `promote-cas` returns 403 without changing the
base-image readback, and records a Fast Snapshot Restore rejection before
signer dispatch. The candidate publisher then boots source, candidate-image,
and promoted-image leases sequentially. A trusted `CRABBOX_BIN` adapter
delegates every command to the exact candidate CLI and captures the structured
promotion and rollback receipts. Candidate API readbacks must prove the exact
seeded base image and revision were restored and the failed image revision lost
its catalog role. A `200` readback for that AMI is accepted only as a matching
provider-only record with no revision, promotion timestamp, or catalog-only
marker; `404` is also valid. A stale request naming the failed revision must
then return 409 with the seeded revision as current,
while complete candidate API readbacks remain unchanged, including catalog,
default, and FSR state. Candidate logs are supplemental only. The adapter
returns exit 86 only after the promoted smoke succeeds. A credentialless child
is then killed while authority-owned image state remains for protected cleanup.

The authority and candidate configuration fix the run to Linux, one
`t3.small`/`t3a.small` on-demand instance at a time, exactly three launches,
one active image/checkpoint set, encrypted bounded root storage, no instance
profile, no Fast Snapshot Restore, a $10 usage ceiling, one attempt, and an
absolute 120-minute expiry. Execution stops after 80 minutes so protected
cleanup retains at least 20 minutes.

The independent `Image qualification reaper` runs after workflow completion
and hourly. It discovers the active run from the durable registry rather than
workflow artifacts. If the controller Worker disappeared, it recovers the
deployment hash from the isolated candidate's service-binding settings,
recreates only the protected controller, and performs the same idempotent
finalization and zero-residue checks.

After clean teardown, both the controller and candidate are absent. The reaper
then creates a transient controller solely to ask the authority registry whether
it is empty. Only an authenticated `{ "run": null }` response establishes idle;
an active run, malformed response, or failed request remains a failure. The
probe cannot finalize or retire a run.

The reaper checks controller absence before recovery and never replaces an
existing controller because authentication or discovery failed. Each idle
probe or candidate-recovery controller carries a unique ownership tag;
cleanup rechecks its version, tag, and bindings before deletion and verifies
absence before reporting idle. Partial
deployment failures still attempt owned cleanup. Changed or unverifiable
ownership preserves the Worker and reports failure for operator investigation,
including both discovery and cleanup errors when applicable. Inspect the
reaper's `finalization.json` before retrying; do not delete an unfamiliar
controller to force recovery. A stale candidate hash can be skipped only after
its owned recovery controller has been deleted and absence verified.

Before enabling the workflow, maintainers must create the protected
`image-qualification` GitHub environment and configure:

- secret `CLOUDFLARE_API_TOKEN`, limited to deploying, inspecting, and deleting
  the qualification candidate/controller Workers and their Durable Objects;
- secret `CRABBOX_IMAGE_QUALIFICATION_CONTROLLER_TOKEN`;
- variable `CLOUDFLARE_ACCOUNT_ID`;
- variables `CRABBOX_IMAGE_QUALIFICATION_AUTHORITY_SHA`,
  `CRABBOX_IMAGE_QUALIFICATION_AUTHORITY_VERSION`,
  `CRABBOX_IMAGE_QUALIFICATION_POLICY_HASH`,
  `CRABBOX_IMAGE_QUALIFICATION_AWS_REGION`,
  `CRABBOX_IMAGE_QUALIFICATION_SUBNET_ID`,
  `CRABBOX_IMAGE_QUALIFICATION_SECURITY_GROUP_ID`,
  `CRABBOX_IMAGE_QUALIFICATION_BASE_AMI_ID`, and
  `CRABBOX_IMAGE_QUALIFICATION_ROOT_GB`.

The non-public authority Worker and its dedicated sandbox-account IAM deny
contract must already be deployed. This repository change creates no
environment, secret, Worker, Durable Object, AWS resource, or spend by itself.

## Hetzner status

Hetzner image bytes belong in the Hetzner project. Crabbox can boot a configured
image through `image` or `CRABBOX_HETZNER_IMAGE`. Direct native checkpoints can
create, verify, delete, and fork Hetzner project snapshots, while brokered
Hetzner checkpoints remain workspace archives. Hetzner `image create` and
`image promote` remain unsupported coordinator operations. Direct `image
delete --provider hetzner` only accepts a snapshot backed by exactly one local
`hetzner-snapshot` checkpoint record and applies the checkpoint ownership checks
before deletion.

## Related docs

- [Prebaked runner images](prebaked-images.md)
- [image command](../commands/image.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
