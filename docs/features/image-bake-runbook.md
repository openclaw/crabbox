# Image Bake Runbook

Read when:

- baking a new Crabbox AWS image;
- promoting or rolling back the default AWS image;
- preparing a desktop/browser image for UI QA;
- checking whether state belongs in the image or in a warm lease.

This runbook is for trusted operators. Image commands need coordinator admin
auth and can create provider-side artifacts that cost money until cleaned up.

## Naming

Use names that identify owner, purpose, and UTC bake time:

```text
crabbox-linux-desktop-browser-YYYYMMDD-HHMM
crabbox-linux-devtools-YYYYMMDD-HHMM
crabbox-windows-devtools-YYYYMMDD-HHMM
crabbox-macos-arm64-YYYYMMDD-HHMM
```

Use names that make the target and architecture obvious. A promoted macOS AMI
is scoped separately from Linux and Windows images, but the name should still be
human-auditable in the AWS console.

## What To Bake

Bake machine capabilities:

- current OS security updates;
- SSH, Git, rsync, curl, jq, and readiness helpers;
- Xvfb/slim XFCE/VNC for desktop leases;
- Chrome/Chromium for browser leases;
- `ffmpeg`, `ffprobe`, `scrot`, `xdotool`, and other capture helpers;
- Node 24, npm, corepack, pnpm;
- Docker Engine plus the Compose and buildx plugins where the platform supports
  them;
- build-essential, Python, and common native-addon headers;
- empty cache directories such as `/var/cache/crabbox/pnpm`.

Do not bake scenario state:

- secrets, tokens, or provider credentials;
- browser profiles, cookies, Slack/Discord/WhatsApp sessions, or OAuth state;
- source checkouts, `node_modules`, `dist`, PR artifacts, screenshots, or
  videos;
- local operator notes or one-off debugging files.

## Create A Candidate AMI

Warm a source lease:

```bash
crabbox warmup \
  --provider aws \
  --class standard \
  --desktop \
  --browser \
  --ttl 2h \
  --idle-timeout 30m
```

Capture the lease id from the output. Use the canonical `cbx_...` id for image
commands, not only the friendly slug.

Verify the source lease:

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
   command -v x11vnc
   command -v google-chrome || command -v chromium || command -v chromium-browser
   test -d /work/crabbox
   sudo mkdir -p /var/cache/crabbox/pnpm
   sudo chmod 1777 /var/cache/crabbox /var/cache/crabbox/pnpm'
```

Create the candidate image:

```bash
crabbox image create \
  --id <cbx_id> \
  --name crabbox-linux-desktop-browser-YYYYMMDD-HHMM \
  --wait \
  --json
```

Keep the JSON output. At minimum, record the AMI id, name, source lease id,
creation time, and operator.

## Smoke Candidate Before Promotion

Boot the candidate explicitly. Use the provider image override supported by the
current environment, for example:

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

For desktop/browser images, also run a real desktop/browser proof:

```bash
crabbox screenshot --provider aws --id <candidate-cbx_id-or-slug> --output /tmp/crabbox-image-smoke.png
```

Do not promote if SSH readiness, browser startup, screenshot capture, or the
package/tool checks fail.

## Promote

Promote only after a candidate smoke passes:

```bash
crabbox image promote ami-1234567890abcdef0 --json
```

Then verify a normal brokered lease without overrides uses the promoted image:

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

## Linux And Windows Developer Images

For generic AWS Linux and Windows developer AMIs, use the guarded wrapper
instead of hand-running the prep and image commands:

```bash
scripts/mint-aws-devtools-image.sh --target linux
scripts/mint-aws-devtools-image.sh --target windows
```

The default is a no-spend plan. Add `--run` only when the selected AWS account,
region, quotas, and image name are correct:

```bash
scripts/mint-aws-devtools-image.sh \
  --target linux \
  --region us-west-2 \
  --type m7i.large \
  --run

scripts/mint-aws-devtools-image.sh \
  --target windows \
  --region us-west-2 \
  --type m7i.large \
  --windows-mode normal \
  --run
