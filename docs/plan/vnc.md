# Interactive Desktop, VNC, And Browser Plan

Read when:

- implementing `--desktop`, `--browser`, or `crabbox vnc`;
- changing Linux UI bootstrap or browser provisioning;
- deciding how static macOS/Windows hosts participate in interactive QA;
- reviewing the security boundary for desktop takeover.

## Goal

Implement the first real Crabbox interactive-desktop vertical slice so
Mantis/OpenClaw can request a UI-capable machine, run browser automation in a
visible session, and let Peter take over through a tunnel.

Crabbox owns machine capability:

- lease lifecycle, TTL, idle touch, cleanup, and claims;
- provider-specific bootstrap and SSH connection details;
- desktop services, browser installation/probing, and connection metadata;
- tunnel-only VNC instructions.

Mantis/OpenClaw own scenario logic:

- Discord or app credentials;
- browser profiles, Playwright/Selenium scripts, assertions, screenshots, and
  videos;
- PR comments, artifacts, and pass/fail reporting.

## Capability Flags

Use two explicit capability flags:

```sh
crabbox warmup --desktop
crabbox warmup --desktop --browser
crabbox run --desktop --browser -- <command...>
```

`--desktop` means the lease should expose a visible UI session and takeover
path. On managed Linux this provisions desktop/VNC services. On static targets
it probes existing operator-managed services.

`--browser` means the target should have a known browser binary for automation.
It is separate because browser installation is heavier and more provider/OS
specific than a basic display session.

For `run`, `--browser` never implies `--desktop`. It supports headless browser
automation on a machine with a known browser binary. Use `--desktop --browser`
only when the browser should run in the visible VNC session.

Store both capabilities on leases:

```json
{
  "desktop": true,
  "browser": true
}
```

Provider labels/tags should include:

```text
desktop=true
browser=true
```

## CLI Surface

Add:

```sh
crabbox warmup --desktop [--browser]
crabbox run --desktop [--browser] -- <command...>
crabbox vnc --id <lease-or-slug>
```

`crabbox vnc` should resolve a lease like `crabbox ssh`, claim/touch it like
manual use, and print a concise connection block:

```text
lease: cbx_... slug=blue-lobster provider=aws target=linux
display: :99
ssh tunnel:
  ssh -i ... -p 2222 -N -L 5901:127.0.0.1:5900 crabbox@203.0.113.10
vnc:
  localhost:5901

Keep the tunnel process running while connected.
```

JSON output can come later. Text output is enough for v0.

If noVNC is implemented later, extend the block with a local browser URL. Do
not implement public noVNC in this slice.

## Security Boundary

Hard requirements:

- never expose VNC/noVNC to the public internet;
- bind runner-side VNC to `127.0.0.1`;
- do not add provider firewall/security-group ingress for VNC;
- print SSH tunnel commands only;
- do not put VNC passwords in command-line arguments, provider labels, run
  history, or logs;
- keep TTL and idle-timeout behavior unchanged;
- cleanup remains VM deletion or static-host no-op, as today.

For Linux v0, use loopback-bound x11vnc with a per-lease password:

```sh
x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass
```

Generate a per-lease remote password file, do not log it, and have
`crabbox vnc` retrieve and print it only when needed.

## Managed Linux Bootstrap

Default bootstrap must remain tiny. Desktop/browser packages are installed only
when requested.

### `--desktop`

Install the smallest useful visible-session stack:

```text
xvfb
openbox
x11vnc
xauth
dbus-x11
fonts-dejavu
fonts-liberation
ca-certificates
```

Use systemd units so the desktop survives command boundaries on kept leases:

- `crabbox-xvfb.service`
- `crabbox-openbox.service`
- `crabbox-x11vnc.service`

Suggested unit behavior:

```text
crabbox-xvfb:
  Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac

crabbox-openbox:
  DISPLAY=:99 openbox

crabbox-x11vnc:
  x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -nopw
```

`crabbox-ready` should check desktop readiness only when `desktop=true`:

```sh
systemctl is-active --quiet crabbox-xvfb.service
systemctl is-active --quiet crabbox-openbox.service
systemctl is-active --quiet crabbox-x11vnc.service
ss -ltn | grep -q '127.0.0.1:5900'
```

Normal non-desktop leases must not run these checks.

### `--browser`

Browser support should be opt-in.

For managed Linux, install Chrome stable if feasible and fall back to Chromium
when the distro package path is available. Prefer Chrome stable over Ubuntu
`chromium-browser` because Ubuntu Chromium commonly routes through Snap, which
is awkward in minimal cloud images, but a verified Chromium fallback is
acceptable.

