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

The CLI defaults to Tailscale and the remote work root `/workspace/crabbox`.
Explicit `tailscale.enabled` YAML values, `CRABBOX_TAILSCALE`, and `--tailscale`
override the transport default, including `false` for native mesh. An explicit
`workRoot` is preserved, even when it equals another provider's default.

## Coordinator configuration

```text
KOYEB_API_TOKEN                         # coordinator-only API credential
CRABBOX_KOYEB_ORGANIZATION_ID           # exact Koyeb organization UUID
CRABBOX_KOYEB_APP_ID                    # existing application UUID
CRABBOX_KOYEB_APP_TARGETS                # validated JSON registry for a managed app pool
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

Start a managed app pool with exactly two registered targets. Each entry binds
an application UUID to its Koyeb name, organization, and region. The legacy
`CRABBOX_KOYEB_APP_ID` must remain one of the entries; all entries must use the
configured organization and region, and names and IDs must be unique.

```json
[
  {
    "organizationID": "33333333-3333-4333-8333-333333333333",
    "appID": "44444444-4444-4444-8444-444444444444",
    "appName": "my-app-workers-1",
    "region": "was"
  },
  {
    "organizationID": "33333333-3333-4333-8333-333333333333",
    "appID": "66666666-6666-4666-8666-666666666666",
    "appName": "my-app-workers-2",
    "region": "was"
  }
]
```

Later additions are count-independent: append validated entries and use the
ordinary coordinator configuration reload. Adding or reordering entries does
not retarget existing leases. Admission freezes the selected application UUID,
provider scope, and region into the durable lease and provisioning plan. A
removed registration, a renamed application, or a live application identity
that differs from its registration blocks further provider work for that
target.

Native mesh mode is selected per lease with `--tailscale=false`. It is
available only when Koyeb's injected `KOYEB_APP_ID`, `KOYEB_APP_NAME`,
`KOYEB_ORGANIZATION_ID`, and `KOYEB_REGION` identify the coordinator's
registered legacy app and region. A lease in another registered app uses the
cross-app private hostname `<service>.<app-name>.internal`. This prevents an
external or mis-targeted coordinator from publishing a private DNS name it
cannot reach. See Koyeb's
[service mesh and discovery documentation](https://www.koyeb.com/docs/reference/service-mesh-and-discovery).

Managed-pool admission uses the supported
`GET /v1/quotas/organizations/{organization_id}/usage` response for organization
service, memory, and configured instance-type usage. Its limits must agree with
the organization quota endpoint. Allowed types and regions, per-app capacity,
and per-type limits remain enforced. A zero type limit in usage means uncapped
only when the quota map omits that type; an explicit zero quota remains
exhausted. Direct app-scoped service listings count every service type for
per-app occupancy. Aggregate memory accounting does not require attributing
memory to a `DATABASE` service or inferring whether Neon is included. Missing,
malformed, inconsistent, or drifted evidence fails closed, including conflicting
app bindings for the same service UUID.

Koyeb caches aggregate usage for 60 seconds and supplies neither service
membership nor a source-generation timestamp. Admission rejects a snapshot
more than 60 seconds after local receipt of the usage response. These windows
can combine to nearly 120 seconds of source age, plus transport time; the local
timestamp does not prove source freshness or atomicity with service creation.
Every unresolved durable lease is therefore added to the aggregate service,
memory, and instance-type counters, even when its service is visible or may
already be included in those totals. Only canonical cleanup or no-resource
evidence retires that reservation. Direct app membership can avoid counting
the same service twice for per-app occupancy; it never reduces the aggregate
overlay.

The existing coordinator transaction counts durable reservations across every
registered app. An unresolved lease keeps its provisioning slot until durable
publication makes it active or canonical evidence confirms no provider resource
remains. Cancellation, failure, expiry, and a healthy provider listing do not
release that slot. Provisioning observations from the organization and each
registered app are combined by service UUID, with deployment identity and
status checked. An observed provisioning service covers a reservation only
when its service ID is already bound in the durable lease. Before publication,
an ID retained only in the operation journal may be conservatively counted again.
This bounds coordinator-owned work; it does not establish organization-wide
headroom for external actors. That requires complete provider accounting or an
authoritative atomic quota-enforcement contract, which remains unverified.

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
   public route, and exposes the Sandbox management port 3030 plus its TCP proxy
   port 3031 only on the private service network. The coordinator calls
   `http://<service>.<app>.internal:3030` with the generated Sandbox bearer,
   bootstraps loopback-only SSH on port 22, and binds the authenticated Sandbox
   proxy to it. No routing key or Tailscale credential exists.
5. Bootstrap starts loopback-only key-only SSH, Xvfb/XFCE, localhost-only VNC,
   and code-server. The Sandbox TCP proxy is the only mesh listener for SSH.
6. The CLI pins the returned SSH host key. It uses `tailscale nc` for the
   default transport and direct SSH to port 3031 on a validated
   `<service>.<app>.internal` mesh host. No public SSH or VNC port is created.
7. Release and expiry delete only a service whose app, organization, lifecycle,
   deployment, image, environment, route, scaling, and generation still match
   the frozen plan. Active/latest deployment drift blocks deletion.

Active-lease cleanup journals the exact allocation and deletion dispatch before
calling Koyeb. After Koyeb accepts deletion, an owned service with a recognized
status remains pending until an observation returns absence or `DELETED`.
The existing scheduler advances confirmation every two seconds; each invocation
performs one observation and can resume from the retained journal after a
coordinator restart. It does not repeat DELETE for a retained dispatch.

The coordinator has an explicit five-minute deletion-confirmation budget,
independent of CLI and Gateway request deadlines. An exhausted budget, unknown
dispatch outcome, authentication failure, malformed response, or ownership
change leaves cleanup unresolved with no automatic retry or success receipt.
Diagnostic records retain deletion result class, recognized observed status,
and timestamps; error messages contain bounded stage/HTTP status rather than
provider response bodies. Inspect the owned resource and retained evidence
before requesting cleanup again. CLI cleanup observers continue to fail closed.

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

The managed-pool implementation has production-shaped local coverage for
cross-app routing and lifecycle ownership. Live cross-app private transport
acceptance is still pending; local verification and a healthy deployment do not
establish that acceptance.

## Related docs

- [Infrastructure](../infrastructure.md)
- [Operations](../operations.md)
- [Tailscale](../features/tailscale.md)
- [Lifecycle cleanup](../features/lifecycle-cleanup.md)
