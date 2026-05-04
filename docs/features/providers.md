# Providers

Read when:

- changing Hetzner, AWS, or Blacksmith Testbox provisioning;
- adding a backend;
- adjusting machine classes, fallback order, regions, or images.

Crabbox currently supports two brokered providers:

```text
hetzner     Hetzner Cloud servers
aws         AWS EC2 instances
```

Brokered Hetzner leases are Linux targets. Brokered AWS supports Linux, native
Windows Server, and EC2 Mac when a Dedicated Host is configured. Static SSH
still exists for reusing existing macOS and Windows machines:

```text
ssh         Existing SSH host selected by static.host
```

## Provider Pages

- [AWS](aws.md): EC2 Linux, Windows, WSL2, EC2 Mac, capacity, AMIs, and security groups.
- [Hetzner](hetzner.md): Linux-only managed provider behavior, classes, and cleanup.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox backend behavior.

## Hetzner Summary

- imports or reuses the lease SSH key;
- creates a server with Crabbox labels;
- uses configured image and location;
- falls back across class server types when capacity or quota rejects a request;
- fetches server-type hourly prices when cost estimates need provider pricing.

## AWS Summary

- signs EC2 Query API calls inside the Worker;
- imports or reuses an EC2 key pair;
- creates or reuses the `crabbox-runners` security group with SSH ingress limited to configured CIDRs or the request source IP;
- launches one-time Linux Spot or On-Demand instances;
- launches AWS Windows Server desktop leases with EC2Launch PowerShell user
  data, OpenSSH, Git for Windows, and TightVNC when `target=windows`;
- launches EC2 Mac leases only with an explicit Dedicated Host id
  (`CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`) and On-Demand capacity;
- tags instances, volumes, and Spot requests;
- falls back across broad C/M/R instance families for class requests, including account policy and capacity rejections;
- can fall back to a small burstable type when account policy rejects the high-core class candidates;
- preflights applied Spot or On-Demand vCPU quotas in brokered mode when Service Quotas allows it, then records skipped candidates as quota attempts;
- supports `--market spot|on-demand` on `warmup` and `run` for one-off capacity-market overrides;
- uses Spot placement score across configured regions in direct AWS mode;
- can fall back to On-Demand after Spot capacity/quota failures when configured;
- fetches Spot price history when cost estimates need provider pricing.

Explicit `--type` requests are treated as exact provider type requests. If that type is rejected, Crabbox fails clearly instead of silently choosing a different instance type. Remove `--type` and use a machine class when fallback is desired.

`crabbox list` marks brokered provider machines as `orphan=no-active-lease`
when their provider label references a lease that is no longer active in the
coordinator. This is an operator hint only; `keep=true` machines are not
deleted automatically.

Machine classes map to provider-specific types:

```text
Hetzner
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53

AWS
standard  c7a.8xlarge, c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge
fast      c7a.16xlarge, c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge
large     c7a.24xlarge, c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge
beast     c7a.48xlarge, c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, c7i.32xlarge, m7a.32xlarge, c7a.24xlarge, c7a.16xlarge

AWS Windows
standard  m7i.large, m7a.large, t3.large
fast      m7i.xlarge, m7a.xlarge, t3.xlarge
large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge
beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge

AWS Windows WSL2
standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge
large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge
beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge

AWS macOS
all       mac2.metal unless `--type` is set
```

Direct provider mode still exists when no coordinator is configured. It uses local AWS credentials or `HCLOUD_TOKEN`/`HETZNER_TOKEN` and should stay secondary to the brokered path.

Tailscale is not a provider. Use `--tailscale` to add tailnet reachability to
new managed Linux leases, or set a static host to a MagicDNS name/100.x address
when the existing host is already on a tailnet. See [Tailscale](tailscale.md).

Direct smoke shape:

```sh
tmp="$(mktemp)"
printf 'provider: hetzner\n' > "$tmp"
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox warmup --provider hetzner --class standard --ttl 15m --idle-timeout 4m
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox run --provider hetzner --id <slug> --no-sync -- echo direct-hetzner-ok
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox stop --provider hetzner <slug>
rm -f "$tmp"
```

Use `--provider aws` with AWS SDK credentials for direct AWS smoke. Direct mode
has no Durable Object alarm; cleanup is best-effort through provider labels and
manual `crabbox cleanup`. Direct AWS fallback can retry provider types, but the
structured quota preflight and `provisioningAttempts` metadata belong to the
brokered Worker path.

Crabbox can also wrap Blacksmith Testboxes with `provider: blacksmith-testbox`. That backend does not use the Crabbox broker or direct cloud credentials. It shells out to the authenticated Blacksmith CLI for `testbox warmup`, `run`, `status`, `list`, and `stop`, while Crabbox keeps local slugs, repo claims, config, and timing summaries. See [Blacksmith Testbox](blacksmith-testbox.md).

Static SSH targets:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: steipete
  port: "22"
  workRoot: /Users/steipete/crabbox
```

```yaml
provider: ssh
target: windows
windows:
  mode: normal
static:
  host: win-dev.local
  user: Peter
  port: "22"
  workRoot: C:\crabbox
```

`target: windows` supports `windows.mode: normal` and `windows.mode: wsl2`.
Normal mode uses PowerShell over OpenSSH and syncs the manifest as a tar archive.
WSL2 mode requires AWS nested virtualization, so managed AWS WSL2 leases use
C8i, M8i, or R8i families and enable nested virtualization at launch. Static
WSL2 hosts keep the POSIX SSH contract: commands run through
`wsl.exe --exec bash -lc`, rsync uses `wsl.exe rsync`, and `static.workRoot`
should be a WSL path such as `/home/peter/crabbox`. macOS also uses the POSIX
contract and needs `git`, `rsync`, `tar`, and SSH.

Related docs:

- [Infrastructure](../infrastructure.md)
- [AWS](aws.md)
- [Hetzner](hetzner.md)
- [Tailscale](tailscale.md)
- [Blacksmith Testbox](blacksmith-testbox.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Cost and usage](cost-usage.md)
