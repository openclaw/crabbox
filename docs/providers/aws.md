# AWS Provider

Read this when you are:

- choosing `provider: aws`;
- debugging EC2 capacity, Service Quotas, AMIs, security groups, or EC2 Mac
  Dedicated Hosts;
- changing `internal/providers/aws` or brokered AWS provisioning in the coordinator.

AWS is the broad managed provider. Its normal CLI path is an SSH-lease backend:
Crabbox provisions an EC2 instance, then owns SSH readiness, sync, command
execution, results, desktop tunnels, and cleanup. It supports Linux, native
Windows, Windows under WSL2, and EC2 Mac. AWS is one of the five providers that
can run through the coordinator (alongside Azure, Daytona, GCP, and Hetzner);
without a broker URL configured it runs direct-from-CLI against the EC2 API.

A separate [private AWS workspace service](../features/aws-private-workspaces.md)
uses the Node/PostgreSQL coordinator, an exact small-instance allowlist, and SSM
instead of SSH. It is an API-managed deployment shape, not another CLI machine
class.

## When to use AWS

Reach for AWS when you need:

- managed Windows or WSL2 test machines on EC2 capacity;
- EC2 Mac desktops backed by a Dedicated Host;
- broad Linux capacity with Spot and On-Demand fallback;
- broker-owned cloud credentials and cost accounting.

Prefer [Hetzner](./hetzner.md) for cheaper Linux-only capacity, or
[Static SSH](./ssh.md) when a known host already exists.

## Commands

```sh
crabbox warmup --provider aws --class standard
crabbox warmup --provider aws --arch arm64 --class fast
crabbox run --provider aws --class fast -- pnpm test
crabbox run --provider aws --market on-demand -- pnpm check
crabbox run --provider aws --id cbx_abcdef123456 --lease-output run.json -- pnpm test
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox warmup --provider aws --lease-id cbx_abcdef123456 --slug operation-display
```

`--type` is exact: if EC2 rejects the requested type, Crabbox fails rather than
silently substituting another instance. Use `--class` when you want capacity
fallback across instance families.

### Fixed-ID replay

`warmup --lease-id cbx_<12 lowercase hex>` makes direct AWS acquisition
idempotent across process restarts. Crabbox writes the normalized create intent
to the ordinary durable lease claim under its existing cross-process lock
before any provider mutation. Each resolved launch attempt records its exact
region or availability zone, subnet, type, market, AMI, security group, keypair,
stable timestamps, parameter hash, and a deterministic per-attempt EC2 client
token before `RunInstances`.

An ambiguous response remains pinned to that attempt. Replay first reconciles
all matching Crabbox-tagged resources, fails closed if more than one exists,
and fails closed without another `RunInstances` call while the resource is not
visible. The initial request writes non-secret attempt-attestation tags, and a
later replay adopts a visible instance only when its provider identity and every
attempt tag match the persisted region/AZ/subnet/type/market/image/security-group/
host/key/token tuple. This deliberately
accepts a false-negative if the process crashes after persisting the attempt but
before sending the network request, preserving at-most-once launch semantics.
Capacity fallback advances only after a definite no-resource response clears
the attempt during the original invocation. A changed create intent returns
`lease_id_conflict`; a matching slug alone never authorizes adoption. Once
acquisition is marked complete, a missing bound instance is also a terminal
conflict rather than permission to call `RunInstances` again.

Once Crabbox observes an instance matching the durable launch attempt, it
saves the instance identity and cleanup labels before waiting for its public
IP or SSH readiness. The claim remains prepared until readiness succeeds, so
an interrupted warmup can be stopped with
`crabbox stop --provider aws --id cbx_...` without authorizing normal use of the
unfinished lease. Replay remains bound to that exact instance; readiness cannot
replace it with another instance carrying copied tags.

EC2 can temporarily report a newly allocated instance as missing. The existing
readiness wait allows that propagation delay within its ten-minute limit and
honors cancellation. Cleanup keeps a prepared claim and its keys when instance
visibility or termination is uncertain. Retry while the exact instance is
visible; an observed terminal instance permits key recovery. If termination
was accepted but key cleanup failed and the instance is no longer visible,
the prepared claim and keys remain for operator recovery. The existing
already-gone recovery paths for acquired and ordinary leases are unchanged.

