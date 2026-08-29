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
single `SANDBOX:PATH` operand to the remote side and transfers over the resolved
transport. Both upload and download preserve the existing cp syntax. `-L`
follows host-side symlinks during upload.

Crabbox prefers local rsync 3.4.3 or newer and rejects older clients for data
transfer because known sender and receiver vulnerabilities cross the lease
trust boundary. From POSIX operator hosts to native Linux or macOS leases, when local
rsync is missing or older—including stock macOS OpenRsync—Crabbox creates or validates a checksummed tar+gzip stream in Go and
carries it over the same private SSH session. The remote uses standard POSIX
archive and filesystem tools only as the archive endpoint; Crabbox does not
fall back to rsync, scp, or another copy protocol.

Archive entries are confined to one root with bounded entry and byte counts.
Downloads reject links, special files, duplicate paths, and invalid checksums
before replacing the destination. Archive uploads reject symlinks unless `-L`
follows them. Archive transfers stage the complete
operand and replace its selected destination only after validation. An
interruption before publication keeps the prior contents at the target or in a
durable backup that the next copy restores. After publication, the new target
remains and the next copy finishes backup cleanup. Target-named sidecars recover either state
after a process or host crash. Unlike rsync's incremental merge,
a successful archive transfer replaces that selected subtree and does not
preserve unrelated entries already inside it.

Downloads accept regular files and directories, not lease-provided symlinks or
special files. Ownership and group metadata are discarded, and newly created
host files and directories use normalized `0644`/`0755` modes subject to the
host umask rather than lease-provided permission bits.

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

For file copy and `crabbox tunnel`, Crabbox writes a private temporary OpenSSH config
containing the resolved user, host, port, key/certificate paths, host-key
policy, and ProxyCommand. The Crabbox-launched subprocess receives only `-F
<private-path>` and a fixed non-secret alias. Token usernames therefore do not
enter that argv or environment. For targets whose routing lives in the user's
OpenSSH config, Crabbox preserves its resolved identity and certificate files
and identity agent along with `HostName`, `ProxyJump`, and `ProxyCommand`. Interactive directives
such as extra forwards, TTY requests, and remote commands are not inherited.
OpenSSH executes the provider-resolved ProxyCommand under the provider's
existing transport contract. The config directory is mode `0700`, the file is
mode `0600`, and cleanup runs after the child exits. Windows applies a protected
current-user DACL instead of relying on POSIX mode bits. When a Windows client
uses WSL rsync, Crabbox stages the private config and identity in a
mode-restricted WSL directory and removes that directory after the copy.
Config-backed Windows aliases instead use native rsync and OpenSSH so
`%USERPROFILE%\.ssh\config` routing can be resolved safely.

VNC/WebVNC tunnel children and pond member forwards use this private session
when the resolved target marks its SSH username as secret (`AuthSecret`).
Ordinary, non-secret tunnel invocations retain their established arguments.
Printed VNC/WebVNC tunnel commands redact secret usernames and provider proxy
commands; those placeholders are not runnable credentials. Managed secret
targets reject proxy commands containing or expanding the secret username and
disable ambient identities, certificates, and agents unless the target
explicitly supplies identity/certificate files. Config-backed routes preserve
the operator's explicit authentication and routing contract. Their
`ProxyCommand`, `Match exec`, or `ProxyJump` directives may independently expand
tokens into descendant arguments; private root arguments do not guarantee
secrecy inside those external helpers.

Attached VNC/WebVNC and pond sessions retain the private config until SSH is
reaped and owned process-tree teardown completes. Pond forwards keep one
connection per member and preserve the ten-second connect timeout and three
connection attempts. Its Unix anchor receives the same filtered and overridden
child environment as SSH.

Detached native VNC and secret-backed `pond connect --export` instead retain
all private configs, including generated jump configs, until every requested
IPv4 `127.0.0.1` listener belongs to its exact tracked SSH root PID. Roots use
`ControlMaster=no`, `ControlPath=none`, `ControlPersist=no`, and
`ForkAfterAuthentication=no`. OpenSSH establishes these listeners after
authentication, so this barrier permits deleting the configs before returning
success while forwarding continues over the established connections. A
descendant's listener, a reachable port, or an elapsed grace period is not
sufficient. Startup remains bounded and cancellable; failures stop and reap
started children. Cleanup failures are reported rather than successful
detachment, and failed tree teardown retains configs. Pond publishes its
existing daemon state only after this barrier and config cleanup, with a safe
`-F` path and alias in the recorded command. Windows pond export remains
unsupported; platforms without listener ownership checks fail closed.

An abrupt parent death before the barrier can leave a temporary config
directory; there is no new crash-recovery daemon. This protection covers the
tunnel spawn owners, not all SSH-based CLI commands: command-based readiness,
password retrieval, remote setup/cleanup, and sync need their own transport
boundary. Native VNC handoff stdout still intentionally contains the viewer
credentials documented by that command.

## Workspace-owner setup failures

On reused POSIX and WSL2 leases, SSH readiness commands also run under the
remote workspace owner. A successful SSH login or bootstrap readiness command
does not prove that the host permits child observation and witness registration.
If that setup fails, Crabbox stops with `remote workspace owner setup failed`
and the setup stage, rather than retrying it as `ssh-auth` or a TCP-port failure.
Check the remote process-observation permissions, lock tools, and owner state;
do not disable ownership checks or relax host-key verification.

Setup diagnostics contain a bounded stage identifier, without owner tokens,
paths, or process command lines. A per-command marker separates them from
workload stderr, and classification ends before workload execution. A workload
that returns exit code 74 therefore remains a workload failure.

## Related

- [`cp`](../commands/cp.md)
- [`tunnel`](../commands/tunnel.md)
- [Sync](sync.md)
- [Network and reachability](network.md)
- [SSH keys](ssh-keys.md)
