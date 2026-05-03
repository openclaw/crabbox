# warmup

`crabbox warmup` provisions or leases a remote box and waits until SSH and Crabbox bootstrap plumbing are ready.

```sh
crabbox warmup --class beast
crabbox warmup --provider aws --class beast --market on-demand
crabbox warmup --browser
crabbox warmup --desktop --browser
crabbox warmup --actions-runner
crabbox warmup --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test
crabbox warmup --provider ssh --target macos --static-host mac-studio.local
crabbox warmup --provider ssh --target windows --windows-mode normal --static-host win-dev.local --static-work-root 'C:\crabbox' --browser
```

The command returns a stable `cbx_...` lease ID and a friendly slug. Reuse either for subsequent `run`, `status`, `ssh`, `inspect`, and `stop` commands; scripts should keep using the canonical ID.

With `--provider blacksmith-testbox`, the canonical ID is the Blacksmith `tbx_...` ID returned by `blacksmith testbox warmup`; Crabbox still assigns and stores a local slug for reuse.

With `--provider ssh`, warmup claims an existing static SSH host instead of
creating cloud capacity. Use `--target macos`, `--target windows
--windows-mode normal`, or `--target windows --windows-mode wsl2` to select the
remote command/sync contract. Native Windows static hosts must already have
OpenSSH Server reachable, PowerShell, Git, `tar`, and a writable
`static.workRoot`. Restart `sshd` after installing Git so new SSH sessions see
the updated PATH.

On success, `warmup` prints a concise total duration line. Add `--timing-json` to emit a final JSON timing record with provider, lease ID, slug, total duration, and exit code.

Flags:

```text
--provider hetzner|aws|ssh|blacksmith-testbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--profile <name>
--class <name>
--type <provider-type>
--market spot|on-demand
--ttl <duration>
--idle-timeout <duration>
--desktop
--browser
--keep
--actions-runner
--reclaim
--timing-json
--blacksmith-org <org>
--blacksmith-workflow <file|name|id>
--blacksmith-job <job>
--blacksmith-ref <ref>
```

`--idle-timeout` releases the lease after no touch for that duration, default `30m`. `--ttl` remains the maximum wall-clock lifetime, default `90m`.
Warmup records a local claim tying the lease to the current repo; `--reclaim` overwrites an existing local claim for that lease.

`--browser` provisions a known browser binary and records it in
`/var/lib/crabbox/browser.env`. It can be used without `--desktop` for headless
browser automation. Managed Linux tries Google Chrome stable first, then a
Chromium package fallback.

`--desktop` provisions Xvfb, Openbox, and loopback-bound x11vnc for visible UI
automation and operator takeover. It does not imply a browser. Use
`--desktop --browser` when a headed browser should run in the visible display.

For AWS, `--market` overrides `capacity.market` for this lease. Use
`--market on-demand` when Spot capacity is blocked or when a quota request was
approved only for the standard On-Demand quota. Explicit `--type` still means
exact type: Crabbox reports quota/capacity/policy failures instead of silently
changing capacity.

`--actions-runner` immediately registers the warm box as an ephemeral self-hosted GitHub Actions runner for the current repository. Most projects should prefer `crabbox actions hydrate --id <lease-id-or-slug>` after warmup because it also dispatches the workflow and waits for the ready marker.

`--actions-runner` is not supported with `blacksmith-testbox` because Blacksmith owns Testbox workflow hydration.

New leases use per-lease SSH keys under the user config directory:

```text
~/.config/crabbox/testboxes/<lease-id>/id_ed25519
```
