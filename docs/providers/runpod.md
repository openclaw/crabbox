# RunPod Provider

Read when:

- choosing `provider: runpod`;
- pointing Crabbox at a RunPod pod;
- changing `internal/providers/runpod`.

[RunPod](https://runpod.io) is a GPU/CPU cloud built around pods, GPU types,
CPU flavors, templates, and machines. Crabbox treats a pod as a standard
SSH-lease box: it provisions one through the RunPod REST API, waits for public
SSH, and from then on uses the usual sync/run/ssh/status/stop paths. RunPod is
**direct-only** — it never goes through the coordinator.

## How leasing works

A RunPod pod can expose a public IP and a NAT-mapped public port for each
exposed private port. Crabbox deploys pods with `ports: ["22/tcp"]` and
`supportPublicIp: true`, then polls until RunPod reports the public mapping for
port 22. RunPod returns it as `publicIp` plus `portMappings["22"]`; older
responses surface the same data through `runtime.ports[]`:

```json
{
  "ip": "203.0.113.7",
  "privatePort": 22,
  "publicPort": 41010,
  "isIpPublic": true,
  "type": "tcp"
}
```

Once the mapping is live, Crabbox hands the caller a normal `SSHTarget` pointed
at `root@<publicIp>:<publicPort>` and uses its standard SSH transport. RunPod's
basic SSH proxy is not used, because rsync needs the SCP/SFTP support the proxy
lacks.

**SSH auth is public-key only.** Upload your ED25519 public key on the RunPod
settings page once; RunPod injects it into every pod you launch. Crabbox does
not manage that key.

### Lifecycle

- **Acquire** — deploys a pod named `crabbox-<slug>-<leaseSuffix>`, then waits
  for the public TCP mapping for port 22.
- **Resolve** — looks up an existing pod by lease id, pod id, or pod name.
- **List** — enumerates the caller's pods via `GET /v1/pods` and filters to the
  `crabbox-` prefix unless `--all` is set.
- **Release** — calls `DELETE /v1/pods/{id}`, terminating the pod immediately.
- **Doctor** — runs read-only pod-list checks only; it never creates a pod, so
  it is safe to run on every CI invocation.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes (public IP + NAT-mapped port 22) |
| Crabbox sync (rsync over SSH) | Yes |
| Provider-managed sync | No |
| Desktop / browser / code | No |
| Actions hydration | Yes (Linux SSH lease) |
| Coordinator (broker) | No — direct only |
| Tailscale | No (rejected; public SSH only) |

A non-Linux `--target`, `--tailscale`, `--class`, and `--type` are all rejected
for this provider. Use `--runpod-instance-id` instead of `--class`/`--type` and
`--runpod-image` to set the pod image.

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
export RUNPOD_API_KEY=...   # required; from https://www.runpod.io/console/user/settings
```

`CRABBOX_RUNPOD_API_KEY` is also accepted and takes precedence over
`RUNPOD_API_KEY`. The key is read from the environment only — there is no CLI
flag for it, so it cannot be passed on the command line.

Confirm the same credential RunPod expects:

```sh
curl https://rest.runpod.io/v1/pods \
  -H "Authorization: Bearer $RUNPOD_API_KEY"
```

Crabbox sends the identical `Authorization: Bearer $RUNPOD_API_KEY` header to
the REST pod endpoints. Cross-origin redirects are rejected before credentials
or pod-create bodies can be replayed to another destination.

## Config

```yaml
provider: runpod
target: linux
runpod:
  apiUrl: https://rest.runpod.io/v1
  cloudType: SECURE       # SECURE | COMMUNITY
  instanceId: NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500
  image: runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04
  templateId: ""          # optional
  diskGB: 20
  user: root
  workRoot: /tmp/crabbox
```

The defaults pick secure-cloud GPU pods on the RunPod PyTorch image with a 20
GB container disk, because that path reliably exposes the public TCP SSH
mapping Crabbox needs for rsync. `instanceId` accepts a comma-separated GPU
priority list — RunPod tries each in order. Override the GPU type, CPU flavor,
or image as needed, but verify that the shape you pick exposes public TCP SSH
before relying on it.

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

## Cost discipline

Pods are terminated immediately on release. If `--keep` is set, the pod stays
up and keeps accruing cost until you terminate it. Run
`crabbox list --provider runpod` to spot leaked pods, and
`crabbox stop --provider runpod <id>` to clean them up.

## Gotchas

- A funded RunPod account is required. `crabbox doctor --provider runpod`
  succeeds on a zero-balance account because it only reads the pod list — the
  balance shortfall only surfaces when an `Acquire` runs.
- Upload your ED25519 public key to RunPod once before any pod bootstrap will
  accept your SSH session.
- The pod's public SSH port is allocated at runtime and changes between pods;
  never hard-code `--ssh-port`.
- RunPod's basic SSH proxy is not a Crabbox transport, because rsync needs
  SCP/SFTP support.

Related docs:

- [Provider backends](../provider-backends.md)
