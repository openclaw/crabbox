# doctor

`crabbox doctor` checks local prerequisites and broker/provider access.

```sh
crabbox doctor
crabbox doctor --provider aws
crabbox doctor --provider ssh --target windows --windows-mode normal --static-host win-dev.local
```

It checks local tools, user config permissions, per-lease key generation support,
coordinator health when configured, and direct-provider API access otherwise. If
`CRABBOX_SSH_KEY` is explicitly set, it also validates that private key and
matching `.pub` file.

For `provider=ssh`, doctor checks that the static SSH host is reachable and has
the tools required by the selected target mode.

Flags:

```text
--provider hetzner|aws|ssh
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
```