```

The Linux prep script installs common CLI/build tooling, GitHub CLI, Node 24,
corepack/pnpm, Chrome or Chromium for browser lanes, desktop/VNC helpers, Docker
Engine, Compose, buildx, and a small default Docker image set. The Windows prep
script installs common CLI/build tooling, GitHub CLI, Node 24, corepack/pnpm,
and Windows Server container support with Docker Engine. It deliberately avoids
Docker Desktop because headless image bakes should not depend on a user-session
desktop app or Docker Desktop licensing.

Windows container support can require one reboot before Docker starts. The
wrapper detects the prep script's reboot marker, reboots the source lease,
waits for Crabbox readiness, reruns the prep script to pull the configured
Docker images, and only then runs the source smoke and AMI capture.

Tune the default prebake set with environment variables:

```bash
CRABBOX_LINUX_DOCKER_IMAGES='hello-world ubuntu:24.04 node:24-bookworm'
CRABBOX_WINDOWS_DOCKER_IMAGES='mcr.microsoft.com/windows/servercore:ltsc2022'
CRABBOX_LINUX_BROWSER=0
CRABBOX_LINUX_DESKTOP_TOOLS=0
CRABBOX_WINDOWS_INSTALL_DOCKER=0
```

The wrapper always proves the source lease, candidate AMI, and promoted AMI
before declaring success unless `--no-promote` is set. It writes warmup timing
logs under `.crabbox/image-mint-<image-name>-*.log`, which is the evidence to
compare before and after each bake.

## Fast Boot Expectations

Fast images come from moving stable machine setup into the AMI and keeping
per-lease bootstrap tiny. Bake OS patches, developer tools, Docker, browser
bits, cache directories, service enablement, and first-run suppression. Do not
bake repository checkouts, package installs tied to one lockfile, browser login
state, or secrets.

For Blacksmith-like cold-start times on AWS, an AMI alone is not always enough.
EBS snapshots hydrate lazily by default, so new regions or availability zones
can still pay first-read penalties. For hot production lanes, keep capacity in
the same region as the promoted AMI, track the wrapper timing logs, and enable
AWS Fast Snapshot Restore on the backing snapshots in the availability zones
where the image must boot immediately. Treat snapshot warmup as a separate
provider-cost decision; do not enable it casually for every candidate image.

## macOS Images

macOS images use the same `image create` command, but the source lease must be
an AWS EC2 Mac lease on an allocated Dedicated Host:

```bash
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts list --provider aws --target macos --region eu-west-1
```

If no suitable host is available, allocate one explicitly before warmup:

```bash
crabbox admin hosts allocate \
  --provider aws \
  --target macos \
  --region eu-west-1 \
  --type mac2.metal \
  --dry-run
```

If dry-run reports `UnauthorizedOperation`, update the coordinator AWS identity
with the EC2 Mac host lifecycle policy in [admin](../commands/admin.md#hosts)
before doing the real allocation. Confirm the caller identity and print the
copy-pasteable combined policy with:

```bash
crabbox admin providers identity --provider aws --region eu-west-1 --json > /tmp/crabbox-provider-identity.json
crabbox admin providers policy --provider aws --target macos > /tmp/crabbox-macos-image-policy.json
crabbox admin hosts policy --provider aws --target macos

scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile auto
```

The apply helper dry-runs first. With `--profile auto`, it scans local AWS
profiles and selects the one whose account matches the coordinator account. If
the dry-run is pointed at the right account and target, attach the combined
policy to the coordinator AWS principal before rerunning the preflight:

```bash
scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile <aws-profile> \
  --apply
