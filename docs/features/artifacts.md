# Artifacts

Read this when you need to:

- collect screenshots, video, logs, or metadata from a desktop lease;
- turn a desktop recording into a trimmed GIF or contact sheet;
- publish QA proof into a GitHub pull request comment;
- choose where inline-ready assets are hosted (broker, AWS S3, or Cloudflare R2).

A Crabbox artifact bundle is a local directory of files plus an optional set of
hosted URLs. The workflow is built for QA handoff: capture the state of a lease,
keep enough metadata to explain what happened, and publish a concise
summary comment with inline-ready images.

## Run-scoped artifacts

`crabbox run` can also produce artifacts directly, without a separate collect
step. Repeat `--artifact-glob <glob>` to archive matching files from the remote
workdir after a successful SSH-backed command; profile and preset `artifactGlobs`
feed the same collector. The local tarball lands under
`.crabbox/runs/<run-or-lease>/` and is listed in the final run details and in
the `--timing-json` artifact array. Native Windows and macOS targets reject this
collector; use Linux or Windows WSL2.

Repeat `--require-artifact <glob>` when the run should fail unless a proof file,
manifest, or report exists after the command exits successfully. Required
artifacts use the same safe relative glob syntax and target limits as
`--artifact-glob`; each required glob must resolve to at least one regular file.
A symlink counts only when its target is a regular file; dangling symlinks and
symlinks to directories do not satisfy the proof gate or enter artifact archives.
Crabbox checks required artifacts before local `--download` outputs, then
includes required artifacts in the run artifact tarball. Callers can pair
`--require-artifact reports/data/manifest.json` with a broader
`--artifact-glob reports/data/**`.

`--require-artifact` is an existence guard, not manifest validation or a data
safety scanner. Keep required artifacts bounded and scrubbed, such as manifests,
summaries, screenshots, or QA reports. Do not use run artifacts for raw datasets,
secrets, credentials, signed URLs, or unredacted customer rows.

Delegated providers reject run artifact collection until they grow an explicit
bounded artifact capability. Archive-capable adapters may validate and collect
required artifacts and artifact globs. Download-capable adapters may materialize
safe relative single-file `--download` outputs capped at 64 KiB.

`--emit-proof <path>` renders proof as a derived artifact after a successful
run. The proof block uses the selected profile's proof template, the expanded
command, run metadata, copied live console output, artifact paths, and the
GitHub Actions URL when available, keeping PR-ready evidence next to the raw
logs and test reports that back it.

## `artifacts collect`

`crabbox artifacts collect --id <lease-id-or-slug>` writes a bundle directory.
By default it lands in a private `artifacts/<slug>` directory. An explicit
`--output <dir>` preserves that directory's sharing permissions, while retained
`logs.txt` and `run.json` remain owner-only. Publishing still uploads the
selected files normally. The bundle contains:

- `metadata.json`: Crabbox version, lease id, slug, provider, network, target
  OS, optional run id, and capture time.
- `screenshot.png`: a desktop screenshot through the managed VNC boundary
  (on by default; `--screenshot=false` to skip).
- `doctor.txt`: the same desktop/session checks as `crabbox desktop doctor`.
- `webvnc-status.json`: WebVNC bridge/viewer status, written only when the lease
  is coordinator-backed and not a static or Blacksmith provider.
- `logs.txt` and `run.json`: retained run output and run metadata, written only
  when `--run <run-id>` is set.
- `screen.mp4`, `screen.contact.png`, `screen.trimmed.gif`, and
  `screen.trimmed.mp4` when video/GIF capture is requested.

Capture is opt-in beyond the defaults: pass `--video` to record, `--gif` to
derive a trimmed GIF (requires `--video`), or `--all` to enable both. Tune
recording with `--duration` (default 10s), `--fps` (default 15), `--gif-width`,
and `--gif-fps`. A contact sheet is generated next to recorded video by default
(`--contact-sheet`, `--no-contact-sheet`), sized with `--contact-sheet-frames`,
`--contact-sheet-cols`, and `--contact-sheet-width`.

