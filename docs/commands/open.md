# open

`crabbox open` prepares an existing SSH-capable lease for an external editor.
Select the editor with `--editor`; Crabbox prints its connection instructions
and remote folder, then keeps the lease active until you press Ctrl-C.

Zed Remote Projects is the first supported editor:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

In Zed, open **Remote Projects**, select **Connect New Server**, paste the
printed command, and open the printed folder. Keep `crabbox open` running for
the duration of the editor session.

## Why the command stays running

External editors open their own SSH processes after a connection is added.
Those processes do not send Crabbox lease heartbeats. While `crabbox open` is
running, Crabbox keeps coordinator-backed leases heartbeating and marks
direct-provider leases running or ready where the provider supports lease
touches.

The lease's configured hard TTL still applies while the command is running.

Stopping `crabbox open` does not release the lease. Use
`crabbox stop <id-or-slug>` when the workspace is no longer needed.

## Workspace and synchronization

The command opens the current repository's normal remote workspace and follows
the current local subdirectory. It also honors a workspace hydrated through
GitHub Actions. If the folder does not exist, the command asks you to run
`crabbox run --id <lease> --sync-only` first.

Crabbox synchronization is local to remote. Commit and push changes made in
the editor, or copy them back explicitly, before releasing an ephemeral lease.

## Zed security and platform boundaries

The Zed handoff does not edit `~/.ssh/config`, modify Zed settings, or launch an
editor process. Zed's command-line URL syntax cannot carry arbitrary SSH
options such as per-lease identity files and proxy commands, so the complete
command is handed to Zed's supported **Connect New Server** flow instead.

Zed supports key-based SSH access to Linux and macOS targets. The handoff
rejects Windows remote targets, which Zed does not support as remote servers,
and token-as-username SSH providers, whose credentials should not be persisted
in editor settings.

## Flags

`crabbox open` requires `--editor=<name>` and accepts the same lease, provider,
target, routing, network, and `--reclaim` flags as
[`crabbox connect`](connect.md). The first positional argument is also accepted
as the lease id or slug:

```sh
crabbox open --editor=zed swift-crab
crabbox open --editor=zed --id swift-crab --network tailscale
crabbox open --editor=zed --provider ssh --target macos --static-host mac-studio.local
```

## See also

- [`crabbox run`](run.md) - synchronize the local checkout to the lease.
- [`crabbox ssh`](ssh.md) - print a general-purpose SSH command without holding a heartbeat.
- [`crabbox connect`](connect.md) - open an interactive terminal session.
- [`crabbox stop`](stop.md) - release the lease when finished.
