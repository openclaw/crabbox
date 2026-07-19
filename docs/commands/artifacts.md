# artifacts

`crabbox artifacts` turns a desktop lease into durable QA evidence: it collects
screenshots, video, logs, doctor output, and metadata into a bundle, makes
trimmed review media, writes QA summary markdown, and publishes inline-ready
assets to a pull request.

Reach for it when a desktop/WebVNC issue or UI fix needs more than a one-off
screenshot. Subcommands:

- `collect` — gather a full evidence bundle into a directory.
- `video` — record an MP4 from a desktop lease.
- `gif` — make a trimmed GIF from a recorded video (alias for
  [`crabbox media preview`](media.md)).
- `template` — write QA summary markdown.
- `publish` — upload a bundle and optionally comment on a PR.
- `list` / `pull` — read or download files from a published manifest.

## Collect

```sh
crabbox artifacts collect --id blue-lobster --output artifacts/blue-lobster
crabbox artifacts collect --id blue-lobster --all --duration 20s --output artifacts/blue-lobster
crabbox artifacts collect --id blue-lobster --run run_123 --output artifacts/blue-lobster
```

By default `collect` writes:

- `metadata.json`
- `screenshot.png`
- `doctor.txt`
- `webvnc-status.json` when a coordinator login is configured
- `logs.txt` and `run.json` when `--run <run-id>` is provided

The default bundle directory is private. An explicit `--output` directory keeps
its existing sharing permissions, while retained `logs.txt` and `run.json`
remain owner-only. Publishing is unchanged.

`--all` enables `--video` and `--gif`, so the bundle also records `screen.mp4`,
writes a `screen.contact.png` contact sheet, and produces a trimmed
`screen.trimmed.gif`. `--gif` requires `--video` (or `--all`). Linux video uses
remote `ffmpeg`/X11 capture; native Windows captures frames in the interactive
console session and encodes the MP4 locally with `ffmpeg`. MP4 capture is
supported for Linux and native Windows desktop targets only.

When the output directory is omitted, Crabbox derives one from the lease ID and
slug. The bundle is not collected for static (BYO) hosts on non-Linux targets,
or for the Blacksmith provider, which owns its own connectivity.

Useful flags:

```text
--id <lease-id-or-slug>
--output <dir>
--run <run-id>
--all
--screenshot                  default true
--video
--gif
--doctor                      default true
--webvnc-status               default true
--metadata                    default true
--duration <duration>         default 10s
--fps <n>                     default 15
--gif-width <px>              default 1000
--gif-fps <n>                 default 24
--contact-sheet               default true
--no-contact-sheet
--contact-sheet-frames <n>    default 5
--contact-sheet-cols <n>      default 5
--contact-sheet-width <px>    default 320
--provider <name>
--network auto|public|tailscale
--reclaim
--json
```

When collection hits an unhealthy desktop, WebVNC, VNC, or input layer, it
prints the same inline `problem:`, `detail:`, and `rescue:` hints used by the
[`desktop`](desktop.md) and [`webvnc`](webvnc.md) commands. With `--json`,
stdout stays valid JSON and those hints are returned in the `warnings` array
instead. If a capture step fails after the bundle has started, the command
still exits nonzero and includes an `error` object with a stable code and
message.

## Video

```sh
crabbox artifacts video --id blue-lobster --duration 15s --output screen.mp4
```

`video` records an MP4 from a desktop lease and writes a sampled
`*.contact.png` contact sheet beside it by default. Use it when you want capture
separate from a full bundle. Disable the sidecar with `--contact-sheet=false` or
`--no-contact-sheet`, or set `--contact-sheet-output <path>`. Like
`collect --video`, it supports Linux and native Windows desktop targets.

```text
--id <lease-id-or-slug>
--output <path>
--duration <duration>         default 10s
--fps <n>                     default 15
--contact-sheet               default true
--no-contact-sheet
--contact-sheet-output <path>
--contact-sheet-frames <n>    default 5
--contact-sheet-cols <n>      default 5
--contact-sheet-width <px>    default 320
```

## GIF

```sh
crabbox artifacts gif \
  --input screen.mp4 \
  --output screen.trimmed.gif \
  --trimmed-video-output screen.trimmed.mp4
```

`gif` runs the same local motion-trimmed preview logic as
[`crabbox media preview`](media.md); see that page for the full flag list.

## Template

