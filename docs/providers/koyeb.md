# Koyeb Sandbox Provider

Read this when you are:

- choosing brokered `provider: koyeb`;
- operating the Koyeb coordinator or immutable runner image;
- debugging Sandbox provisioning, private-mesh or Tailscale access, or cleanup;
- changing `internal/providers/koyeb`, `worker/src/koyeb.ts`, or
  `images/koyeb-sandbox-runner`.

Koyeb is a coordinator-only Linux SSH-lease provider. The coordinator creates
one short-lived Koyeb Sandbox service from a digest-pinned runner image and
uses its authenticated management API to bootstrap it. Tailscale remains the
default transport. A coordinator running in the same Koyeb app, organization,
and region can instead request Koyeb's native private mesh explicitly.

## When to use it

Use Koyeb when you want disposable, browser-capable Linux workspaces without
operating a VM substrate. The reviewed runner contains Chrome, XFCE, VNC,
code-server, Git, Node.js, Python with pip and venv support, build-essential,
zip/unzip, and Crabbox's browser and terminal launchers.

Koyeb is not a direct CLI provider. It requires the Node/PostgreSQL or
Cloudflare coordinator to own the Koyeb credential, encrypted provisioning
material, lease journal, and cleanup. The Tailscale transport additionally
requires an OAuth client.

## Commands

```sh
crabbox doctor --provider koyeb
crabbox warmup --provider koyeb --desktop --browser --code
crabbox warmup --provider koyeb --desktop --browser --code --tailscale=false
crabbox run --provider koyeb --id swift-crab -- pnpm test
crabbox ssh --provider koyeb --id swift-crab
crabbox vnc --provider koyeb --id swift-crab
crabbox stop --provider koyeb swift-crab
```

Configure the normal broker URL and user authentication on the CLI. Do not put
the Koyeb API token or Tailscale OAuth secret in local Crabbox config.

## Coordinator configuration

```text
KOYEB_API_TOKEN                         # coordinator-only API credential
CRABBOX_KOYEB_ORGANIZATION_ID           # exact Koyeb organization UUID
CRABBOX_KOYEB_APP_ID                    # existing application UUID
CRABBOX_KOYEB_IMAGE                     # runner image as tag@sha256:digest
CRABBOX_KOYEB_API_URL                   # optional; default https://app.koyeb.com
CRABBOX_KOYEB_REGION                    # optional; default was
CRABBOX_KOYEB_INSTANCE_TYPE             # optional; default large
CRABBOX_KOYEB_REGISTRY_SECRET           # optional private-registry secret name
CRABBOX_DURABLE_PROVISIONING_ADMISSION  # required true
CRABBOX_SESSION_SECRET                  # durable-material encryption key
CRABBOX_TAILSCALE_CLIENT_ID             # optional; required by the default transport
CRABBOX_TAILSCALE_CLIENT_SECRET         # optional; required by the default transport
CRABBOX_TAILSCALE_TAGS                  # optional dedicated runner tag allowlist
```

The image reference must include an immutable digest. The optional registry
value is the Koyeb secret name, not a credential or UUID. The coordinator sends
the Koyeb token only to the Koyeb control plane and never stores it in a lease
record or runner environment.

Native mesh mode is selected per lease with `--tailscale=false`. It is
available only when Koyeb's injected `KOYEB_APP_ID`, `KOYEB_APP_NAME`,
`KOYEB_ORGANIZATION_ID`, and `KOYEB_REGION` identify the configured target app
and region. This prevents an external or mis-targeted coordinator from
publishing a private DNS name it cannot reach. See Koyeb's
[service mesh and discovery documentation](https://www.koyeb.com/docs/reference/service-mesh-and-discovery).

## Lifecycle and access

1. The coordinator freezes the lease, runner image, placement, owner, SSH key,
   TTL, idle timeout, and provisioning generation in a durable plan.
2. It creates one uniquely named Sandbox service with exact ownership markers,
   `min=1`, `max=1`, no sleep target, and Koyeb lifecycle deletion bounds.
3. With the default transport, the only public route is
   `/koyeb-sandbox/` on port 3030. Koyeb edge policy requires a generated API
   key, and the runner validates the same bearer. Bootstrap consumes a
   one-call Tailscale auth key and publishes a tailnet SSH identity.
4. With `--tailscale=false`, the Sandbox joins the Koyeb mesh, publishes no
   public route, and exposes ports 3030 and 22 only on the private service
   network. The coordinator calls `http://<service>.<app>.internal:3030` with
   the generated Sandbox bearer; no routing key or Tailscale credential exists.
5. Bootstrap starts key-only SSH, Xvfb/XFCE, localhost-only VNC, and
   code-server. SSH listens on the private interface only in mesh mode.
6. The CLI pins the returned SSH host key. It uses `tailscale nc` for the
   default transport and direct SSH for a validated `<service>.<app>.internal`
   mesh host. No public SSH or VNC port is created.
7. Release and expiry delete only a service whose app, organization, lifecycle,
   deployment, image, environment, route, scaling, and generation still match
   the frozen plan. Active/latest deployment drift blocks deletion.

Scale-to-zero is deliberately disabled. Runner identity lives in ephemeral
Sandbox state; the default transport also uses a non-reusable enrollment key.
A deep-sleep restart would strand a published lease. Cost is bounded with hard
TTL, inactivity deletion, and explicit cleanup instead.

## Capabilities

- Provider kind: SSH lease, Linux amd64 only.
- SSH and rsync: yes, through userspace Tailscale by default or the Koyeb mesh
  when explicitly selected.
- Desktop, browser, VNC, terminal, and code-server: yes.
- Public workload ingress: no.
- Actions hydration: no.
- Native checkpoints or persistent disks: no.
- Coordinator: required.
- Default instance type: `large`.

## Verification

Without creating a Sandbox:

```sh
crabbox doctor --provider koyeb
npm test --prefix worker -- --run test/koyeb.test.ts
node images/koyeb-sandbox-runner/contract.test.mjs
```

A live lifecycle test is billable and requires valid coordinator, Koyeb,
registry, and runner-image configuration. The default transport also requires
Tailscale. Run it only against an empty dedicated application, then confirm the
created service is deleted.

## Related docs

- [Infrastructure](../infrastructure.md)
- [Operations](../operations.md)
- [Tailscale](../features/tailscale.md)
- [Lifecycle cleanup](../features/lifecycle-cleanup.md)