`artifacts collect` is not supported on Blacksmith leases (Blacksmith owns
machine connectivity), and desktop artifacts are not collected from static SSH
hosts on non-Linux targets, since those are existing machines rather than
Crabbox-created desktops.

### Failure behavior

The command is rescue-first. If the input stack is dead, the VNC bridge is
disconnected, the browser did not launch, or screenshot/video capture fails, it
prints a concrete `problem:` line plus exact `rescue:` commands before
returning. In `--json` mode those hints stay in `warnings`, stdout remains
parseable JSON, and a post-start capture failure adds an `error` object while
the command still exits nonzero.

## Media capture

Video capture is desktop-session scoped:

- Linux leases record the X11 desktop with remote `ffmpeg` (`x11grab`) and
  stream the MP4 back over SSH. Wayland desktop environments are not yet
  supported for recording.
- Native Windows leases capture a frame sequence in the interactive console
  session, stream the archive back, and encode the MP4 locally with `ffmpeg`
  (which must be installed locally).
- macOS desktop targets support launch, screenshot, VNC, and input paths, but
  not recording.

MP4 capture also writes a contact sheet by default: one PNG grid sampled across
the video for quick review. GIF generation reuses the motion-trimming logic from
`crabbox media preview` — leading/trailing static regions are removed, and an
optional trimmed MP4 is emitted beside the GIF.

`crabbox artifacts video` records an MP4 (with a contact sheet) from a desktop
lease as a standalone step, and `crabbox artifacts gif` is an alias for
`crabbox media preview`. `crabbox desktop proof` produces the same bundle shape
for visual terminal smokes without a separate collect step, and
`desktop proof --publish-pr <n>` publishes that bundle through the artifact
backend immediately. See [capabilities](capabilities.md) for the `--desktop`
session that backs all of these.

## Publishing

The GitHub issue-comment API cannot upload arbitrary local files, so
`crabbox artifacts publish --pr <n>` uploads files to a storage backend first,
renders Markdown with inline image/GIF links, writes that body to
`published-artifacts.md`, writes and publishes `artifact-manifest.json`, and
posts the body with `gh`.

`--dir <bundle>` is required. Publishing rejects symlinks and other non-regular
bundle entries before upload, manifest, or Markdown side effects. Storage
backends (`--storage`):

- `auto` (default): use brokered publishing when a coordinator with a token is
  configured, otherwise fall back to `local`.
- `broker`: upload through the coordinator, which owns the object-store
  credentials and returns short-lived upload URLs plus final asset URLs.
- `s3`: upload through the `aws` CLI (requires `--bucket`).
- `cloudflare`: upload through `wrangler r2 object put` (requires `--bucket`).
- `r2`: upload through the `aws` CLI against an R2 endpoint (requires `--bucket`
  and `--endpoint-url`).
- `local`: only record local paths; pair with `--base-url <url>` when another
  process already serves the bundle.

For S3, use a public/custom-domain URL via `--base-url`, or temporary links via
`--presign --expires <duration>` (default 7d). For `cloudflare`/`r2`, a public
bucket/custom-domain `--base-url` is required when commenting on a PR; without
it the upload can succeed but the comment would only carry `r2://` object
identifiers, not inline-ready links.

Commenting requires either brokered publishing, `--storage s3|r2|cloudflare`,
or `--base-url` for already-hosted local assets. Use `--dry-run` to print the
upload/comment commands without running them, `--no-comment` to skip the PR
comment, and `--repo <owner/name>` to target a repository other than the
current one.

### Manifest, list, and pull

The manifest (`artifact-manifest.json`) is written by default and is the durable
handoff object: schema version, generated time, backend/bucket/prefix/base URL,
and one entry per file with kind, name, URL/key, content type, size, SHA256,
optional expiry, and access policy.