Preferred managed Linux path:

1. install Google signing key into `/etc/apt/keyrings`;
2. add the Chrome apt source;
3. install `google-chrome-stable`;
4. write a small metadata file with the discovered browser path.

Example metadata:

```text
/var/lib/crabbox/browser.env
```

Content:

```sh
CHROME_BIN=/usr/bin/google-chrome
BROWSER=/usr/bin/google-chrome
```

`crabbox-ready` should check the browser only when `browser=true`:

```sh
test -x /usr/bin/google-chrome
/usr/bin/google-chrome --version
```

## Runtime Environment

When `run --desktop` executes on a Linux desktop-capable target, inject:

```sh
DISPLAY=:99
CRABBOX_DESKTOP=1
```

When `run --desktop --browser` knows a browser path, also inject:

```sh
CRABBOX_BROWSER=1
CHROME_BIN=/usr/bin/google-chrome
BROWSER=/usr/bin/google-chrome
```

This should merge with the existing allowed-env and Actions env-file behavior.
Do not leak secrets; these values are static machine metadata.

If `--desktop` is requested against an existing lease that was not provisioned
with `desktop=true`, fail clearly before running:

```text
lease cbx_... was not created with desktop=true; warm a new lease with --desktop
```

Static Linux can instead probe services and proceed if they are already present.

## Provider Behavior

### Brokered AWS/Hetzner

Support both `--desktop` and `--browser`.

Flow:

1. CLI sends `desktop` and `browser` in the lease request.
2. Worker validates Linux-only target as today.
3. Worker stores both booleans on `LeaseRecord`.
4. Worker labels/tags cloud machines with `desktop` and `browser`.
5. Worker cloud-init appends optional desktop/browser bootstrap blocks.
6. CLI receives the booleans back from `CoordinatorLease`.
7. `run` and `vnc` enforce/probe the capability before use.

Do not change AWS security group ingress. SSH remains the only public ingress.

### Direct AWS/Hetzner

Support both `--desktop` and `--browser` with the same optional cloud-init path
as the Worker.

Direct labels should include the booleans so `findLease` can detect whether an
existing lease is desktop/browser-capable.

### Static Linux

Support `crabbox vnc` if services already exist. Do not install packages on
static hosts in v0.

Probe:

```sh
test "${DISPLAY:-:99}" = ":99" || true
pgrep -f 'Xvfb :99'
pgrep -f x11vnc
ss -ltn | grep -q '127.0.0.1:5900'
```

For browser:

```sh
command -v google-chrome || command -v chromium || command -v chromium-browser
```

If missing, fail with clear operator instructions.

### Static macOS

Do not install or enable services in v0.

Support browser probing:

```sh
/Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --version
```

For takeover, macOS Screen Sharing uses VNC-compatible port `5900`, but enabling
it requires administrator configuration. `crabbox vnc` can print a tunnel only
if port `127.0.0.1:5900` or `localhost:5900` is reachable on the host.

If not reachable:

```text
target=macos does not expose a localhost VNC service; enable Screen Sharing or use a preconfigured VNC server
```

### Static Windows

Do not install or enable services in v0.

Support browser probing for common paths or `where`:

```powershell
where chrome.exe
where msedge.exe
```

Windows native takeover is RDP, not VNC. For v0, `crabbox vnc` should fail
unless a VNC server is already bound to loopback and reachable through SSH.

Clear failure:

```text
target=windows does not support managed VNC in v0; configure a loopback VNC server or use an OS-native remote desktop path
```

Do not open firewall rules or install a VNC server automatically.

### Blacksmith Testbox

`--desktop` and `crabbox vnc` are unsupported until Blacksmith exposes a stable
tunnel/connection API.

Headless browser automation can remain possible through Blacksmith-owned
workflow setup, but Crabbox should fail clearly for desktop takeover:

```text
desktop/VNC is not supported for provider=blacksmith-testbox; Blacksmith owns machine connectivity
```

## Implementation Files

CLI:

- `internal/cli/app.go`: route `vnc`, top-level help.
- `internal/cli/config.go`: `Desktop`, `Browser`, YAML/env parsing.
- `internal/cli/run.go`: `--desktop`, `--browser`, lease acquisition, existing
  lease enforcement, run env injection.
- `internal/cli/bootstrap.go`: optional desktop/browser cloud-init blocks.
- `internal/cli/coordinator.go`: request/response structs and lease conversion.
- `internal/cli/provider_labels.go`: direct provider labels.
- `internal/cli/static.go`: static target probe behavior.
- `internal/cli/ssh_cmd.go`: reuse patterns for claim/touch.
- `internal/cli/vnc.go`: new command.
- `internal/cli/target.go`: provider/target validation helpers.

