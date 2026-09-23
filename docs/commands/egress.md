# egress

`crabbox egress` gives a lease mediated outbound network: a lease-local browser
or app proxies its traffic out through the machine running the egress host
agent, rather than reaching the internet directly from the box.

```sh
crabbox egress run --id blue-lobster --allow api.ipify.org --no-sync --no-hydrate -- \
  curl --fail --proxy http://127.0.0.1:3128 https://api.ipify.org
crabbox egress start --id blue-lobster --profile slack
crabbox egress start --id blue-lobster --profile slack --daemon
crabbox desktop launch --id blue-lobster --browser --url https://slack.com/signin --egress slack
crabbox egress status --id blue-lobster
crabbox egress stop --id blue-lobster
```

## How it works

`egress run` and `egress start` set up the same transport:

1. Installs a short-lived egress client helper on the lease.
2. Starts a loopback HTTP proxy on the box (default `127.0.0.1:3128`).
3. Runs a local host bridge on the operator machine.

`run` then executes a command through [`crabbox run`](run.md) and cleans up the
bridge and remote helper on completion or cancellation. The app must opt into
the lease proxy, as `curl --proxy` does above. `start` keeps the bridge running
for manually managed sessions.

Both sides connect outbound to the coordinator using one-use tickets. The
coordinator pairs the two WebSockets and forwards multiplexed proxy frames; it
never opens internet connections itself. Only the host agent dials real
outbound TCP connections.

Automatic setup sends the client ticket through SSH stdin, keeping it
out of SSH, remote shell, and helper process arguments. A foreground helper
validates and closes the bounded SSH input, then hands it through a private
pipe to the detached client before returning. Invalid input fails without
starting a client or falling back to coordinator login. Manual
`host`/`client --ticket` behavior is unchanged.

The data path is:

```text
browser/app in lease
  -> lease 127.0.0.1:3128 (egress client)
  -> coordinator Durable Object (pairs the two sockets)
  -> local crabbox egress host process
  -> internet from the operator machine
```

`desktop launch --egress <profile>` wires the lease-local proxy into the
browser by appending:

```text
--proxy-server=http://127.0.0.1:3128
```

Override the proxy address with `--egress-proxy` if you changed `--listen`.

The [portal](../features/portal.md) lease detail page shows the active egress
session, host/client connection state, and copyable `egress status` /
`egress stop` commands. It does not expose tickets or raw proxy URLs.

## Subcommands

```text
run      Run a command with egress and clean up its session afterward
start    Install the lease client, then run the local host bridge
host     Run only the local egress host bridge
client   Run only the lease-side proxy bridge
status   Show coordinator bridge status
stop     Stop the local host daemon and the remote lease client
```

Use `run` for one job on an existing, exclusively owned Linux SSH lease, and
`start` for a long-lived browser or app session. Use `host` and `client` directly
for manual helper setup or debugging: run `host` on the operator machine and
`client` inside the lease.

```sh
crabbox egress host --id blue-lobster --profile slack
crabbox egress client --id blue-lobster --listen 127.0.0.1:3128
```

For daemonized sessions, ordinary [`stop`](stop.md) also makes a best-effort
cleanup pass before releasing an SSH lease. It stops local egress host daemon
pid state and SSH-kills the lease-side egress client when the target is still
reachable.

Each lease has one active egress session and client. `run` refuses an existing
connected bridge; `start` replaces it. Controllers need exclusive lease ownership.
`run` handles its own scoped cleanup. For manual cleanup, use the ID from the
early `egress session: lease=… session=…` output:

```sh
crabbox egress stop --id blue-lobster --session <session-id>
```

