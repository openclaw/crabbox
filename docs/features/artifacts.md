# Artifacts

Read when:

- collecting screenshots, videos, logs, or metadata from a desktop lease;
- turning a desktop recording into a trimmed GIF;
- publishing QA proof into a GitHub PR;
- deciding whether AWS S3 or Cloudflare R2 should host inline assets.

Crabbox artifacts are a local bundle plus optional hosted URLs. The command is
designed for QA handoff: capture the state of a lease, preserve enough metadata
to reproduce what happened, and publish a concise before/after/summary comment.

`crabbox run` can also create run-scoped artifacts directly. Repeat
`--artifact-glob <glob>` to archive matching files from the remote workdir after
a successful SSH-backed command. Profile and preset `artifactGlobs` use the same
collector. The local tarball lands under `.crabbox/runs/<run-or-lease>/` and is
listed in the final run details and `--timing-json` artifact array. Native
Windows and macOS targets reject this collector; use Linux or Windows WSL2.

`--emit-proof <path>` renders proof as a derived artifact after a successful
run. The proof block uses the selected profile proof template, expanded command,
run metadata, copied live console output, artifact paths, and Actions URL when
available. This keeps PR-ready evidence next to the raw logs and test reports
that back it.

## Bundle Contract

`crabbox artifacts collect --id <lease>` writes a directory such as
`artifacts/blue-lobster` with:

- `metadata.json`: Crabbox version, lease id, slug, provider, network, target,
  run id when provided, and capture time.
- `screenshot.png`: a desktop screenshot captured through the managed VNC
  boundary.
- `doctor.txt`: the same desktop/session checks as `crabbox desktop doctor`.
- `webvnc-status.json`: bridge/viewer status when the lease is coordinator
  backed.
- `logs.txt` and `run.json`: retained run output and run metadata when
  `--run <run-id>` is set.
- `screen.mp4`, `screen.contact.png`, `screen.trimmed.gif`, and
  `screen.trimmed.mp4` when video/GIF capture is requested.

Failures keep the rescue-first UX. If the input stack is dead, the VNC bridge
is disconnected, the browser did not launch, or screenshot/video capture fails,
the command prints a concrete `problem:` plus exact `rescue:` commands before
returning. In `--json` mode those hints are kept in `warnings`, stdout remains
parseable JSON, and post-start capture failures add an `error` object while
still returning a nonzero exit code.

## Media

Video capture is intentionally desktop-session scoped. Linux leases record the
X11 desktop with remote `ffmpeg` and stream the MP4 back over SSH. Native
Windows leases capture a frame sequence inside the interactive console session,
stream that archive back, and encode the MP4 locally with `ffmpeg`. MP4 capture
currently supports Linux and native Windows desktop targets; macOS desktop
flows still support launch, screenshot, VNC, and input capture paths, but not
recording. MP4 capture also writes a contact sheet by default: one PNG grid
sampled across the video for quick PR review. GIF generation reuses the local
motion-trimming logic from `crabbox media preview`: leading/trailing static
regions are removed and an optional trimmed MP4 can be emitted beside the GIF.

`crabbox desktop proof` produces the same bundle shape for visual terminal
smokes without a separate collect step: metadata, screenshot, recorder
diagnostics, MP4, and contact sheet. `desktop proof --publish-pr <n>` publishes
that bundle through the artifact backend immediately.

Use `desktop launch --fullscreen` only when the artifact should show a
browser-only capture. The standard human QA profile remains windowed so panel
and window chrome stay visible.

## Publishing

GitHub comments cannot directly upload arbitrary local files through the issue
comment API. `crabbox artifacts publish --pr <n>` therefore uploads files to a
storage backend first, renders Markdown with inline image/GIF links, writes the
same body to `published-artifacts.md`, and posts that body with `gh`.

Supported storage:

- Brokered coordinator publishing through `crabbox artifacts publish` with no
  storage flags. The coordinator owns object-store credentials and returns
  short-lived upload URLs plus final public URLs.
- AWS S3 through the `aws` CLI.
- Cloudflare R2 through `wrangler r2 object put`.
- Local/hosted mode through `--storage local --base-url <url>` when another
  process already serves the bundle.

For AWS S3, use either public/custom-domain URLs through `--base-url` or
temporary links through `--presign --expires <duration>`. For Cloudflare R2,
provide a public bucket/custom-domain `--base-url` when publishing to a PR;
without it, the upload can succeed but the PR would only have `r2://` object
identifiers, not inline-ready links.

## Broker Secret Model

Brokered publishing is intentionally asymmetric. Local users and agents only
need normal Crabbox coordinator auth. The coordinator holds the storage keys and
uses them to sign one upload request per artifact. Each upload grant includes a
signed `content-length`, so the configured size cap is enforced by the storage
backend, not only by the request metadata. The broker enforces both a 1 GiB
per-file cap and a 5 GiB per-request aggregate cap before minting upload URLs.
When users do not pass `--prefix`, hosted publishing adds a unique
PR/bundle/timestamp prefix so later artifact bundles cannot overwrite links from
earlier QA comments.

Coordinator artifact vars describe the backend:

- `CRABBOX_ARTIFACTS_BACKEND`: `s3` or `r2`.
- `CRABBOX_ARTIFACTS_BUCKET`: destination bucket.
- `CRABBOX_ARTIFACTS_PREFIX`: root object prefix for all brokered uploads.
- `CRABBOX_ARTIFACTS_BASE_URL`: public URL prefix for final Markdown links.
- `CRABBOX_ARTIFACTS_REGION` and `CRABBOX_ARTIFACTS_ENDPOINT_URL`: S3/R2 signing
  endpoint details.
- `CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS`: lifetime for write grants.
- `CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS`: lifetime for signed read URLs when
  no public base URL is configured.

Coordinator artifact secrets authorize signing:

- `CRABBOX_ARTIFACTS_ACCESS_KEY_ID`
- `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`
- `CRABBOX_ARTIFACTS_SESSION_TOKEN` when the backend uses temporary
  credentials.

These keys are object-store credentials, not Crabbox provider credentials. They
should be scoped to the artifact bucket/prefix and should not grant Worker
deployment, Cloudflare account administration, lease creation, or cloud VM
provider access. The CLI receives only pre-signed URLs and final asset URLs.

## Templates

`crabbox artifacts template openclaw` and `crabbox artifacts template mantis`
produce Markdown with:

- `Summary`
- `Before / After`
- `Evidence`

The publish command uses the same layout, so local preview and PR comments stay
consistent.
