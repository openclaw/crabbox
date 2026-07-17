# open

`crabbox open` prepares an existing SSH-capable lease for an external editor.
Select the editor with `--editor`; Crabbox prints its connection details and
remote folder, then keeps the lease active until you press Ctrl-C.

Zed Remote Projects is the first supported editor:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

The default output separates the SSH command and remote folder into labeled,
copyable blocks. In Zed, open **Remote Projects**, select **Connect New Server**,
paste the SSH command, and open the remote folder. Keep `crabbox open` running
for the duration of the editor session.

## Automation and agents

Use `--json` when another process needs to consume the handoff:

```sh
crabbox open --editor=zed --id swift-crab --json
```

Crabbox writes one newline-terminated JSON object to stdout and then remains in
the foreground to maintain lease activity. Diagnostics and workspace notices
continue to use stderr, so stdout stays machine-readable. An agent can start the
command as a child process, decode the first JSON object, use `sshCommand` and
`remoteFolder`, and keep the child running until the editor session ends.

The versioned `crabbox/editor-handoff/v1` object includes:

- `editor` and `displayName`
- `leaseId`, `sshCommand`, and `remoteFolder`
- `hydratedByActions`
- `leaseActivity`, which is `foreground`
- `hardTTLApplies`
- `releaseCommand`, when the lease has an id

`releaseCommand` carries the effective non-secret provider and routing flags,
so it remains usable when the handoff overrides repository configuration.

## Why the command stays running

External editors open their own SSH processes after a connection is added.
Those processes do not send Crabbox lease heartbeats. While `crabbox open` is
running, Crabbox keeps coordinator-backed leases heartbeating and marks
direct-provider leases running or ready where the provider supports lease
touches.

Managed leases with a resolved expiry keep their configured hard TTL while the
command is running. Static SSH targets have no Crabbox-enforced hard TTL.

Stopping `crabbox open` does not release the lease. Use the printed release
command or `crabbox stop <id-or-slug>` when the workspace is no longer needed.

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

`crabbox open` requires `--editor=<name>`. Add `--json` for the versioned
machine-readable handoff. The command accepts the same lease, provider, target,
routing, network, and `--reclaim` flags as [`crabbox connect`](connect.md). The
first positional argument is also accepted as the lease id or slug:

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
