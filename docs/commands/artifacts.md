# artifacts

`crabbox artifacts` collects desktop QA evidence into a durable bundle, creates
trimmed review media, and publishes inline-ready assets for pull requests.

Use it when a desktop/WebVNC issue or UI fix needs more than a one-off
screenshot: MP4 recording, trimmed GIF, logs, doctor output, WebVNC status, and
metadata in one directory.

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

`--all` also records `screen.mp4`, writes `screen.contact.png`, creates
`screen.trimmed.gif`, and writes `screen.trimmed.mp4` using the same motion
window. Linux video uses remote `ffmpeg`/X11 capture. Native Windows video
captures frames in the interactive console session and encodes the MP4 locally
with `ffmpeg`. MP4 capture is currently supported for Linux and native Windows
desktop targets.

Useful flags:

```text
--id <lease-id-or-slug>
--output <dir>
--run <run-id>
--all
--screenshot
--video
--gif
--doctor
--webvnc-status
--metadata
--duration <duration> default 10s
--fps <n>             default 15
--gif-width <px>      default 1000
--gif-fps <n>         default 24
--contact-sheet       default true
--no-contact-sheet
--contact-sheet-output <path>
--contact-sheet-frames <n> default 5
--contact-sheet-cols <n>   default 5
--contact-sheet-width <px> default 320
--provider <name>
--network auto|public|tailscale
--json
```

When collection hits an unhealthy desktop, WebVNC, VNC, or input layer, it
prints the same inline `problem:`, `detail:`, and `rescue:` commands used by the
desktop and WebVNC commands. With `--json`, stdout remains valid JSON and those
same repair hints are returned in the `warnings` array instead of being printed
as text before the JSON document. If a capture step fails after the bundle has
started, the command still exits nonzero and includes an `error` object with a
stable code and message.

## Video

```sh
crabbox artifacts video --id blue-lobster --duration 15s --output screen.mp4
```

`video` records an MP4 from a desktop lease and writes a sampled
`*.contact.png` contact sheet beside it by default. It is useful when you want
to keep capture separate from bundle collection. Disable the sidecar with
`--contact-sheet=false` or `--no-contact-sheet`, or set
`--contact-sheet-output <path>`. Like `collect --video`, it supports Linux and
native Windows desktop targets.

## GIF

```sh
crabbox artifacts gif \
  --input screen.mp4 \
  --output screen.trimmed.gif \
  --trimmed-video-output screen.trimmed.mp4
```

`gif` is an alias for the same local motion-trimmed preview logic as
[`crabbox media preview`](media.md).

## Templates

```sh
crabbox artifacts template openclaw \
  --before before.png \
  --after after.gif \
  --summary "Login modal no longer overlaps the toolbar." \
  --output summary.md

crabbox artifacts template mantis --summary-file qa-notes.md
```

Templates write Markdown with `Summary`, `Before / After`, and `Evidence`
sections sized for Mantis/OpenClaw QA comments.

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

`publish` uploads bundle files, writes `published-artifacts.md`, and comments
on the PR with inline images/GIFs plus links to videos, logs, and metadata.
Use `--dry-run` to generate markdown and print intended actions without upload
or comment side effects.

Storage backends:

- `--storage auto` is the default. When a coordinator is configured, Crabbox
  asks the broker for upload URLs and the broker-owned artifact backend handles
  storage credentials. Without a coordinator, auto falls back to local markdown.
- `--storage broker` requires a configured coordinator and uploads through
  broker-minted URLs.
- `--storage s3` uses the AWS CLI and uploads to `s3://<bucket>/<prefix>/...`.
- `--storage cloudflare` uses `wrangler r2 object put --remote`.
- `--storage r2` uses the AWS CLI against an S3-compatible R2 endpoint.
- `--storage local` writes markdown only. For `--pr`, local publishing needs a
  `--base-url` that already serves the files, otherwise the PR would contain
  unusable local paths.

S3 flags:

```text
--bucket <name>
--prefix <path>
--base-url <url>
--region <region>
--profile <profile>
--endpoint-url <url>
--acl <acl>
--presign
--expires <duration> default 168h
```

When `--base-url` is supplied, published links use that public URL. Otherwise
`--presign` generates temporary AWS/R2 S3 URLs after upload.

Cloudflare R2 flags:

```text
--bucket <name>
--prefix <path>
--base-url <url>     required for --pr inline-ready links
```

For native Cloudflare publishing, `publish` runs `wrangler` with
`CRABBOX_ARTIFACTS_CLOUDFLARE_*` when present, then the generic
`CLOUDFLARE_*` environment. Prefer brokered publishing for shared teams so
Cloudflare and object-store secrets stay on the coordinator.

For S3-compatible R2 publishing, pass `--storage r2 --endpoint-url <r2-endpoint>
--profile <r2-profile>`. When present, Crabbox uses
`CRABBOX_ARTIFACTS_R2_ENDPOINT_URL` and `CRABBOX_ARTIFACTS_R2_AWS_PROFILE`
before falling back to generic AWS defaults.

Coordinator artifact backend configuration:

```text
CRABBOX_ARTIFACTS_BACKEND=s3|r2
CRABBOX_ARTIFACTS_BUCKET
CRABBOX_ARTIFACTS_PREFIX
CRABBOX_ARTIFACTS_BASE_URL
CRABBOX_ARTIFACTS_REGION
CRABBOX_ARTIFACTS_ENDPOINT_URL
CRABBOX_ARTIFACTS_ACCESS_KEY_ID
CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY
CRABBOX_ARTIFACTS_SESSION_TOKEN
CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS
CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS
```

For brokered publishing, the CLI never receives object-store credentials. It
sends artifact names, sizes, content types, and hashes to
`POST /v1/artifacts/uploads`; the coordinator returns one short-lived upload URL
per file plus the final URL to place in Markdown. Upload grants are signed with
the declared `content-length`, so the object store rejects oversized PUTs during
the grant window; the broker also caps each upload request at 5 GiB total before
signing grants. When `--prefix` is omitted for hosted publishing, the CLI derives
a unique prefix from the PR number, bundle directory, and current time so later
QA comments do not overwrite earlier evidence.

Coordinator artifact values split into two groups:

- Worker vars: `CRABBOX_ARTIFACTS_BACKEND`, `CRABBOX_ARTIFACTS_BUCKET`,
  `CRABBOX_ARTIFACTS_PREFIX`, `CRABBOX_ARTIFACTS_BASE_URL`,
  `CRABBOX_ARTIFACTS_REGION`, `CRABBOX_ARTIFACTS_ENDPOINT_URL`,
  `CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS`, and
  `CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS`. These describe where artifacts go and
  how long URLs should live.
- Worker secrets: `CRABBOX_ARTIFACTS_ACCESS_KEY_ID`,
  `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`, and optional
  `CRABBOX_ARTIFACTS_SESSION_TOKEN`. These are S3-compatible object-store keys
  used only by the coordinator to sign artifact upload/read URLs.

Our deployed coordinator currently uses R2-compatible storage with public final
URLs on `https://artifacts.openclaw.ai`, bucket
`openclaw-crabbox-artifacts`, and object prefix `crabbox-artifacts`. The actual
R2 access key id and secret access key are Worker secrets; they are not required
on developer machines for normal `crabbox artifacts publish`.

Environment defaults:

```text
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

`publish --pr` uses `gh issue comment <pr> --body-file ...`, so the current
checkout must be authenticated with GitHub. Pass `--repo owner/name` when the
working directory is not inside the target repository.
