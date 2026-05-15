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
- Node 22, npm, corepack, pnpm;
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

## macOS Images

macOS images use the same `image create` command, but the source lease must be
an AWS EC2 Mac lease on an allocated Dedicated Host:

```bash
crabbox admin mac-hosts offerings --region eu-west-1 --type mac2.metal
crabbox admin mac-hosts list --region eu-west-1
```

If no suitable host is available, allocate one explicitly before warmup:

```bash
crabbox admin mac-hosts allocate \
  --region eu-west-1 \
  --type mac2.metal \
  --dry-run
```

If dry-run reports `UnauthorizedOperation`, update the coordinator AWS identity
with the EC2 Mac host lifecycle policy in [admin](../commands/admin.md#mac-hosts)
before doing the real allocation. Confirm the caller identity and print the
copy-pasteable policy with:

```bash
crabbox admin aws-identity --region eu-west-1
crabbox admin aws-policy
crabbox admin mac-hosts policy
```

Do not treat that host policy as the whole image bake policy. It only unblocks
Dedicated Host allocation and release. The full paid lifecycle also needs the
normal AWS provider permissions in [Infrastructure](../infrastructure.md#aws-ec2)
for key pairs, security groups, macOS `RunInstances`, AMI creation, candidate
boot, promotion, snapshot cleanup, and lease termination. Print the baseline
provider policy with `crabbox admin aws-policy`.

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
starts WebVNC, waits for the portal bridge to report `connected=true`, collects
desktop artifacts, creates a candidate AMI with a rebooting image capture,
boots and smokes the candidate, then promotes and smokes the promoted image
when `CRABBOX_MACOS_PROMOTE=1`. Tune the WebVNC bridge wait with
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
baseline AWS provider policy, EC2 Mac host policy, host offering/list/dry-run,
allocation, image create, image promotion, host wait, warmup, and WebVNC status
evidence under the run's `evidence/` directory. Override the directory with
`CRABBOX_MACOS_ARTIFACT_DIR`.

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
crabbox admin mac-hosts allocate \
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
