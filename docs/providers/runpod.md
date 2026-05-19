# RunPod Provider

Read when:

- choosing `provider: runpod`;
- pointing Crabbox at a RunPod CPU (or GPU) pod;
- changing `internal/providers/runpod`.

[RunPod](https://runpod.io) is a GPU/CPU cloud whose primitives are pods, GPU
types, CPU flavors, templates, and machines. RunPod's public API is GraphQL at
`https://api.runpod.io/graphql` and is authenticated with
`Authorization: Bearer $RUNPOD_API_KEY`. The same key is accepted as a query
parameter (`?api_key=...`), but Crabbox only sends the header form.

## SSH Lease Shape

A RunPod pod has a static public IP and a NAT-mapped public port for each
exposed private port. When the pod is created with `startSsh: true` and
`ports: "22/tcp"`, RunPod reports the mapping via the pod's
`runtime.ports[]` connection once the pod reaches `RUNNING`:

```json
{
  "ip": "203.0.113.7",
  "privatePort": 22,
  "publicPort": 41010,
  "isIpPublic": true,
  "type": "tcp"
}
```

Crabbox provisions the pod through `deployCpuPod`, polls `pod(input: {podId})`
until `runtime.ports` exposes a public port-22 mapping, and then hands the
caller a normal `SSHTarget` pointed at `root@<ip>:<publicPort>`. From that
point on, Crabbox uses its existing SSH sync, run, status, ssh, and stop paths
— there is no parallel RunPod-specific exec surface.

Public-key SSH authentication is the only supported RunPod auth method. Upload
your ED25519 public key under the RunPod settings page once; RunPod injects it
into every pod you launch. Crabbox does not manage the public key.

## Pod Lifecycle

`Acquire` deploys a CPU pod whose name follows the standard Crabbox
`crabbox-<slug>-<leaseSuffix>` pattern, then waits for `runtime.ports` to
expose a public TCP mapping for port 22. `Resolve` looks up an existing pod by
lease id, pod id, or pod name. `List` enumerates the caller's pods via
`myself { pods { … } }` and filters to the `crabbox-` prefix unless `--all` is
set. `ReleaseLease` calls `podTerminate(input: {podId})`. `Doctor` runs
`myself` (auth check) and `myself.pods` (inventory check) without ever calling
a mutation — it is safe to run on every CI.

## Defaults And Cost Discipline

The defaults pick the cheapest documented CPU flavor (`cpu3c-2-4`,
compute-optimized, 2 vCPU / 4 GB RAM) on the `runpod/base:0.6.2` image with a
10 GB container disk and `cloudType: ALL`. Override any of these via flags,
env vars, or YAML before pointing the provider at a GPU SKU — GPU pods cost
substantially more per hour. Pods are terminated immediately on
`ReleaseLease`; if `--keep` is set, the pod stays up and accrues cost until
the caller terminates it. Run `crabbox list --provider runpod` to spot leaked
pods.

## Commands

```sh
crabbox warmup --provider runpod
crabbox run --provider runpod -- pnpm test
crabbox ssh --provider runpod
crabbox status --provider runpod
crabbox stop --provider runpod $LEASE_ID
crabbox list --provider runpod
```

## Auth

```sh
export RUNPOD_API_KEY=...   # required, from https://www.runpod.io/console/user/settings
```

`CRABBOX_RUNPOD_API_KEY` is also accepted and wins over `RUNPOD_API_KEY`,
matching the precedence other direct providers use. The key is read from the
environment only; the provider does not register a CLI flag for it. Do not
pass the key on the command line.

The canonical RunPod request shape is:

```sh
curl -X POST https://api.runpod.io/graphql \
  -H "Authorization: Bearer $RUNPOD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"query":"query { myself { id email } }"}'
```

Crabbox sends the same `Authorization: Bearer $RUNPOD_API_KEY` header and a
JSON `{query, variables}` body to the same endpoint.

## Config

```yaml
provider: runpod
target: linux
runpod:
  apiUrl: https://api.runpod.io/graphql
  cloudType: ALL          # ALL | SECURE | COMMUNITY
  instanceId: cpu3c-2-4   # cheapest documented CPU flavor
  image: runpod/base:0.6.2
  templateId: ""          # optional
  diskGB: 10
  user: root
  workRoot: /tmp/crabbox
```

Provider flags:

```text
--runpod-url
--runpod-cloud-type
--runpod-instance-id
--runpod-image
--runpod-template-id
--runpod-disk-gb
--runpod-user
--runpod-work-root
```

Environment overrides:

```text
CRABBOX_RUNPOD_API_KEY      (or RUNPOD_API_KEY)
CRABBOX_RUNPOD_API_URL      (or RUNPOD_API_URL)
CRABBOX_RUNPOD_CLOUD_TYPE   (or RUNPOD_CLOUD_TYPE)
CRABBOX_RUNPOD_INSTANCE_ID  (or RUNPOD_INSTANCE_ID)
CRABBOX_RUNPOD_IMAGE        (or RUNPOD_IMAGE)
CRABBOX_RUNPOD_TEMPLATE_ID  (or RUNPOD_TEMPLATE_ID)
CRABBOX_RUNPOD_DISK_GB
CRABBOX_RUNPOD_USER
CRABBOX_RUNPOD_WORK_ROOT
```

## Capabilities

- SSH: yes (public IP + NAT-mapped port 22).
- Crabbox sync: yes (standard rsync-over-SSH).
- Provider sync: no.
- Desktop/browser/code: no.
- Actions hydration: yes (Linux SSH lease).
- Coordinator: no — RunPod runs in direct-only mode.

## Gotchas

- A funded RunPod account is required. `deployCpuPod` returns
  `Your account balance is too low to rent a pod. Please add funds to your account.`
  when `clientBalance == 0`. `crabbox doctor --provider runpod` succeeds on a
  zero-balance account because it only reads `myself` and the pod list — the
  balance shortfall only surfaces when an `Acquire` runs.
- You must upload your ED25519 public key to RunPod once before any pod
  bootstrap will accept your SSH session.
- The pod's public port for SSH is allocated at runtime and changes between
  pods; never hard-code `--ssh-port`.
- `--class` and `--type` are rejected for this provider; use
  `--runpod-instance-id` and `--runpod-image` instead.
- `--tailscale` is rejected; RunPod pods expose only public SSH.

Related docs:

- [Provider backends](../provider-backends.md)
