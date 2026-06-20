# egress

`crabbox egress` gives a lease mediated outbound network: a lease-local browser
or app proxies its traffic out through the machine running the egress host
agent, rather than reaching the internet directly from the box.

```sh
crabbox egress start --id blue-lobster --profile slack
crabbox egress start --id blue-lobster --profile slack --daemon
crabbox desktop launch --id blue-lobster --browser --url https://example.com/login --egress slack
crabbox egress status --id blue-lobster
crabbox egress stop --id blue-lobster
```

## How it works

`egress start` does three things:

1. Installs a short-lived egress client helper on the lease.
2. Starts a loopback HTTP proxy on the box (default `127.0.0.1:3128`).
3. Runs a local host bridge on the operator machine.

Both sides connect outbound to the coordinator using one-use tickets. The
coordinator pairs the two WebSockets and forwards multiplexed proxy frames; it
never opens internet connections itself. Only the host agent dials real
outbound TCP connections.

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
start    Install the lease client, then run the local host bridge
host     Run only the local egress host bridge
client   Run only the lease-side proxy bridge
status   Show coordinator bridge status
stop     Stop the local host daemon and the remote lease client
```

`start` is the normal entry point. Use `host` and `client` directly when
debugging tickets, custom tunnels, or a manually installed helper.

For daemonized sessions, ordinary [`stop`](stop.md) also makes a best-effort
cleanup pass before releasing an SSH lease. It stops local egress host daemon
pid state and SSH-kills the lease-side egress client when the target is still
reachable.

## Profiles and allowlist

The host side refuses to become an open proxy. Every session needs either a
built-in profile or an explicit allowlist; without one, `start`, `host`, and
`client` exit with `refusing to start an open proxy`.

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
--id <lease-id-or-slug>          target lease (or first positional arg)
--provider hetzner|aws           coordinator-backed provider (default from config)
--coordinator <url>              coordinator URL override
```

`start`:

```text
--profile <name>                 built-in allowlist profile
--allow <host,patterns>          comma-separated allowed host patterns
--listen 127.0.0.1:<port>        lease-local proxy listen address (default 127.0.0.1:3128)
--daemon                         run the local host bridge in the background
--target linux                   lease target (linux only; see Limitations)
--network auto|tailscale|public  how the CLI reaches the lease over SSH
```

`host` and `client` also accept `--ticket <ticket>` and `--session <id>` for
driving a pre-created bridge by hand; `host` takes `--profile`/`--allow`, and
`client` takes `--listen`.

The listen address must be loopback-only (`127.0.0.1`, `::1`, or `localhost`);
any other host is rejected.

## Requirements and limitations

- A configured coordinator login is required. Run
  [`crabbox login --url <broker-url>`](login.md) first.
- `egress start` supports coordinator-backed Linux SSH leases only; it refuses
  non-Linux targets because no remote helper install/start commands exist for
  them yet.
- The shipped path is per-app/per-process egress (the browser/app proxy), not
  full VM routing.
- `egress start` does not install Cloudflare Access service-token credentials
  on the remote lease. If Access credentials are configured locally, point
  `--coordinator` at a public coordinator route, or run `egress client`
  manually only when it is safe to supply the required access headers.
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
