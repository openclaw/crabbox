# Freestyle Provider

Read when:

- choosing `provider: freestyle`;
- configuring Freestyle VM size or workdir;
- changing `internal/providers/freestyle`.

Freestyle is a delegated run provider. Crabbox uses the [Freestyle](https://freestyle.sh)
v1 REST API (`https://api.freestyle.sh`) for VM lifecycle, archive sync, and
command execution through pure Go `net/http` calls. Freestyle owns VM state and
exec transport; Crabbox owns local config, repo claims, sync manifests and
guardrails, slugs, timing summaries, and normalized list/status rendering.

## When To Use

Use Freestyle when the remote Linux VM should be owned by Freestyle and commands
can run through Freestyle's exec API. Use AWS, Hetzner, Static SSH, or Daytona
when you need Crabbox SSH access.

## Commands

```sh
crabbox warmup --provider freestyle --keep
crabbox run --provider freestyle -- pnpm test
crabbox run --provider freestyle --sync-only
crabbox run --provider freestyle --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider freestyle --id blue-lobster
crabbox stop --provider freestyle blue-lobster
crabbox list --provider freestyle
crabbox doctor --provider freestyle
```

## Live Smoke

Use a live smoke when changing Freestyle lifecycle, sync, or exec code. Keep the
API key in `FREESTYLE_API_KEY`; do not pass it as a command-line argument.

```sh
export FREESTYLE_API_KEY=...
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox run --provider freestyle --keep 'bash test.sh'
bin/crabbox warmup --provider freestyle --actions-runner   # expect exit 2
bin/crabbox doctor --provider freestyle
```

Expected results:

- `run` prints a Freestyle lease (`fsb_...`), sync summary, command output, and
  `exit=0`.
- `warmup --actions-runner` is rejected because Freestyle does not register
  GitHub Actions runners.
- `doctor` reports inventory readiness when auth and list calls succeed.

## Auth

```sh
export FREESTYLE_API_KEY=...
```

`CRABBOX_FREESTYLE_API_KEY` is also accepted and wins over `FREESTYLE_API_KEY`.

Freestyle API keys must not be passed as CLI flags. Crabbox reads them from
environment variables only.

Provider error diagnostics redact the configured API key before display.

`FREESTYLE_API_URL`, `CRABBOX_FREESTYLE_API_URL`, or `freestyle.apiUrl` in the
user config can override the default `https://api.freestyle.sh`. Repository
config cannot override this credential destination. Custom endpoints must use
HTTPS, except for loopback development URLs. Crabbox refuses cross-origin
redirects so the bearer token cannot be forwarded to another origin.

## Config

```yaml
provider: freestyle
target: linux
freestyle:
  apiUrl: https://api.freestyle.sh
  workdir: crabbox
  # Optional. Omit both to use the Freestyle plan defaults
  # (4 vCPU / 8 GiB / 20 GB). vcpus and memoryGB must each be a
  # power of two. Setting them on a plan without custom-sizing
  # entitlement fails with CUSTOM_SIZING_NOT_ALLOWED.
  vcpus: 4
  memoryGB: 8
```

Provider flags:

```text
--freestyle-api-url
--freestyle-workdir
--freestyle-vcpus
--freestyle-memory-gb
```

`--freestyle-workdir` / `freestyle.workdir` is interpreted as a relative
directory below `/workspace`. Crabbox rejects absolute paths and `..` escapes
before workspace preparation and sync.

## Lifecycle

1. Create or resolve a Crabbox-owned Freestyle VM.
2. Store a local lease ID with the `fsb_` prefix and a friendly slug.
3. Validate the Freestyle workdir, build the Crabbox sync manifest, and upload a
   gzipped archive into `/workspace/<freestyle.workdir>`.
4. Execute commands through Freestyle's exec API in that workdir via `bash -lc`.
5. Stop deletes the VM and removes the local lease claim.

## Sync

Freestyle advertises archive sync (`FeatureArchiveSync`). Crabbox supports
`--sync-only`, `--force-sync-large`, and `--no-sync`.

Crabbox creates Freestyle VMs with an explicit empty external-port list. No
guest service is publicly exposed by default.

When `sync.delete` is enabled, Crabbox extracts into a staging directory and
replaces the retained workspace only after extraction succeeds. Failed uploads
leave the prior workspace intact. `sync.timeout` bounds archive creation,
upload, extraction, and replacement.

Archive upload tries Freestyle's file API first. When that endpoint is
unavailable, Crabbox falls back to chunked base64 upload through exec. Sync still
completes reliably through the fallback path.

The raw VM ID, Crabbox VM name, and generated slug shown by `crabbox list` can
recover a Crabbox-owned VM after local claim state is lost.

`--checksum` is not supported because Freestyle uses archive sync rather than
Crabbox rsync checksum mode.

## Limitations

- `--actions-runner` is not supported for warmup or run.
- `--checksum` is not supported.
- No SSH lease path; this is delegated run only.
- API keys are env-only; there is no `--freestyle-api-key` flag.

## Doctor

`crabbox doctor --provider freestyle` checks auth and lists Crabbox-owned VMs in
the Freestyle account. It does not create a VM during the check.