Successful stop and exact resource/key cleanup retain a terminal receipt in
the existing claim fields: the original account, region, canonical lease ID,
slug, instance ID, repository path, and versioned intent plus its original
fingerprint label. SSH connectivity, launch attempts, and other active state
are cleared. After inventory no longer includes the instance, a repeated
`stop --provider aws --id cbx_...` can acknowledge that receipt without another
deletion or rewriting it. AWS checks the current caller account and configured
region scope, expected release identity when supplied, and conflicting visible
resources. The release owner reloads and revalidates the receipt and inventory
under the durable claim lock; missing inventory or a terminal provider status
alone never proves cleanup. The recorded repository path is retained as
identity evidence, not a requirement to run stop from the original directory.

After saving the receipt, stop and automatic cleanup close canonical per-lease
SSH control masters and remove the local SSH key and trust files under the same
claim lock. A local cleanup failure returns an error while preserving the
terminal receipt. Fix the reported local problem and repeat
`crabbox stop --provider aws --id cbx_...`; after the same account, region,
identity, and inventory checks, it retries local cleanup without deleting AWS
resources again or rewriting the receipt.

This is a forward repair: older compact tombstones discarded instance and
region binding and cannot prove that scope after upgrading. They remain
unchanged and fail closed on repeated stop. Definitive launch rejections also
block reuse but, without an acquired instance identity, are not successful-stop
receipts. An acquired claim whose instance disappears before successful key
cleanup still makes stop fail; the existing exact AWS cleanup path may finish
those obligations separately. Failed key cleanup keeps the acquired claim.
Receipt replay applies only to canonical fixed-ID stop, not inspect, reuse,
slug lookup, raw instance lookup, or ordinary non-fixed leases.

Automatic AWS cleanup does not prune terminal claims and there is no time-based
operation-ID reuse window. Both retained receipts and older compact tombstones
continue blocking fixed-ID reallocation. Explicit local claim deletion forfeits
this guarantee; normal automation must always use a new operation ID for a
later lease.

Fixed claims and tombstones use the local provider discriminator
`aws-fixed-v1`. Current Crabbox resolves that marker to runtime provider AWS,
while older clients see an unknown provider and therefore skip or refuse
destructive AWS claim cleanup instead of erasing fixed identity state. Cloud
resource provider tags remain `aws`.

### Instance classes

When you pass `--class` instead of `--type`, Crabbox tries an ordered list of
instance types and falls back across families on capacity or quota errors. For
Linux the classes resolve to (first candidate shown):

| Class | First candidate | vCPUs |
| --- | --- | --- |
| `tiny` | `m7a.large` | 2 |
| `small` | `c7a.2xlarge` | 8 |
| `standard` | `c7a.8xlarge` | 32 |
| `fast` | `c7a.16xlarge` | 64 |
| `large` | `c7a.24xlarge` | 96 |
| `beast` (default) | `c7a.48xlarge` | 192 |

Windows and macOS targets use their own candidate lists (Windows WSL2 uses
nested-virtualization families; macOS uses `mac*.metal` types). The default
class is `beast`.

## Provisioning diagnostics

For coordinator-managed groups in the default VPC, VPC discovery and the
[default-VPC group-name lookup](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeSecurityGroups.html)
run together. Both reads finish and the returned group scope is checked before
any ingress change. Subnet-scoped discovery still resolves the subnet's VPC
first; explicitly configured and private-workspace groups keep their existing
lookup paths.

Coordinator AWS create logs use the `crabbox_aws_provisioning` component. Each
fixed operation bucket can include `transport` totals for its signed requests.
Nested operations own their own totals; concurrent creates and preparation
branches remain separate. These are bounded log counters, not stored lease
state or permission decisions.

`requests` counts calls entering credential preparation. `credentialsMs` and
`credentialFailures` cover that preparation; `requestMs` and `requestFailures`
cover the inherited SDK fetch call. A returned HTTP error response is not a
transport failure. The operation's existing error count records how its caller
handled that response. Response-body reading and decoding remain in the outer
operation duration.

`signInvocations`, `signCompletions`, `signFailures` and `signMs` observe the
SDK's public signing method without changing its retry policy. Repeated signing
invocations on a request indicate retry-loop re-entry; a completed signature
alone does not prove that a server received the request. Signing time is part
of `requestMs`, so do not add them together. These totals cannot distinguish
network latency from SDK retry backoff or identify intermediate response status
codes. They do not establish throttling. Requests outside a measured create
operation and qualification-authority RPC transport do not add these totals.

