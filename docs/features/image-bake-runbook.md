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

Rollback is just another promotion to a known-good AMI:

```bash
crabbox image promote ami-previous-good --json
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
available. The workflow uploads its complete mint logs and macOS lifecycle
evidence as a 30-day Actions artifact. A failed candidate or promoted-image
smoke fails the workflow and leaves the previous promoted image selected.

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
  tooling, GitHub CLI, Node 24, corepack/pnpm, TruffleHog 3.95.9, Chrome or
  Chromium for browser lanes, desktop/VNC helpers, Docker Engine, Compose,
  buildx, and a small default Docker image set. TruffleHog archives are pinned
  to reviewed SHA-256 digests for amd64 and arm64. NodeSource, Docker, and
  Chrome APT repositories use scoped keyrings whose primary fingerprints are
  checked before installation; primary-key rotations require a reviewed code
  update and a fresh image-bake smoke. Failed TruffleHog, NodeSource, or Docker
  verification stops image preparation; failed Google verification skips
  Chrome and tries the distro Chromium package.
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

Windows developer bakes are headless by default for faster boot and fewer
desktop-bootstrap moving parts. Pass `--desktop` only when the image must back
interactive desktop leases. Windows container support can require one reboot
before Docker starts; the wrapper detects the prep script's reboot marker,
reboots the source lease, waits for Crabbox readiness, reruns the prep script to
pull the configured Docker images, and only then runs the source smoke and AMI
capture.

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
warmup prints its exact `log=` path; use those files as the evidence to compare
before and after each bake.

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

## Hetzner status

Hetzner image bytes belong in the Hetzner project. Crabbox can boot a configured
image through `image` or `CRABBOX_HETZNER_IMAGE`, but Hetzner image
create/promote lifecycle commands are not implemented yet. Until then, create
and manage Hetzner snapshots with Hetzner tooling, then configure Crabbox to use
the selected image.

## Related docs

- [Prebaked runner images](prebaked-images.md)
- [image command](../commands/image.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