```

For assumed-role identities, attach the policy to the underlying role name from
the ARN rather than to the session name. `admin providers identity --provider aws --json` includes
`policyTarget.type` and `policyTarget.name` when Crabbox can derive the IAM
attachment target from the ARN.

If dry-run succeeds, run
`crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal`
before real allocation. It prints the selected EC2 Mac Dedicated Host quota
from AWS Service Quotas, which is the next useful no-spend blocker after IAM.
For a single artifact bundle that captures identity, IAM policy, quota,
allocation dry-run, local AWS profile matching, and quota request dry-run
evidence, run:

```bash
scripts/macos-coordinator-remediation-audit.sh --region eu-west-1 --type mac2.metal --profile auto
```

The audit writes `summary.json` with `blocked` or `ready-for-paid-smoke`,
artifact-relative evidence paths, blocker names, and exact remediation
commands.

Do not treat that host policy as the whole image bake policy. It only unblocks
Dedicated Host allocation and release. The full paid lifecycle also needs the
normal AWS provider permissions in [Infrastructure](../infrastructure.md#aws-ec2)
for key pairs, security groups, macOS `RunInstances`, AMI creation, candidate
boot, promotion, snapshot cleanup, and lease termination. Print the baseline
provider policy with `crabbox admin providers policy --provider aws`, or the
combined provider plus Dedicated Host policy with
`crabbox admin providers policy --provider aws --target macos`.

For an end-to-end guarded run, use the repository smoke script:

```bash
scripts/macos-image-lifecycle-smoke.sh
```

By default it only runs host offering/list/dry-run checks and stops before paid
allocation or lease creation. The dry-run is parsed from the command's JSON
output so the script only continues when at least one availability zone reports
`ok: true`. After the dry-run succeeds, opt in to the paid lifecycle
explicitly:

```bash
CRABBOX_MACOS_ALLOCATE=1 \
CRABBOX_MACOS_PROMOTE=1 \
scripts/macos-image-lifecycle-smoke.sh
```

The script warms a macOS desktop lease, verifies SSH/sync/VNC prerequisites,
requires an active Apple developer tools directory, a macOS SDK through
`xcrun`, Swift, Homebrew, Node/npm/corepack/pnpm, and Python 3, starts WebVNC,
waits for the portal bridge to report `connected=true`, collects desktop
artifacts, creates a candidate AMI with a rebooting image capture, boots and
smokes the candidate, then promotes and smokes the promoted image when
`CRABBOX_MACOS_PROMOTE=1`. Command Line Tools are enough by default; full Xcode
is not required unless `CRABBOX_MACOS_REQUIRE_XCODE=1` is set. For `mac2*`
families the default gates are macOS 14+ and Swift tools 6.0+ because those are
the launchable hosts commonly available today. For newer `mac-m*` families the
defaults are macOS 15+ and Swift tools 6.2+, which matches Swift package lanes
that require `swift-tools-version: 6.2` and macOS 15 SDKs. Tune the toolchain
gates with `CRABBOX_MACOS_REQUIRED_MAJOR` and
`CRABBOX_MACOS_REQUIRED_SWIFT_TOOLS`. Tune the WebVNC bridge wait with
`CRABBOX_MACOS_WEBVNC_WAIT_TIMEOUT` and
`CRABBOX_MACOS_WEBVNC_WAIT_INTERVAL`; tune the post-start grace period with
`CRABBOX_MACOS_WEBVNC_START_GRACE`. EC2 Mac Dedicated Hosts have
provider-side billing and release constraints; the script stops each lease's
local WebVNC daemon before lease cleanup, waits for the host to return to
`available` between macOS boots, and releases the host only when
`CRABBOX_MACOS_RELEASE_HOST=1`. Host release is honored for source-only,
candidate-only, and promoted-image runs; the script refuses to release a
pre-existing host unless `CRABBOX_MACOS_RELEASE_EXISTING_HOST=1` is also set.
Every run writes `.crabbox/macos-image-smoke/<image-name>/summary.json` with
the current phase, host id, lease ids, AMI id when available, blocker
remediation commands when blocked, and artifact paths. It also preserves the
baseline AWS provider policy, EC2 Mac host policy, combined macOS image policy,
host offering/list/dry-run, allocation, image create, image promotion, host
wait, warmup, WebVNC daemon, and WebVNC status evidence under the run's
`evidence/` directory.
Override the directory with
`CRABBOX_MACOS_ARTIFACT_DIR`.

If the source lease needs operator-specific setup before smoking, pass a local
prep script:

```bash
CRABBOX_MACOS_SOURCE_PREP_SCRIPT=scripts/install-macos-developer-tools.sh \
CRABBOX_MACOS_ALLOCATE=1 \
scripts/macos-image-lifecycle-smoke.sh
```

The bundled developer-tool prep script keeps the image generic: it verifies
Command Line Tools, installs Homebrew when missing, installs common developer
packages such as Git, GitHub CLI, jq/yq, ripgrep, fd, ShellCheck, shfmt, Python,
Node 24, and activates pnpm through corepack. It also creates `/usr/local/bin`
shims so non-login SSH commands can find those tools after the AMI boots. Use a
private prep hook only for organization-specific setup. Do not put Apple
credentials, download tokens, or private package mirrors in this repository or
in baked images.

For the generic developer-tools image, prefer the small wrapper instead of
remembering the lifecycle environment by hand:

```bash
scripts/mint-macos-devtools-image.sh
```

That default is no-spend: it runs coordinator, IAM, offering, quota, host list,
and allocation dry-run checks, writes the usual lifecycle summary, and stops
before lease creation. To mint from an already available host:

```bash
scripts/mint-macos-devtools-image.sh \
  --region us-west-2 \
  --type mac2.metal \
  --use-existing