Scoped cleanup requires the current Linux helper and pidfd support. It leaves
host processes untouched and prevents late automatic startup of that session.
Without `--session`, stopping is lease-wide. See [session lifecycle and
cleanup](../features/egress.md#session-lifecycle-and-cleanup) for controller
responsibilities and failure handling.

## Profiles and allowlist

The host side refuses to become an open proxy. Every session needs either a
built-in profile or an explicit allowlist; without one, `run`, `start`, and `host`
exit with `refusing to start an open proxy`.

```sh
crabbox egress start --id blue-lobster --profile slack
crabbox egress start --id blue-lobster --allow example.com,*.example.com
```

`--profile` and `--allow` combine; entries are lowercased and de-duplicated.
Built-in profiles:

- `slack`: `slack.com`, `*.slack.com`, `slack-edge.com`, `*.slack-edge.com`
- `discord`: `discord.com`, `*.discord.com`, `discordcdn.com`,
  `*.discordcdn.com`, `hcaptcha.com`, `*.hcaptcha.com`

A wildcard entry like `*.example.com` matches `example.com` and any of its
subdomains. A bare entry like `example.com` matches only that exact host.

## Flags

Common to all subcommands:

```text
--id <lease-id-or-slug>          target lease (required explicitly for run)
--provider hetzner|aws           coordinator-backed provider (default from config)
```

`start`, `host`, `client`, and `status` also accept `--coordinator <url>`.
`run` uses the configured coordinator so its bridge and workload target the
same lease. Other subcommands also accept the lease as their first positional
argument.

`run` and `start`:

```text
--profile <name>                 built-in allowlist profile
--allow <host,patterns>          comma-separated allowed host patterns
--upstream-proxy-env <ENV_NAME>  local env containing an HTTP/HTTPS upstream proxy URL
--listen 127.0.0.1:<port>        lease-local proxy listen address (default 127.0.0.1:3128)
--target linux                   lease target (linux only; see Limitations)
--network auto|tailscale|public  how the CLI reaches the lease over SSH
```

`run` adds these flags:

```text
--script-stdin                  upload and run a script from stdin; pass script arguments after --
--no-sync                       skip local file transfer
--no-hydrate                    skip configured Actions hydration
```

Pass a command after `--`, or use `--script-stdin`. Scripts are read completely
before changing transport state. Sync and configured hydration retain normal
[`run`](run.md) defaults unless skipped explicitly. `run` stays in the foreground
and does not accept `--daemon`; `start --daemon` backgrounds the host bridge.

`host` and `client` also accept `--ticket <ticket>` and `--session <id>` for
driving a pre-created bridge by hand; `host` takes `--profile`/`--allow`, and
`client` takes `--listen`. `host` also accepts `--upstream-proxy-env`.

`stop` accepts `--session <id>` to stop only the matching lease-side client,
without stopping a local host daemon.

The listen address must be loopback-only (`127.0.0.1`, `::1`, or `localhost`);
any other host is rejected.

## Upstream proxy

To chain destination traffic through an HTTP or HTTPS CONNECT proxy, have
your proxy or credential manager populate a local environment variable:

```sh
crabbox egress run --id blue-lobster --allow api.example.com \
  --upstream-proxy-env CRABBOX_EGRESS_UPSTREAM_PROXY --no-sync --no-hydrate -- \
  curl --fail --proxy http://127.0.0.1:3128 https://api.example.com/health
```

Only the variable name enters arguments; upstream credentials stay on the
host. Selection is explicit and failures never fall back to direct egress.
An empty name, empty variable, or invalid URL is an error; omit the option for
direct egress. The destination allowlist and public-address preflight still
apply. See [upstream proxy setup](../features/egress.md#upstream-proxies) for
authentication, TLS trust, DNS ownership, and grant lifetimes.

## Requirements and limitations

- A configured coordinator login is required. Run
  [`crabbox login --url <broker-url>`](login.md) first.
- `egress run` and `egress start` support coordinator-backed Linux SSH leases
  only; they refuse non-Linux targets because no remote helper install/start
  commands exist for them yet.
- The shipped path is per-app/per-process egress (the browser/app proxy), not
  full VM routing.
- Automatic setup does not install Cloudflare Access service-token credentials
  on the remote lease. Configure a public coordinator route for `run`, use
  `start --coordinator` with that route, or set up the low-level client manually
  with an explicit credential plan.
- Bridge frames are JSON with base64-encoded payloads (max 2 MiB per message).
  That is fine for browser QA; throughput-sensitive workloads are not the
  target use case.

## Troubleshooting

`egress start requires --profile or --allow; refusing to start an open proxy`

The host bridge will not run as an open proxy. Pick a profile or pass an
explicit `--allow` allowlist.

`remote egress client did not listen on 127.0.0.1:3128`

The remote helper failed to come up. Inspect its log on the box:

```sh
crabbox ssh --id blue-lobster
cat /tmp/crabbox-egress-client.log
```

`desktop launch --egress currently requires --browser`

The automatic `--proxy-server` flag is only wired for browser launches. For a
custom app, pass the app's own proxy flag pointing at the lease-local proxy
address printed by `egress start`.

`egress run` sends transport setup messages to stderr and preserves workload
output and exit status. Bridge or cleanup failures also fail the command.
Cancellation allows up to 60 seconds for remote helper cleanup; wrappers should
allow at least 75 seconds after SIGTERM before forcing termination. See
[job lifecycle](../features/egress.md#session-lifecycle-and-cleanup).

For upstream connection, authentication, TLS, or scoped cleanup errors, see
[egress troubleshooting](../features/egress.md#troubleshooting).
