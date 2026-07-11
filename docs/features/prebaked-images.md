# Prebaked Runner Images

A prebaked image is a provider machine image (AWS AMI, Hetzner snapshot, and so
on) with the stable parts of a runner already installed, so a lease boots ready
instead of installing tooling on every warmup.

Read this when you are:

- deciding what belongs in a provider image versus a warm lease or a repo cache;
- speeding up `crabbox warmup` and `crabbox run` for desktop or browser QA;
- planning to bake or promote a Crabbox runner image.

The guiding rule: **prebaked images store machine capabilities, not scenario
state.** Tools, browsers, and OS patches go in the image. Checkouts, dependency
caches, credentials, and login state stay out.

For the exact AWS bake, smoke, promotion, rollback, and cleanup commands, follow
the [Image bake runbook](image-bake-runbook.md). This page covers the underlying
model.

## Where images live

Provider-owned image storage is always the source of truth for image bytes:

- **AWS** — AMIs and their backing EBS snapshots live in the AWS account.
  `crabbox image create` builds a candidate AMI from a lease, and
  `crabbox image promote` records the selected AMI as the default for matching
  brokered AWS leases. Promotion is scoped by target, architecture, and region,
  so a macOS AMI never replaces the Linux or Windows default.
  Promotions may declare OS, SDK/runtime, browser, WebView2, and desktop
  capabilities. Capability-aware leases select the newest matching AMI from the
  scoped promotion catalog and fail before leasing when no image matches.
- **Azure / GCP** — managed images and disk snapshots live in the cloud project.
  `crabbox image create` can capture them and `crabbox image delete` can remove
  them (`--provider azure|gcp`).
- **Hetzner** — snapshots live in the Hetzner project. Crabbox can already boot a
  configured image (via config or `CRABBOX_HETZNER_IMAGE`), but the
  create/promote lifecycle commands are not implemented for Hetzner. Manage
  Hetzner snapshots with Hetzner tooling, then point Crabbox at the result.
- **Delegated runners** (for example Blacksmith) — images are owned by the
  provider's runner infrastructure, not by Crabbox.

The coordinator stores scoped provider image identifiers, promotion capability
metadata, and enough tags to explain provenance. Do not store image bytes in
git, release artifacts, or coordinator durable state.

## What to bake

Bake stable machine capabilities:

- current OS security updates and base packages;
- core access tooling: SSH, Git, rsync, curl, jq, and the readiness helpers;
- desktop and browser capabilities for `--desktop --browser` leases
  (resize-capable TigerVNC, slim XFCE, Chrome or Chromium);
- capture tools such as `ffmpeg`, `ffprobe`, `scrot`, and `xdotool`;
- language and build toolchains the image targets: Node 24 with corepack/pnpm,
  `build-essential`, Python, and common native-addon headers;
- Docker Engine and supporting plugins where the platform runs headless Docker;
- empty shared cache directories such as `/var/cache/crabbox/pnpm`.

Do not bake scenario state:

- secrets, tokens, or provider credentials;
- browser profiles, cookies, OAuth state, or chat/login sessions;
- repository checkouts, `node_modules`, built `dist/`, or PR artifacts;
- one-off operator notes or debugging files.

Anything that varies per repository, per lockfile, or per run does not belong in
a shared image.

## Runtime caches belong outside the image

Dependency state changes far more often than machine capabilities, so it lives
outside the image:

- a **warm lease** can keep `/var/cache/crabbox/pnpm` and browser profiles for a
  short-lived operator session;
- **GitHub Actions** should cache candidate pnpm stores by lockfile and platform;
- product-specific runtime bundles and evidence belong in the workflow
  workspace, for example under `.artifacts/`;
- long-lived reusable volumes should be keyed by repo, lockfile, runtime version,
  platform, and image id before Crabbox mounts them into leases.

This split keeps one image reusable across many repositories while still letting
slow QA lanes skip repeated dependency work when they deliberately reuse a warm
lease or a keyed external cache.

## Operator flow

The [Image bake runbook](image-bake-runbook.md) has the precise commands and
guard scripts. At a high level, an AWS bake is:

1. Warm a fresh source lease with the capabilities the image must provide:

   ```bash
   crabbox warmup --provider aws --class standard --desktop --browser \
     --ttl 2h --idle-timeout 30m
   ```

2. Verify the machine capability contract on that lease (tools, browser,
   directories) over `crabbox run --no-sync --shell`.
3. Create a candidate AMI from the lease's canonical `cbx_...` id:

   ```bash
   crabbox image create --id <cbx_id> --name my-org-linux-desktop-YYYYMMDD-HHMM \
     --wait --json
   ```

4. Boot the candidate explicitly through an image override and smoke it:

   ```bash
   CRABBOX_AWS_AMI=ami-1234567890abcdef0 \
     crabbox warmup --provider aws --class standard --desktop --browser \
     --ttl 30m --idle-timeout 10m
   ```

5. Promote the candidate once the smoke passes:

   ```bash
   crabbox image promote ami-1234567890abcdef0 --json
   ```

   Add declarations such as `--os-version 26.04 --runtime node=24.2
   --browser --desktop` when future leases must select by baked capabilities.

6. Run a normal brokered lease (no override) plus the relevant QA lane to confirm
   the promoted image is selected and healthy.
7. Keep the previous known-good AMI until the new image has real QA proof.

A successful bake is not just "the browser exists." A useful image measurably
reduces `crabbox warmup` and `crabbox run` time in your timing evidence while
keeping credentials, login state, and repository artifacts out of the image.

## Image commands

All image commands require coordinator admin auth and can create paid
provider-side artifacts.

- `crabbox image create --id <cbx_id> --name <name> [--wait]` — capture a
  provider image from a lease (`--no-reboot` defaults to true on AWS).
- `crabbox image promote <ami-id> [--target linux|macos|windows] [--region <r>]`
  — set the default brokered AWS image; supports `--fast-snapshot-restore` with
  `--fsr-az <az>` and capability declarations.
- `crabbox image fsr-status <ami-id|snapshot-id>` — AWS Fast Snapshot Restore
  status.
- `crabbox image delete <image-id> [--provider aws|azure|gcp]` — remove a
  Crabbox-created provider image. Deletion requires stored Crabbox ownership
  metadata and refuses unrelated provider-native image or snapshot IDs.

See the [image command reference](../commands/image.md) for full flags.

## Related docs

- [Image bake runbook](image-bake-runbook.md)
- [image command](../commands/image.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