```sh
crabbox artifacts template openclaw \
  --before before.png \
  --after after.gif \
  --summary "Login modal no longer overlaps the toolbar." \
  --output summary.md

crabbox artifacts template mantis --summary-file qa-notes.md
```

`template` writes Markdown with `Summary`, `Before / After`, and `Evidence`
sections sized for QA comments. The kind (`openclaw` or `mantis`) is taken from
the first positional argument or `--kind`. Output goes to stdout unless
`--output <path>` is set.

```text
--kind openclaw|mantis
--before <url-or-path>
--after <url-or-path>
--summary <text>
--summary-file <path>
--output <path>
```

## Publish

```sh
crabbox artifacts publish \
  --dir artifacts/blue-lobster \
  --pr 123

crabbox artifacts publish \
  --dir artifacts/blue-lobster \
  --pr 123 \
  --storage s3 \
  --bucket qa-artifacts \
  --prefix pr-123/blue-lobster \
  --base-url https://qa-artifacts.example.com

crabbox artifacts publish \
  --dir artifacts/blue-lobster \
  --pr 123 \
  --storage cloudflare \
  --bucket qa-artifacts \
  --prefix pr-123/blue-lobster \
  --base-url https://artifacts.example.com
```

`publish` uploads bundle files, writes and publishes `artifact-manifest.json`,
writes `published-artifacts.md`, and comments on the PR with inline images/GIFs
plus links to videos, logs, metadata, and the manifest. Use `--dry-run` to
generate markdown and print intended actions without uploading or commenting.
Pass `--skip-manifest` only when you explicitly want the old markdown-only
output.

```text
--dir <dir>
--storage auto|broker|local|s3|cloudflare|r2   default auto
--bucket <name>
--prefix <path>
--base-url <url>
--pr <n>
--repo <owner/name>
--template openclaw|mantis    default openclaw
--summary <text>
--summary-file <path>
--region <region>
--profile <profile>
--endpoint-url <url>
--acl <acl>
--presign
--expires <duration>          default 168h (7d)
--dry-run
--no-comment
--skip-manifest
--no-manifest                 alias for --skip-manifest
```

