# zed

`crabbox zed` prepares an existing SSH-capable lease for use with Zed Remote
Projects. It prints the exact SSH command and remote folder to enter in Zed,
then keeps the lease active until you press Ctrl-C.

```sh
crabbox run --id swift-crab --sync-only
crabbox zed --id swift-crab
```

In Zed, open **Remote Projects**, select **Connect New Server**, paste the
printed command, and open the printed folder. Keep `crabbox zed` running for
the duration of the editor session.

## Why the command stays running

Zed opens its own SSH processes after the connection is added. Those processes
do not send Crabbox lease heartbeats. While `crabbox zed` is running, Crabbox
keeps coordinator-backed leases heartbeating and marks direct-provider leases
running or ready where the provider supports lease touches.

The lease's configured hard TTL still applies while the command is running.

Stopping `crabbox zed` does not release the lease. Use
`crabbox stop <id-or-slug>` when the workspace is no longer needed.

## Workspace and synchronization

The command opens the current repository's normal remote workspace and follows
the current local subdirectory. It also honors a workspace hydrated through
GitHub Actions. If the folder does not exist, the command asks you to run
`crabbox run --id <lease> --sync-only` first.

Crabbox synchronization is local to remote. Commit and push changes made in
Zed, or copy them back explicitly, before releasing an ephemeral lease.

## Security and platform boundaries

`crabbox zed` does not edit `~/.ssh/config`, modify Zed settings, or launch an
editor process. Zed's command-line URL syntax cannot carry arbitrary SSH
options such as per-lease identity files and proxy commands, so the complete
command is handed to Zed's supported **Connect New Server** flow instead.

The command supports key-based SSH access to Linux and macOS targets. It rejects
Windows remote targets, which Zed does not support as remote servers, and
token-as-username SSH providers, whose credentials should not be persisted in
editor settings.

## Flags

`crabbox zed` accepts the same lease, provider, target, routing, network, and
`--reclaim` flags as [`crabbox connect`](connect.md). The first positional
argument is also accepted as the lease id or slug:

```sh
crabbox zed swift-crab
crabbox zed --id swift-crab --network tailscale
crabbox zed --provider ssh --target macos --static-host mac-studio.local
```

## See also

- [`crabbox run`](run.md) - synchronize the local checkout to the lease.
- [`crabbox ssh`](ssh.md) - print a general-purpose SSH command without holding a heartbeat.
- [`crabbox connect`](connect.md) - open an interactive terminal session.
- [`crabbox stop`](stop.md) - release the lease when finished.