```sh
crabbox artifacts publish --dir artifacts/blue-lobster --storage cloudflare \
  --bucket qa-artifacts --base-url https://artifacts.example.com --no-comment
crabbox artifacts list https://artifacts.example.com/runs/abc/artifact-manifest.json
crabbox artifacts pull https://artifacts.example.com/runs/abc/artifact-manifest.json \
  --output /tmp/blue-lobster-proof
```

`artifacts pull` verifies SHA256 and size before reporting success and refuses
path-escaping or symlinked entries. Remote manifest fetches, artifact downloads,
and brokered uploads follow redirects only when the scheme, hostname, and
effective port remain unchanged. Use `--skip-manifest` (alias `--no-manifest`)
only for legacy Markdown-only output.

## Brokered publishing and the broker secret model

Brokered publishing is intentionally asymmetric. Local users and agents only
need normal Crabbox coordinator auth; the coordinator holds the storage keys and
signs one upload request per artifact. Each upload grant carries a signed
`content-length`, so the size cap is enforced by the storage backend, not just
by request metadata — the CLI verifies the file size still matches the grant
before uploading. The broker enforces a 1 GiB per-file cap and a 5 GiB
per-request aggregate cap before minting upload URLs. When you do not pass
`--prefix`, hosted publishing adds a unique PR/bundle/timestamp prefix so later
bundles cannot overwrite links from earlier QA comments.

New brokered grants use a versioned object namespace containing opaque encodings
of the authenticated organization and owner before the caller prefix. Existing
object URLs remain unchanged; the coordinator does not dual-write or translate
legacy keys.

Coordinator artifact variables describe the backend:

- `CRABBOX_ARTIFACTS_BACKEND`: `s3` or `r2`.
- `CRABBOX_ARTIFACTS_BUCKET`: destination bucket.
- `CRABBOX_ARTIFACTS_PREFIX`: root object prefix for all brokered uploads
  (default `crabbox-artifacts`).
- `CRABBOX_ARTIFACTS_BASE_URL`: public URL prefix for final Markdown links.
- `CRABBOX_ARTIFACTS_REGION` and `CRABBOX_ARTIFACTS_ENDPOINT_URL`: S3/R2 signing
  endpoint details (region defaults to `auto` for r2, `us-east-1` for s3;
  r2 requires the endpoint URL).
- `CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS`: lifetime for write grants.
- `CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS`: lifetime for signed read URLs when no
  public base URL is configured.

Coordinator artifact secrets authorize signing:

- `CRABBOX_ARTIFACTS_ACCESS_KEY_ID`
- `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`
- `CRABBOX_ARTIFACTS_SESSION_TOKEN` when the backend uses temporary credentials.

These are object-store credentials, not Crabbox provider credentials. Scope them
to the artifact bucket/prefix; they should not grant Worker deployment,
Cloudflare account administration, lease creation, or cloud VM provider access.
The CLI only ever receives pre-signed upload URLs and final asset URLs.

The CLI also reads `CRABBOX_ARTIFACTS_*` environment variables as defaults for
the matching `artifacts publish` flags (for example `CRABBOX_ARTIFACTS_DIR`,
`CRABBOX_ARTIFACTS_STORAGE`, `CRABBOX_ARTIFACTS_BUCKET`,
`CRABBOX_ARTIFACTS_BASE_URL`, `CRABBOX_ARTIFACTS_PRESIGN`,
`CRABBOX_ARTIFACTS_EXPIRES`), so a configured environment can publish with a
bare `crabbox artifacts publish --dir <bundle> --pr <n>`.

## Templates

`crabbox artifacts template openclaw` and `crabbox artifacts template mantis`
write QA summary Markdown with these sections:

- `Summary`
- `Before / After`
- `Evidence`

`artifacts publish` renders the same layout (with `--template openclaw|mantis`),
so local preview and PR comments stay consistent. Supply prose with `--summary`
or `--summary-file`, and inline before/after assets with `--before` and
`--after`.