Worker:

- `worker/src/types.ts`: `desktop`, `browser` on request/record.
- `worker/src/config.ts`: config coercion/defaults.
- `worker/src/bootstrap.ts`: optional desktop/browser bootstrap.
- `worker/src/provider-labels.ts`: cloud labels.
- `worker/src/fleet.ts`: persist booleans and return them in leases.

Docs:

- `docs/features/interactive-desktop-vnc.md`
- `docs/features/runner-bootstrap.md`
- `docs/commands/warmup.md`
- `docs/commands/run.md`
- `docs/commands/vnc.md`
- `docs/commands/README.md`
- `docs/features/README.md`
- `README.md`
- `docs/source-map.md`

## Tests

Go tests:

- `cloudInit(baseConfig())` does not include desktop/browser packages or units.
- `cloudInit(Config{Desktop:true})` includes desktop packages, units, and
  desktop readiness checks.
- `cloudInit(Config{Desktop:true, Browser:true})` includes Chrome setup and
  browser readiness checks.
- `--desktop` and `--browser` parse for `warmup` and `run`.
- `run --desktop` injects `DISPLAY=:99` and `CRABBOX_DESKTOP=1`.
- `run --desktop --browser` injects `CHROME_BIN`, `BROWSER`, and
  `CRABBOX_BROWSER=1` when metadata exists or managed Linux defaults apply.
- `crabbox vnc --id <lease>` prints SSH tunnel, VNC endpoint, display, and
  tunnel warning.
- `crabbox vnc` rejects Blacksmith and unsupported static macOS/Windows cases
  with clear messages.
- Existing `warmup` and `run` tests confirm default behavior remains unchanged.

Worker tests:

- `leaseConfig` defaults `desktop=false`, `browser=false`.
- `leaseConfig({ desktop:true, browser:true })` preserves both.
- Worker cloud-init excludes desktop/browser blocks by default.
- Worker cloud-init includes desktop/browser blocks only when requested.
- Fleet create response stores `desktop` and `browser`.
- Provider labels include `desktop=true` and `browser=true` only when requested
  or include explicit false values if label consistency is preferred.

Docs tests:

- `npm run docs:check` must pass after adding `docs/commands/vnc.md`.

## Gates

Focused during implementation:

```sh
go test ./internal/cli
npm test --prefix worker -- bootstrap config provider-labels fleet
npm run docs:check
```

Before handoff:

```sh
gofmt -w $(git ls-files '*.go')
go vet ./...
go test -race ./...
scripts/check-go-coverage.sh 85.0
npm run check
npm run docs:check
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm test --prefix worker
npm run build --prefix worker
git diff --check
```

Live proof:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
bin/crabbox warmup --provider aws --type t3.small --desktop --browser --ttl 20m --idle-timeout 5m
bin/crabbox run --id <slug> --desktop --browser -- google-chrome --version
bin/crabbox run --id <slug> --desktop --browser --shell 'echo "$DISPLAY"; echo "$CHROME_BIN"'
bin/crabbox vnc --id <slug>
bin/crabbox stop <slug>
bin/crabbox admin leases --state active --json
```

For the first live run, also verify over SSH that VNC is loopback-bound:

```sh
ss -ltn | grep 5900
```

Expected remote bind:

```text
127.0.0.1:5900
```

## Acceptance Criteria

1. Existing `warmup` and `run` behavior is unchanged without `--desktop` or
   `--browser`.
2. `warmup --desktop` requests and provisions a Linux lease with desktop
   bootstrap.
3. `warmup --desktop --browser` additionally provisions a known browser binary.
4. `run --desktop --browser -- <cmd>` runs with `DISPLAY=:99` and browser env.
5. `crabbox vnc --id <lease>` prints a usable SSH tunnel command and endpoint.
6. VNC is never exposed publicly; no provider firewall ingress is added.
7. Static Linux can participate if services already exist.
8. Static macOS/Windows fail clearly when VNC/browser prerequisites are missing.
9. Blacksmith desktop/VNC fails clearly.
10. Docs and tests are updated.
11. The repo is clean except for intentional commits.

## Deferred

- noVNC/websockify.
- Automatic static macOS Screen Sharing enablement.
- Automatic Windows VNC/RDP service installation.
- Browser profile lifecycle management.
- Scenario screenshots, videos, assertions, and PR comments.
- Blacksmith Testbox desktop integration.