These durations use `Date.now()`. In deployed Cloudflare Workers,
[timers advance only after I/O](https://developers.cloudflare.com/workers/runtime-apis/performance/).
A `0` in `credentialsMs` or `signMs` therefore does not establish zero CPU work
or zero elapsed time. Local Node fixtures use different timer behavior; their
timings validate attribution, not deployed CPU cost. Invocation, completion and
failure counters remain observations independent of this timer limitation.

## Configuration

```yaml
provider: aws
target: linux
architecture: amd64
class: beast
market: spot
aws:
  region: eu-west-1        # default eu-west-1
  ami: ""                  # override the auto-selected AMI
  securityGroupId: ""      # reuse an existing security group
  subnetId: ""             # pin a subnet (and its VPC)
  instanceProfile: ""      # IAM instance profile name to attach
  rootGB: 400              # default 400
  sshCIDRs: []             # allowed SSH source ranges
  macHostId: ""            # pin an EC2 Mac Dedicated Host
```

Set `architecture: arm64` or pass `--arch arm64` for Linux Graviton leases.
Crabbox switches class fallback to C7g/M7g/R7g families and resolves Canonical
Ubuntu ARM64 AMIs unless `aws.ami` is pinned. ARM64 is not supported for managed
Windows or WSL2 targets.

For fresh, automatically selected Canonical Ubuntu 26.04 amd64 images, direct
CLI and public broker provisioning use cloud-init's APT policy to select
`https://archive.ubuntu.com/ubuntu/` as the primary archive. Security updates
retain the separate `http://security.ubuntu.com/ubuntu/` archive. This avoids
relying on the regional EC2 HTTP archive for the normal bootstrap. Suite,
component, signing-key, and source-preservation policies remain image-owned.
Explicit AMIs (including `CRABBOX_AWS_AMI`), promoted images, checkpoint forks,
ARM64, and Ubuntu 24.04 retain their existing source policy. The private AWS
workspace service keeps its separate SSM bootstrap and HTTPS-only source policy.

For brokered SSH access, the coordinator validates the combined lease and global
source ranges, then removes exact duplicates before reconciling each SSH port.
Whitespace is trimmed; distinct IPv4 and IPv6 ranges keep their order. This does
not reuse observed permissions: each unique desired range is still authorized,
and stale-rule pruning and world-access revocation keep their existing policy.
For each port, up to four authorization requests run together after revocation
finishes. Every batch settles before recovery, another batch or the next port;
rule-limit recovery compacts once and retries each affected rule. Diagnostic
request-duration totals include overlapping requests and can exceed wall time.

Access refresh indexes the current and retained leases once. Ports share a
security-group lookup only when their group, allowed source ranges and
reconciliation mode match; different policies remain separate. Runner and
workspace default groups retain distinct identities when stored group names
are absent. Refresh uses the recorded ingress fields directly rather than
rebuilding machine provisioning settings.

### Environment variables (direct mode)

```text
AWS_PROFILE
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN
AWS_REGION
CRABBOX_AWS_REGION                  # overrides AWS_REGION / aws.region
CRABBOX_AWS_AMI
CRABBOX_AWS_SECURITY_GROUP_ID
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB
CRABBOX_AWS_SSH_CIDRS               # comma-separated
CRABBOX_HOST_ID                     # brokered pin requires admin auth except owned released hosts
CRABBOX_AWS_MAC_HOST_ID             # legacy alias for the Mac host id
CRABBOX_CAPACITY_REGIONS            # comma-separated fallback regions
CRABBOX_CAPACITY_AVAILABILITY_ZONES
CRABBOX_CAPACITY_HINTS              # 0 disables brokered capacity hints
```

Notes:

- The standard AWS SDK credential chain applies in direct mode (`AWS_PROFILE`,
  static keys, SSO, etc.). `aws.instanceProfile` is the **IAM instance profile**
  attached to the launched instance, not the local AWS CLI profile.
- `CRABBOX_AWS_REGION` wins over `AWS_REGION` and `aws.region`; the built-in
  default region is `eu-west-1`.
- Region values are trimmed, lowercased, and must match an AWS region name such
  as `eu-west-1`. Broker readiness, lease `awsRegion`, and
  `capacity.regions` reject malformed request values before constructing a
  SigV4 endpoint; invalid environment/config fallback candidates are skipped.
- For brokered AWS, cloud credentials live in the coordinator, not on developer
  machines. Existing Worker deployments can inject static credentials; the
  Node runtime also supports the AWS default credential chain. The dedicated
  ECS private-workspace deployment requires its task role and rejects static
  access keys. See `crabbox config set-broker --provider aws`, the brokered IAM
  policy from `crabbox admin aws-policy`, and
  [Private AWS Workspaces](../features/aws-private-workspaces.md).
- In brokered mode, explicit `aws.ami`, `aws.securityGroupId`, `aws.subnetId`,
  and `aws.instanceProfile` selectors require admin-token authentication.
  Normal broker users receive the coordinator-managed image, network, and
  instance identity. Direct mode keeps these local configuration overrides.

## Private workspace service

Use the dedicated private mode when a service client needs create/status/delete
workspaces in one isolated AWS account and Region without public addresses or
SSH. Its placement is entirely server-side:

- exact expected account and Region, verified through ECS metadata and STS;
- explicit x86_64 instance allowlist and vCPU/memory ceilings, with
  `t3a.small,t3.small` as the recommended starting set;
- 20 GiB encrypted gp3 root volume by default;
- private subnet, no public IP or key pair, IMDSv2, no ingress, TCP 443 egress;
- SSM managed-node readiness, SSM command bootstrap, and CloudWatch log output;
- task-role credentials refreshed through the Node AWS default chain;
- ownership-tagged, generation-fenced, idempotent termination.

The client selects this placement by using the dedicated service URL and
route-scoped bearer. Client target labels or Region-shaped metadata are not
placement controls. The full deployment template, environment contract,
readiness preflight, API, AWS-GO gate, and live canary are in
[Private AWS Workspaces](../features/aws-private-workspaces.md).

## Targets

| Target | Notes |
| --- | --- |
| Linux | Ubuntu bootstrap, SSH, rsync sync, optional desktop/browser/code, Tailscale, Actions hydration. |
| Windows native | EC2Launch bootstrap, OpenSSH, Git for Windows, archive sync; optional desktop with `--desktop`. |
| Windows WSL2 | `--windows-mode wsl2`; launches on nested-virtualization families (`c8i`/`m8i`/`m8i-flex`/`r8i`); POSIX sync and commands run inside WSL with the Linux image Node/npm baseline. |
| macOS | Requires an available EC2 Mac Dedicated Host in the region; On-Demand only. Admin-authenticated broker requests can pin any host with `CRABBOX_HOST_ID` / `aws.macHostId` (`CRABBOX_AWS_MAC_HOST_ID` is a legacy alias); normal broker users can pin only a host from their own released lease and otherwise use automatic discovery. |

Managed WSL2 commands run as the non-root `crabbox` Linux user, with
`HOME=/home/crabbox`, Bash, passwordless sudo, and membership in the `sudo` and
`docker` groups. This user owns the work root and `/var/cache/crabbox`.
Bootstrap sets `[user] default=crabbox` in `/etc/wsl.conf`, preserving other
distro settings, so staged commands, WSL sessions, sync/copy, readiness, and
workspace ownership share one identity. Root is used only for distro setup;
the Windows SSH transport account is unchanged. Existing leases need
reprovisioning to receive this setup.

Managed WSL2 bootstrap runs the Node-only entrypoint of
`scripts/install-linux-developer-tools.sh`, bundled in the CLI/coordinator rather
than downloaded at boot. It uses the same Node major policy and, on the managed
amd64 distro, the same checksum-pinned Node 24.19.0 archive as Linux developer
images. Node and npm are on the default command PATH and required by readiness.
Re-bootstrap verifies the cached archive and reinstalls the owned version slot.
The installer logs the Node step's elapsed seconds during warmup.

WSL2 installs this subset to keep warmup bounded: it does not install the full
Linux image's Docker, browser/desktop tools, Go, or offline pnpm archive set.
Projects needing those tools should use their setup scripts or Actions hydration.

Managed WSL2 distributions disable cloud-init because Crabbox owns their setup.
This avoids WSL datasource discovery blocking systemd and root login during
later command invocations. Bootstrap restarts the distro and verifies its Linux
ready check before publishing the setup marker; warmup also checks the WSL SSH
runtime. `inspect` and `status` allow a 30-second WSL2 readiness probe.

Managed WSL2 leases set `[general] instanceIdleTimeout=-1` in the Windows SSH
user's `.wslconfig` so detached Linux daemons survive between commands until
lease cleanup. Command deadlines and lease expiration still apply.

Headless managed WSL2 leases also disable WSLg with `guiApplications=false` in
the Windows SSH user's `.wslconfig`, preserving other settings. The GUI/RDP
compositor is unnecessary for these leases and can crash in a Windows service
session, blocking later Linux commands and workspace-owner renewal. Explicit
desktop or browser requests retain their existing GUI configuration. Bootstrap
applies a changed WSL configuration before starting the distro; existing leases
need reprovisioning to receive this policy. Command execution and owner-control
timeout budgets are unchanged.

## Normal SSH lifecycle

1. Import or reuse the per-lease SSH key (RSA for native Windows, ed25519
   otherwise).
2. Select region, market, instance type, subnet, AMI, and security group.
3. Launch the EC2 instance — Spot request, On-Demand instance, native Windows
   instance, or EC2 Mac host-backed instance.
4. Tag the instance, volumes, and Spot requests with Crabbox lease labels.
5. Wait for SSH readiness, plus the `crabbox-ready` marker on POSIX targets.
6. Hand off to core for sync and command execution over SSH.
7. Terminate on release, cleanup, or broker expiry.

Brokered cleanup is owned by the Worker (lease expiry plus an AWS orphan
sweep). Direct cleanup is best-effort via provider labels and
`crabbox cleanup --provider aws`.

AWS also supports the provider-neutral retained run handle. Add
`--lease-output <file>` to a retained run to record the exact lease ID, slug,
coordinator run ID when brokered, and cleanup command before sync or command
execution begins. A reused `--id` lease is retained by default; a newly created
lease also requires `--keep`. The handle remains useful after a later command
failure, but only a coordinator-backed run ID can be used to retrieve a signed
terminal receipt.

## Checkpoints

AWS supports provider-native checkpoints in addition to workspace archives:

- Linux/macOS, image strategy or macOS target → AWS AMI
  (`checkpoint --kind native`, kind `aws-ami`).
- Linux disk-snapshot strategy → EBS snapshot (`aws-ebs-snapshot`).
- Native Windows targets do not support native checkpoints.

New brokered native checkpoints are coordinator-owned and manually retained by
default. Opt into inactivity expiry with
`checkpoint create --expire-unused-after 7d` or `checkpoint policy`. Ownership
binds the exact AWS account, region, AMI/EBS snapshot identity, and source
lease. AMI deletion durably retains every owned backing snapshot ID before
deregistration and is complete only after the exact AMI and all backing
snapshots are gone. Promoted defaults, scoped catalog roles, and catalog-only
variants pin managed AMIs against expiry/deletion. Direct AWS checkpoints and
historical images remain local/operator-managed.

In brokered mode you can promote and warm AMIs:

- `crabbox image promote` promotes a brokered AMI for a target/region.
- `crabbox image fsr-status --provider aws` reports Fast Snapshot Restore state.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Desktop / browser / code: yes, target-dependent.
- Tailscale: Linux managed leases.
- Actions hydration: Linux SSH leases only.
- Retained run-session handle: yes.
- Coordinator (broker): supported.

## Gotchas

- Spot capacity and quota errors are normal. Prefer `--class` over an exact
  `--type` when you want fallback.
- Run `crabbox doctor --provider aws` before the first warmup in a new or
  unfunded account. Doctor reads EC2 vCPU Service Quotas for the effective
  class/type and recommends a smaller class/type when `beast` would exceed the
  account cap.
- `beast` starts at 48xlarge candidates and can consume up to 192 vCPUs per
  request. Under capacity pressure, prefer `standard` or `fast` plus several
  `CRABBOX_CAPACITY_REGIONS`.
- Brokered leases include capacity hints unless disabled with
  `capacity.hints: false` or `CRABBOX_CAPACITY_HINTS=0`.
- Windows WSL2 requires nested-virtualization families. An exact `--type` must
  be a `c8i`/`m8i`/`m8i-flex`/`r8i` instance; `m7`/`t3`-style Windows types are
  rejected before leasing.
- EC2 Mac requires an allocated Dedicated Host in the selected region and is
  On-Demand only.
- VNC stays behind SSH tunnels; never expose VNC ports directly.

## Related docs

- [AWS feature notes](../features/aws.md)
- [Private AWS workspaces](../features/aws-private-workspaces.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
- [Provider backends](../provider-backends.md)
