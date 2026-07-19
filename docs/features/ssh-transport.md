# SSH lease transport

Read this when you are:

- copying files to or from an SSH-backed lease;
- forwarding a remote loopback service to the operator machine;
- reviewing how resolved SSH credentials stay inside Crabbox.

Crabbox providers return a provider-neutral `SSHTarget`. Network resolution then
selects the public, tailnet, or provider ProxyCommand route. File copy and local
forwarding consume that resolved target instead of rebuilding provider-specific
SSH rules.

## File copy

`crabbox cp` preserves provider-native copy when available. If the backend has
no native copy capability but does expose a managed SSH lease, Crabbox maps the
single `SANDBOX:PATH` operand to the remote side and runs rsync over the resolved
transport. Both upload and download preserve the existing cp syntax. `-L`
follows host-side symlinks during upload.

Transfers require local rsync 3.4.3 or newer. Crabbox rejects older clients
before connecting because known sender and receiver vulnerabilities cross the
lease trust boundary.

POSIX and WSL2 SSH targets use this path. Native Windows sync is archive-based,
not rsync-based, so native Windows currently needs a provider-native copy
backend. WSL2 copies probe the remote rsync for secluded-argument support and
use that protocol mode so paths never cross the Windows login shell parser.

## Local forwarding

`crabbox tunnel --id <lease> [--local-port <port>] <remote-port>` creates:

```text
127.0.0.1:<local-port> -> lease 127.0.0.1:<remote-port>
```

Both endpoints are intentionally loopback-only. Automatic local ports are
reserved against concurrent Crabbox selection before SSH starts. Readiness
requires listener ownership by the tracked SSH process tree plus a successful
TCP connection; only then does stdout receive the local HTTP URL.

The forward remains attached to the command. Context or terminal cancellation
hard-stops and reaps the isolated process group on Unix or Job Object on
Windows, including provider proxy descendants.

## Credential boundary

For each transfer or forward, Crabbox writes a private temporary OpenSSH config
containing the resolved user, host, port, key/certificate paths, host-key
policy, and ProxyCommand. The Crabbox-launched subprocess receives only `-F
<private-path>` and a fixed non-secret alias. Token usernames therefore do not
enter that argv or environment. For targets whose routing lives in the user's
OpenSSH config, Crabbox preserves its resolved identity and certificate files
along with `HostName`, `ProxyJump`, and `ProxyCommand`. Interactive directives
such as extra forwards, TTY requests, and remote commands are not inherited.
OpenSSH executes the provider-resolved ProxyCommand under the provider's
existing transport contract. The config directory is mode `0700`, the file is
mode `0600`, and cleanup runs after the child exits. Windows applies a protected
current-user DACL instead of relying on POSIX mode bits. When a Windows client
uses WSL rsync, Crabbox stages the private config and identity in a
mode-restricted WSL directory and removes that directory after the copy.
Config-backed Windows aliases instead use native rsync and OpenSSH so
`%USERPROFILE%\.ssh\config` routing can be resolved safely.

## Related

- [`cp`](../commands/cp.md)
- [`tunnel`](../commands/tunnel.md)
- [Sync](sync.md)
- [Network and reachability](network.md)
- [SSH keys](ssh-keys.md)