Most flags default from `CRABBOX_ARTIFACTS_*` environment variables (see
[Environment defaults](#environment-defaults)).

### Storage backends

- `--storage auto` (default): when a coordinator is configured, Crabbox asks
  the broker for upload URLs and the broker-owned artifact backend handles
  storage credentials. Without a coordinator, auto falls back to `local`.
- `--storage broker` requires a configured coordinator and uploads through
  broker-minted URLs.
- `--storage s3` uses the AWS CLI and uploads to `s3://<bucket>/<prefix>/...`.
- `--storage cloudflare` uses `wrangler r2 object put --remote`.
- `--storage r2` uses the AWS CLI against an S3-compatible R2 endpoint; requires
  `--endpoint-url` (or `CRABBOX_ARTIFACTS_R2_ENDPOINT_URL`).
- `--storage local` writes markdown only. For `--pr`, local publishing needs a
  `--base-url` that already serves the files, otherwise the PR would contain
  unusable local paths.

`s3`, `cloudflare`, and `r2` all require `--bucket`. For `cloudflare`/`r2`, a
`--pr` comment also requires `--base-url` so the inline assets resolve.

When `--base-url` is supplied, published links use that public URL. Otherwise
`--presign` generates temporary AWS/R2 S3 URLs after upload.

For native Cloudflare publishing, `publish` runs `wrangler` with
`CRABBOX_ARTIFACTS_CLOUDFLARE_*` when present, then the generic `CLOUDFLARE_*`
environment. For S3-compatible R2 publishing, pass
`--storage r2 --endpoint-url <r2-endpoint> --profile <r2-profile>`; when set,
`CRABBOX_ARTIFACTS_R2_ENDPOINT_URL`, `CRABBOX_ARTIFACTS_R2_AWS_PROFILE`, and
`CRABBOX_ARTIFACTS_R2_AWS_REGION` are used before generic AWS defaults. Prefer
brokered publishing for shared teams so Cloudflare and object-store secrets stay
on the coordinator.

`publish --pr` shells out to `gh issue comment <pr> --body-file ...`, so the
current checkout must be authenticated with GitHub. Pass `--repo owner/name`
when the working directory is not inside the target repository.

### Brokered publishing

For brokered publishing, the CLI never receives object-store credentials. It
sends artifact names, sizes, content types, and hashes to
`POST /v1/artifacts/uploads`; the coordinator returns one short-lived upload URL
per file plus the final URL to place in Markdown. Upload grants are signed with
the declared `content-length`, so the object store rejects oversized PUTs during
the grant window, and the broker caps each upload request at 5 GiB total before
signing grants. When `--prefix` is omitted for hosted publishing, the CLI
derives a unique prefix from the PR number, bundle directory, and current time
so later QA comments do not overwrite earlier evidence.

The coordinator scopes each new grant under versioned base64url encodings of
the exact authenticated organization and owner. These values are reversible,
not hashed or encrypted, and appear in object paths for both public and signed
URLs. A GitHub user's owner is its immutable `github:<numeric-id>` account
identity. Caller prefixes cannot cross or replace that authorization namespace.

`artifacts publish --pr` can place these URLs in public pull-request comments.
Operators should weigh that identity disclosure before enabling
`CRABBOX_ARTIFACTS_PUBLIC_READS`; the random capability namespace on public
grants prevents easy guessing but does not hide the encoded identity from a URL
recipient. This identity disclosure is an accepted Low/P3 residual risk.

## Manifest, list, and pull

Every publish writes and publishes an `artifact-manifest.json` by default. The
manifest is the durable handoff for PR proof and contains one entry per
published file:

```json
{
  "schemaVersion": 1,
  "storage": { "backend": "broker", "prefix": "pr-123/blue-lobster" },
  "files": [
    {
      "kind": "screenshot",
      "name": "screenshot.png",
      "url": "https://artifacts.example.com/pr-123/blue-lobster/screenshot.png",
      "contentType": "image/png",
      "size": 12345,
      "sha256": "..."
    }
  ]
}
```

Inspect or fetch that manifest later:

```sh
crabbox artifacts list artifacts/blue-lobster/artifact-manifest.json
crabbox artifacts list artifacts/blue-lobster --json
crabbox artifacts pull artifacts/blue-lobster/artifact-manifest.json --output /tmp/blue-lobster-proof
```

`list` accepts a manifest file or a bundle directory and prints one line per
file (kind, name, size, sha256, content type, access policy, location), or the
raw manifest with `--json`.

`pull` requires `--output` and downloads URLs or copies local manifest paths,
preserving nested artifact names and verifying SHA256 and size when the manifest
provides them. Existing output files are rejected unless `--overwrite` is set.

```text
crabbox artifacts list [<manifest-or-dir>] [--json]
crabbox artifacts pull [<manifest-or-dir>] --output <dir> [--overwrite] [--json]
```

## Coordinator artifact backend

When publishing through a coordinator, configure the artifact backend in its
runtime environment. These values split into non-secret settings and secrets.

Coordinator settings (where artifacts go and how long URLs live):

```text
CRABBOX_ARTIFACTS_BACKEND=s3|r2
CRABBOX_ARTIFACTS_BUCKET
CRABBOX_ARTIFACTS_PREFIX
CRABBOX_ARTIFACTS_BASE_URL
CRABBOX_ARTIFACTS_REGION
CRABBOX_ARTIFACTS_ENDPOINT_URL
CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS
CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS
```

Coordinator secrets (S3-compatible object-store keys used only by the coordinator to
sign upload/read URLs):

```text
CRABBOX_ARTIFACTS_ACCESS_KEY_ID
CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY
CRABBOX_ARTIFACTS_SESSION_TOKEN
```

These secrets are not required on developer machines for normal
`crabbox artifacts publish`.

## Environment defaults

`publish` flags default from these client-side variables:

```text
CRABBOX_ARTIFACTS_DIR
CRABBOX_ARTIFACTS_STORAGE
CRABBOX_ARTIFACTS_BUCKET
CRABBOX_ARTIFACTS_PREFIX
CRABBOX_ARTIFACTS_BASE_URL
CRABBOX_ARTIFACTS_AWS_REGION
CRABBOX_ARTIFACTS_AWS_PROFILE
CRABBOX_ARTIFACTS_ENDPOINT_URL
CRABBOX_ARTIFACTS_S3_ACL
CRABBOX_ARTIFACTS_PRESIGN
CRABBOX_ARTIFACTS_EXPIRES
```

For S3-compatible R2 publishing, `CRABBOX_ARTIFACTS_R2_ENDPOINT_URL`,
`CRABBOX_ARTIFACTS_R2_AWS_PROFILE`, and `CRABBOX_ARTIFACTS_R2_AWS_REGION`
override the generic AWS defaults when `--storage r2` is used and the matching
flag is not passed.