```

To allow paid host allocation when no reusable host exists:

```bash
scripts/mint-macos-devtools-image.sh \
  --region us-west-2 \
  --type mac2.metal \
  --allocate
```

The wrapper sets `CRABBOX_MACOS_SOURCE_PREP_SCRIPT` to
`scripts/install-macos-developer-tools.sh`, names images with the
`crabbox-macos-devtools-<timestamp>` prefix, promotes the AMI after candidate
proof, and keeps checkpoint fork proof enabled by default. Use
`--no-promote` for a candidate-only run, `--no-checkpoint` to skip checkpoint
fork proof, and `--release-host` only when the AWS Dedicated Host can be
released safely.

If an available EC2 Mac Dedicated Host already exists, the script still stops
after preflight unless `CRABBOX_MACOS_RUN=1` or `CRABBOX_MACOS_ALLOCATE=1` is
set.

Stopping or terminating an EC2 Mac instance starts the AWS host scrubbing
workflow. The script waits up to `CRABBOX_MACOS_HOST_WAIT_TIMEOUT` before each
next macOS boot; the default is `5h` because Apple silicon scrubbing can take
up to 4.5 hours. Override `CRABBOX_MACOS_HOST_WAIT_INTERVAL` to change the poll
interval. If the host existed before the script started, `CRABBOX_MACOS_RELEASE_HOST=1`
will not release it unless `CRABBOX_MACOS_RELEASE_EXISTING_HOST=1` is also set.

```bash
crabbox admin hosts allocate \
  --provider aws \
  --target macos \
  --region eu-west-1 \
  --type mac2.metal \
  --force
```

```bash
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

Crabbox scopes promoted AWS images by target, architecture, and region. A macOS
promotion is only selected by matching `target=macos` leases, so it will not
replace the Linux or Windows default. If you promote an AMI that was not created
through `crabbox image create`, pass both `--target macos` and `--region`.

## Roll Back

Rollback is another promotion:

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
- deregister stale failed/candidate AMIs after investigation;
- delete their orphaned EBS snapshots in the AWS account.

Do not rely on Crabbox coordinator state as the source of truth for old image
storage costs. Check AWS directly.

## Hetzner Status

Hetzner image bytes belong in the Hetzner project. Crabbox can boot a configured
image through `image` or `CRABBOX_HETZNER_IMAGE`, but Hetzner image
create/promote lifecycle commands are not implemented yet. Until then, create
and manage Hetzner snapshots with Hetzner tooling, then configure Crabbox to use
the selected image.

Related docs:

- [Prebaked runner images](prebaked-images.md)
- [image command](../commands/image.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
