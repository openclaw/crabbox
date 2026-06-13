# Configuration

Read when:

- adding a new config key, env override, or flag;
- debugging "why is Crabbox using value X here?";
- onboarding a repo and deciding what belongs in repo config vs user config;
- reviewing the YAML schema that `crabbox config show` and `crabbox init` emit.

Crabbox configuration is layered. The CLI loads values from several sources and
merges them in a deterministic order. Every source is optional - the binary
boots with sane defaults for everything.

## Precedence

```text
flags > env > repo-local crabbox.yaml/.crabbox.yaml > user config > defaults
```

Lowest precedence is applied first: defaults, then user config, then repo
config, then env vars, then flags. Each layer only overrides fields that are
explicitly set; unset fields fall through to the layer below.

Two commands expose the resolved view:

- `crabbox config show` prints the merged configuration as the CLI sees it
  after every layer runs. `--json` is stable enough to diff in scripts, and
  neither form prints secret values (tokens render as a state such as
  `configured` or `missing`).
- `crabbox config path` prints the user config file path so other tools can
  edit it without parsing prose.

## File locations

```text
macOS user:    ~/Library/Application Support/crabbox/config.yaml
Linux user:    ~/.config/crabbox/config.yaml
XDG override:  $XDG_CONFIG_HOME/crabbox/config.yaml
repo:          ./crabbox.yaml or ./.crabbox.yaml at the repo root
explicit:      $CRABBOX_CONFIG (any path)
```

The user config path comes from the OS user-config directory (`os.UserConfigDir`).
If both `crabbox.yaml` and `.crabbox.yaml` exist at the repo root, both are
read (in that order).

If `CRABBOX_CONFIG` is set, it replaces the entire file search: only that file
is loaded, and neither the user config nor the repo-local files are read. The
CLI writes new user config (for example via `crabbox login` or
`crabbox config set-broker`) with `0600` permissions and warns if an existing
file is group- or world-readable.

State that does not belong in either YAML file:

- live lease records (managed records are coordinator-owned; registered records
  are provider-owned and mirrored to the coordinator);
- per-lease SSH private keys (they live under the user config dir, but not in
  `config.yaml`);
- provider secrets (managed-provider secrets live in the broker environment;
  direct-provider secrets stay in your shell env or credential manager).

## YAML schema

The schema below merges what `crabbox init` emits with what advanced operators
set in user config. Most repos only need a small subset.

### Top-level

```yaml
broker:
  url: https://broker.example.com
  mode: managed             # managed | registered
  autoWebVNC: true          # registered kept desktop leases only
  provider: aws
  token: <signed-github-token-or-shared-token>
  adminToken: <broker-admin-token>     # operators only
  access:
    clientId: <cloudflare-access-service-token-id>
    clientSecret: <cloudflare-access-service-token-secret>

provider: aws            # default provider when --provider is unset
target: linux            # default target OS: linux | macos | windows
architecture: amd64      # amd64 | arm64; arm64 supports Linux on AWS/Azure and native Windows on Azure
os: ubuntu:26.04         # OS image; resolved to per-provider images for linux
windows:
  mode: normal           # normal | wsl2 when target=windows

profile: project-check
class: beast             # standard | fast | large | beast
serverType: c7a.48xlarge # explicit provider type; overrides class fallback
network: auto            # auto | tailscale | public
hostId: h-0123456789abcdef0

lease:
  idleTimeout: 30m
  ttl: 90m
```

`broker.mode` defaults to `managed`. In `registered` mode, every direct SSH
lease keeps its existing provider lifecycle and is registered with the broker
for owner-scoped inventory, sharing, heartbeats, and portal bridges. Registration
is best effort: a broker outage does not fail local provisioning. Releasing the
local lease removes its registration, and coordinator release/expiry never
invokes provider deletion. `CRABBOX_COORDINATOR_MODE` and
`CRABBOX_COORDINATOR_AUTO_WEBVNC` provide environment overrides.

`ttl`, `idleTimeout`, and `workRoot` are also accepted as top-level keys. The
default class is `beast`, the default TTL is `90m`, and the default idle
timeout is `30m`. Setting `serverType` (or `--type` on the command line) pins
that exact provider type and disables class fallback.

### Profiles and presets

Profiles are repo-local contracts for real validation lanes. Keep project
knowledge in YAML, not in the Crabbox binary: runtime prerequisites,
environment defaults, artifact globs, command presets, and proof wording all
belong here.

```yaml
profile: live-qa
profiles:
  live-qa:
    env:
      CI: "1"
      NODE_OPTIONS: "--max-old-space-size=4096"
      allow:
        - QA_*
    envAllow:
      - QA_TEST_PROJECTS_PARALLEL
    artifactGlobs:
      - ".artifacts/qa-e2e/**"
    doctor:
      enabled: true
      tools: [node, corepack, pnpm]
      nodeMajor: 22
      minDiskGB: 40
      requireDocker: true
      requireCompose: true
    presets:
      qa-live:
        command: >
          pnpm qa live
          --scenario {{scenario}}
          --fail-fast
        env:
          VITEST_NO_OUTPUT_TIMEOUT_MS: "900000"
          QA_SERVICE_TIMEOUT_MS: "3000"
        preflight: true
        proofTemplate: real-behavior-pr
    proofTemplates:
      real-behavior-pr:
        behaviorAddressed: "Live QA scenario {{scenario}}"
        realEnvironmentTested: "AWS Crabbox {{leaseId}} ({{slug}}) with disposable service topology."
        exactSteps: "{{command}}"
        observedResult: "The named E2E scenario completed successfully."
        notTested: "No external public service; deterministic disposable services were used."
```

Run with:

```sh
crabbox run --provider aws --profile live-qa --preset qa-live --scenario login-regression --emit-proof /tmp/proof.md --stop-after success
```

Preset commands are expanded before the remote run and printed for
auditability. `{{scenario}}` comes from `--scenario`; additional placeholders
come from repeatable `--preset-var name=value`.

`profiles.<name>.env` sets literal environment defaults for that profile. The
nested `profiles.<name>.env.allow` shape is also accepted and is merged with
`profiles.<name>.envAllow`. Presets and proof templates may also be declared at
the top level (`presets:`, `proofTemplates:`) for use across profiles.

`--profile` is also a plain provider/coordinator label and does not require a
matching `profiles:` block. When a matching block exists, Crabbox applies the
profile metadata above; provider sizing, sync rules, and lease TTL stay at
their normal top-level or job locations.

### Jobs

Named jobs live in repo config and describe reusable Crabbox orchestration, not
project logic baked into the binary. Use them for common "warm a box, hydrate
it with GitHub Actions, run the repo command, clean up" flows. See
[Jobs](jobs.md) for lifecycle details and the field contract.

```yaml
jobs:
  windows-wsl2:
    provider: aws
    target: windows
    architecture: amd64
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
      githubRunner: false
      waitTimeout: 45m
      keepAliveMinutes: 240
    actions:
      workflow: hydrate.yml
      job: hydrate
    shell: true
    command: >
      corepack enable &&
      pnpm install --frozen-lockfile &&
      CI=1 NODE_OPTIONS=--max-old-space-size=4096 pnpm test
    stop: always
```

Run with:

```sh
crabbox job run windows-wsl2
```

`crabbox job run --dry-run <name>` prints the underlying `warmup`,
`actions hydrate`, `run`, and `stop` commands.

Set `hydrate.githubRunner: true` only when the workflow needs GitHub-hosted
runner semantics such as secrets, OIDC, service containers, or unsupported
Actions features. When `hydrate.actions` is false or omitted, `job run` passes
`--no-hydrate` to the nested `run` even if the global profile configures
`actions.workflow`.

### Capacity

```yaml
capacity:
  market: spot           # spot | on-demand
  strategy: most-available
  fallback: on-demand-after-120s
  hints: true
  regions:
    - eu-west-1
    - us-east-1
  availabilityZones:
    - eu-west-1a
    - eu-west-1b
```

See [Capacity and fallback](capacity-fallback.md) for how regions, markets, and
the fallback policy are applied during provisioning.

### AWS

```yaml
aws:
  region: eu-west-1
  ami: ami-0123456789abcdef0
  securityGroupId: sg-0abcdef0123456789
  subnetId: subnet-0abcdef0123456789
  instanceProfile: crabbox-runner
  rootGB: 400
  sshCIDRs:
    - 203.0.113.0/24
  macHostId: h-0123456789abcdef0
hostId: h-0123456789abcdef0
```

The default region is `eu-west-1` and the default root volume is `400` GB.
`aws.macHostId` is the AWS-specific allocated EC2 Mac Dedicated Host and
seeds the generic `hostId` when that is unset; `target: macos` requires one of
`hostId`, `aws.macHostId`, `CRABBOX_HOST_ID`, or `CRABBOX_AWS_MAC_HOST_ID`.

### Azure

```yaml
provider: azure
azure:
  backend: vm           # vm | dynamic-sessions
  location: eastus
  resourceGroup: crabbox-leases
  osDisk: managed       # managed | ephemeral | ephemeral-preview | auto
  vnet: crabbox-vnet
  subnet: crabbox-subnet
  nsg: crabbox-nsg
```

Azure uses managed `StandardSSD_LRS` OS disks by default so leases can support
native disk-snapshot checkpoints. `ephemeral` opts into local OS disks for
stateless leases and disables native Azure checkpoint/fork support.
`ephemeral-preview` opts into Azure's public-preview full-caching ephemeral OS
disk mode and skips known unsupported Crabbox Azure SKUs. `auto` is accepted for
compatibility and resolves to managed.

### Azure Dynamic Sessions

```yaml
provider: azure-dynamic-sessions
target: linux
azureDynamicSessions:
  endpoint: https://<pool>.<environment-id>.eastus.azurecontainerapps.io
  workdir: /workspace/crabbox
```

Use a custom container session pool whose image exposes the Crabbox runner on
port `8787`. Auth uses `az account get-access-token --resource
https://dynamicsessions.io` unless `CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN` is
set.

### Cloudflare Dynamic Workers

```yaml
provider: cloudflare-dynamic-workers
target: worker-runtime
cloudflareDynamicWorkers:
  compatibilityDate: "2026-06-12"
  compatibilityFlags:
    - nodejs_compat
  cacheMode: stable
  egress: blocked
  cpuMs: 50
  subrequests: 12
  timeoutSecs: 30
```

`cloudflare-dynamic-workers` is a delegated Worker-runtime provider. It runs
module source supplied through `crabbox run --script <file>` or
`--script-stdin`; it does not expose a Linux shell, SSH, rsync/archive sync, VNC,
ports, browser, code-server, or Actions hydration.

Use `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL` (or the compatibility alias
`CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_LOADER_URL`) for the loader URL and
`CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN` for bearer auth. Keep the token in
environment or private user config, not repo YAML and not argv. Repository
config cannot replace the loader URL or token, enable `intercept` egress, or
loosen trusted `cpuMs`, `subrequests`, or `timeoutSecs` limits. It may tighten
those limits.
Provider flags cover runtime settings such as
`--cloudflare-dynamic-workers-compatibility-date`,
`--cloudflare-dynamic-workers-compatibility-flags`,
`--cloudflare-dynamic-workers-cache`,
`--cloudflare-dynamic-workers-egress`,
`--cloudflare-dynamic-workers-cpu-ms`,
`--cloudflare-dynamic-workers-subrequests`, and
`--cloudflare-dynamic-workers-timeout-secs`. There is intentionally no token
flag.

### Anthropic Sandbox Runtime

```yaml
provider: anthropic-sandbox-runtime
anthropicSandboxRuntime:
  cliPath: srt
  settings: "" # empty means Anthropic Sandbox Runtime default ~/.srt-settings.json
  debug: false
```

`anthropic-sandbox-runtime` is a local one-shot delegated-run provider. It
shells out to Anthropic Sandbox Runtime with
`srt [--debug] [--settings <path>] -c <command>`. Use
`--anthropic-sandbox-runtime-cli`, `--anthropic-sandbox-runtime-settings`, and
`--anthropic-sandbox-runtime-debug` for command-line overrides, or
`CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI`,
`CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS`, and
`CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG` for environment overrides. Crabbox
validates the provider config keys; Anthropic Sandbox Runtime validates its own
settings JSON and enforcement policy.

### Hetzner

Hetzner credentials and image come from broker-side config. Repos do not need a
`hetzner:` block unless they pin a location or image:

```yaml
provider: hetzner
hetzner:
  location: fsn1
  image: <hetzner-image>
```

### Hostinger

```yaml
provider: hostinger
target: linux
hostinger:
  apiUrl: https://developers.hostinger.com
  itemId: "<hostinger-priced-item-id>"
  paymentMethodId: "<hostinger-payment-method-id>"
  templateId: "<hostinger-template-id>"
  dataCenterId: "<hostinger-data-center-id>"
  hostnamePrefix: crabbox
  user: root
  workRoot: /work/crabbox
  allowPurchase: false
  releaseAction: stop
```

Keep `HOSTINGER_API_TOKEN` or `CRABBOX_HOSTINGER_API_TOKEN` in the shell or a
private user config, not in repo YAML or command lines. `itemId` is the
purchasable priced item id, for example `hostingercom-vps-kvm2-usd-1m`, not the
parent catalog family id. `allowPurchase` defaults to `false`; billable `warmup`
and `run` operations require
`--hostinger-allow-purchase`, `CRABBOX_HOSTINGER_ALLOW_PURCHASE=true`, or
`hostinger.allowPurchase: true` in the private user config selected by
`CRABBOX_CONFIG`. Repo-local config cannot authorize a purchase. Release action
is stop-only (`releaseAction: stop`), and Hostinger billing/subscription state
remains account-owned after Crabbox release. Repo-local config also cannot
set the API token, API URL, item, payment method, template, data center, or
purchase opt-in. Put those account and purchase selections in environment
variables, CLI flags, private user config, or an explicit `CRABBOX_CONFIG`
file. `paymentMethodId` may be omitted only when the account has exactly one
active default payment method; otherwise set it explicitly. Use
`crabbox doctor --provider hostinger --json` to discover available ids without
making a purchase.

### Google Cloud

```yaml
provider: gcp
gcp:
  project: example-project
  zone: europe-west2-a
  network: default
  rootGB: 400
```

### Proxmox

```yaml
provider: proxmox
proxmox:
  apiUrl: https://pve.example.test:8006
  tokenId: crabbox@pve!ci
  node: pve1
  templateId: 9000
  storage: local-lvm
  bridge: vmbr0
  user: crabbox
  workRoot: /work/crabbox
```

Put `tokenSecret` in a private config file or use
`CRABBOX_PROXMOX_TOKEN_SECRET`; do not pass it as a command-line flag.

### XCP-ng

```yaml
provider: xcp-ng
target: linux
xcpNg:
  apiUrl: https://xcp-pool.example.test
  username: crabbox@example.test
  password: <api-password>
  template: crabbox-ubuntu-2404
  templateUuid: ""
  sr: default-sr
  srUuid: ""
  network: pool-network
  networkUuid: ""
  host: ""
  user: crabbox
  workRoot: /work/crabbox
  insecureTLS: false
```

`apiUrl`, `username`, `password`, and either `sr` or `srUuid` are required for
XCP-ng doctor and lifecycle commands. `template` or `templateUuid` is required
before `warmup` or `run` can create a lease. The password must come from private
config or `CRABBOX_XCP_NG_PASSWORD`; there is intentionally no
XCP-ng password command-line flag. Prefer a pool-master `apiUrl`; if XAPI
returns `HOST_IS_SLAVE`, Crabbox retries login once against the reported master.
For credential safety, repository-local `crabbox.yaml` and `.crabbox.yaml`
files cannot override `apiUrl` or `insecureTLS`; set those in user config, an
explicit `CRABBOX_CONFIG` file, or environment variables.

`target: linux` describes the current Crabbox lease surface, not an XCP-ng
hypervisor limitation. XCP-ng itself can host Linux, Windows, and BSD guests on
dedicated 64-bit x86 server-class hardware, but Crabbox's normal `xcp-ng` flow
provisions Linux templates only. The separate XCP-ng ISO E2E harness also
covers Windows x86_64/x64 installer media. Use the Tart provider on Apple
hardware for macOS VM workflows.

`network`, `networkUuid`, and `host` are optional placement hints. When
`network` or `networkUuid` is set, Crabbox moves all VIFs on the copied VM to
that network. Prefer a single-NIC template for Crabbox-managed VMs, or leave
both unset when the template's existing network topology should be preserved.

The current SR-backed lifecycle uses `VM.copy`, then `VM.provision`, and then
attaches a FAT16 `CIDATA` config-drive image. Keep `apiUrl` on an
administrator-only management network or VPN, prefer trusted certificates, and
limit `insecureTLS` to private lab environments.

Environment overrides:

```text
CRABBOX_XCP_NG_API_URL
CRABBOX_XCP_NG_USERNAME
CRABBOX_XCP_NG_PASSWORD
CRABBOX_XCP_NG_TEMPLATE
CRABBOX_XCP_NG_TEMPLATE_UUID
CRABBOX_XCP_NG_SR
CRABBOX_XCP_NG_SR_UUID
CRABBOX_XCP_NG_NETWORK
CRABBOX_XCP_NG_NETWORK_UUID
CRABBOX_XCP_NG_GUEST_CIDR
CRABBOX_XCP_NG_HOST
CRABBOX_XCP_NG_USER
CRABBOX_XCP_NG_WORK_ROOT
CRABBOX_XCP_NG_INSECURE_TLS
```

Set `CRABBOX_XCP_NG_GUEST_CIDR` to an IPv4 `/24` or narrower range attached
to the local runner only when guest tools cannot report an address and active
MAC discovery is required. Crabbox never sweeps all local interfaces.

### Static SSH

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

`static`, `static-ssh`, and `ssh` all select the bring-your-own-host backend.

### Local container

```yaml
provider: local-container
localContainer:
  runtime: docker
  image: debian:bookworm
  user: crabbox
  workRoot: /work/crabbox
  cpus: 0
  memory: ""
  network: bridge
  dockerSocket: false
```

`provider: docker`, `provider: container`, and `provider: local-docker` are
aliases for `local-container`. The backend uses Docker-compatible CLI commands,
so Docker Desktop, OrbStack, Colima, Podman, and similar local runtimes work.
Crabbox detects an installed `docker` or `podman` CLI and uses that runtime; if
both are present, `docker` is selected unless `localContainer.runtime` is set
explicitly. Set `dockerSocket: true` only when commands inside the lease must
use the host Docker-compatible API; Crabbox then mounts the active local Unix
socket from `DOCKER_HOST` or the Docker context and rejects remote TCP contexts.
With the socket enabled and no explicit work root, Crabbox chooses a host-visible
cache work root so nested bind mounts can see the synced checkout.

Use `--desktop --browser` to bootstrap Xvfb, XFCE, x11vnc, noVNC/websockify,
desktop input tools, screenshot tools, ffmpeg, and a packaged browser inside
the container.

### Apple VZ

```yaml
provider: apple-vz
appleVZ:
  # Optional for normal Homebrew/release installs.
  helperPath: /custom/path/crabbox-apple-vz-helper
  image: https://cloud-images.ubuntu.com/releases/resolute/release-20260520/ubuntu-26.04-server-cloudimg-arm64.img
  imageSHA256: 5e091e27d60116efbb0c743b8dd5cb2d15618e414ef04db0817ed43c8e2d7c7b
  user: crabbox
  workRoot: /work/crabbox
  cpus: 4
  memoryMiB: 8192
  diskGiB: 30
```

`provider: applevz` is an alias for `apple-vz`. The backend drives a small
local helper that boots a headless Linux VM with Apple's
`Virtualization.framework`, then exposes guest SSH through a host-local proxy so
Crabbox can use the normal SSH sync and run path. The image default follows the
portable `osImage` selector unless `appleVZ.image` is set explicitly. Default
remote images include pinned SHA-256 checksums; custom remote image URLs must
set `appleVZ.imageSHA256`, while local image paths may omit it. Apple Silicon
Homebrew bottles and release archives install the helper beside `crabbox`;
`helperPath` is only needed for a custom or source-built helper. The effective
architecture defaults to `arm64`, and explicit `amd64` is rejected.

### Multipass

```yaml
provider: multipass
multipass:
  cliPath: multipass
  image: "26.04"
  user: crabbox
  workRoot: /work/crabbox
  cpus: 4
  memory: 8G
  disk: 30G
  launchTimeout: 20m
```

`provider: mp` and `provider: canonical-multipass` are aliases for `multipass`.
The backend drives Canonical's `multipass` CLI on the local workstation, launches
an Ubuntu VM with cloud-init, discovers the VM IP with `multipass info`, then
uses the normal Crabbox SSH sync/run path. The Multipass image follows the
portable `osImage` default unless `multipass.image` is set explicitly.

### Blacksmith Testbox

```yaml
provider: blacksmith-testbox
blacksmith:
  org: example-org
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
  debug: false
```

### Namespace Devbox

```yaml
provider: namespace-devbox
namespace:
  image: builtin:base
  size: M
  repository: github.com/example-org/my-app
  site: ""
  volumeSizeGB: 100
  autoStopIdleTimeout: 30m
  workRoot: /workspaces/crabbox
  deleteOnRelease: false
```

### Namespace Compute Instance

```yaml
provider: namespace-instance
namespaceInstance:
  cli: nsc
  machineType: 4x8
  duration: 30m
  region: ""
  endpoint: ""
  keychain: ""
  volumes: []
  workRoot: /work/crabbox
  bare: true
```

The `namespace` section remains specific to `namespace-devbox`.
`namespaceInstance` configures the separate `nsc`-backed Compute provider.
For repository-local config, Crabbox ignores `cli`, `endpoint`, `region`,
`keychain`, and `volumes`; set those through trusted user config, environment
variables, or explicit flags.

### KubeVirt

```yaml
provider: kubevirt
kubevirt:
  kubectl: kubectl
  virtctl: virtctl
  kubeconfig: ""
  context: ""
  namespace: default
  template: ./kubevirt-vm.yaml
  sshUser: crabbox
  sshKey: ""
  sshPublicKey: ""
  sshPort: "22"
  workRoot: /home/crabbox/crabbox
  deleteOnRelease: true
```

The template must be one KubeVirt `VirtualMachine` using `runStrategy: Manual`.
Crabbox sets its name, namespace, and lease labels, replaces documented
placeholders, applies it with `kubectl`, and starts it with `virtctl`.

### External provider

Protocol adapter:

```yaml
provider: external
external:
  command: node
  args:
    - /absolute/path/provider.mjs
  config:
    backend: vm
    namespace: team-devboxes
  workRoot: /workspaces/crabbox
  routingFile: ""
```

The executable receives one versioned JSON request on stdin per lifecycle
operation and returns one JSON response on stdout. This keeps internal control
plane logic outside Crabbox while preserving normal SSH sync, rsync, WebVNC,
and command execution. Crabbox writes private per-lease routing state for
generated stop commands; `routingFile` is normally set only by those commands.

Declarative CLI:

```yaml
provider: external
external:
  lifecycle:
    acquire:
      steps:
        - [devboxctl, new, "{{resourceName}}", --size, "{{config.size}}"]
        - [devboxctl, setup, "{{resourceName}}"]
      rollbackOnFailure: true
      env:
        DEVBOX_TOKEN: "{{env.DEVBOX_TOKEN}}"
    list:
      argv: [devboxctl, list, --format, json]
      output: json-name-array
      namePrefix: "cbx-"
    release:
      argv: [devboxctl, rm, --yes, "{{resourceName}}"]
  connection:
    resourceName: "{{leaseIdSlug}}"
    cloudId: devboxes/{{resourceName}}
    serverType: "{{config.size}}"
    ssh:
      user: "{{env.DEVBOX_USER}}"
      host: "{{resourceName}}"
      sshConfigProxy: true
  config:
    size: cpu16
  workRoot: /home/developer/crabbox
```

Declarative lifecycle entries use one `argv` array or an ordered `steps` list,
not shell commands. Acquire steps can opt into release cleanup with
`rollbackOnFailure: true`. Put credentials in an operation `env:` map, not in
`argv` or `steps`; environment-derived argv values require the explicit
`allowEnvArgv: true` compatibility opt-in, and environment-derived resource
names require `connection.allowEnvResourceName: true`. See
[External Provider](../providers/external.md) for placeholders, output
semantics, inventory formats, routing behavior, and security guidance.

### Daytona

```yaml
provider: daytona
daytona:
  snapshot: my-app-base
  workRoot: /home/daytona/crabbox
```

Authenticate with `DAYTONA_API_KEY` / `DAYTONA_JWT_TOKEN` /
`DAYTONA_ORGANIZATION_ID` or an authenticated Daytona CLI profile. Keep API
keys in the shell or a credential manager, not in YAML.

### exe.dev

```yaml
provider: exe-dev
exeDev:
  controlHost: exe.dev
  image: ""
  cpus: 2
  memory: 4GB
  disk: 10GB
  command: ""
  user: ""
  workRoot: /tmp/crabbox
  noEmail: true
```

Authenticate with `ssh exe.dev`; repo config should select VM sizing and work
root only. VM creation requires an active exe.dev plan.

### E2B

```yaml
provider: e2b
e2b:
  template: base
  workdir: crabbox
  apiUrl: https://api.e2b.app
  domain: e2b.app
```

Keep `E2B_API_KEY` or `CRABBOX_E2B_API_KEY` in the shell or credential manager.
Repo config should select templates and workdirs, not hold API keys.

### Modal

```yaml
provider: modal
modal:
  app: crabbox
  image: python:3.13-slim
  workdir: /workspace/crabbox
  python: python3
```

Authenticate the local Modal Python client with `python3 -m modal setup` or
`MODAL_TOKEN_ID` / `MODAL_TOKEN_SECRET`. Repo config should select app/image and
workdir only; tokens do not belong in YAML or command-line flags.

### Cloudflare

```yaml
provider: cloudflare
cloudflare:
  apiUrl: https://crabbox-cloudflare-container-runner.example.workers.dev
  workdir: /workspace/crabbox
```

Keep `CRABBOX_CLOUDFLARE_RUNNER_TOKEN` in the shell or credential manager.
`CRABBOX_CLOUDFLARE_RUNNER_URL` can supply the runner URL from the environment.
Repo config should select the runner URL and workdir, not hold bearer tokens.
`crabbox config show` reports the runner URL, workdir, and token state as
`cloudflare.auth` without printing the token. `--type` selects one of the
instance types wired into the deployed runner; update
`worker/wrangler.cloudflare.jsonc` and redeploy when changing the available
`instance_type` bindings or `max_instances`.

Use `cloudflare-dynamic-workers` instead when the target is Worker-runtime module
source rather than Linux command execution.

### Semaphore

```yaml
provider: semaphore
semaphore:
  host: example-org.semaphoreci.com
  project: my-app
  machine: f1-standard-2
  osImage: ubuntu2204
  idleTimeout: 30m
```

Keep `CRABBOX_SEMAPHORE_TOKEN` or `SEMAPHORE_API_TOKEN` in the shell or
credential manager. User config may set host/project defaults; repo config
should only pin Semaphore when the repo intentionally depends on that CI
environment.

### Sprites

```yaml
provider: sprites
sprites:
  apiUrl: https://api.sprites.dev
  workRoot: /home/sprite/crabbox
```

Keep `CRABBOX_SPRITES_TOKEN`, `SPRITES_TOKEN`, `SPRITE_TOKEN`, or
`SETUP_SPRITE_TOKEN` in the shell or credential manager. Repo config should
select the work root only when the repo intentionally depends on a Sprites
layout. The authenticated `sprite` CLI must also be on `PATH`.

Other delegated and SSH-lease providers (RunPod, Tensorlake, Upstash, Islo,
W&B, Railway, Parallels) follow the same shape: a top-level section named after
the provider, with credentials sourced from provider-native env vars rather
than YAML. See [Providers](providers.md) for the per-provider reference.

### Sync

```yaml
sync:
  delete: true
  checksum: false
  gitSeed: true
  fingerprint: true
  baseRef: main
  timeout: 15m
  warnFiles: 50000
  warnBytes: 5368709120
  failFiles: 150000
  failBytes: 21474836480
  allowLarge: false
  exclude:
    - node_modules
    - .turbo
    - dist
```

A `.crabboxignore` file at the repo root appends to `sync.exclude`. See
[Sync](sync.md) for the matcher rules.

### Env forwarding

```yaml
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
```

`env.allow` is name-based and supports trailing wildcards. Crabbox forwards
matching local env vars to the remote command. The default allowlist is
`CI` and `NODE_OPTIONS`. Secrets do not belong in `env.allow`; pass them
through provider-side mechanisms. See [Environment forwarding](env-forwarding.md).

### Run preflight

```yaml
run:
  preflightTools:
    - node
    - bun
    - docker
```

`run.preflightTools` configures which built-in probes `crabbox run --preflight`
executes before the remote command. The CLI flag
`--preflight-tools node,bun,docker` overrides this list for one run. Use
`default` to include Crabbox's default built-ins and `none` to print only the
workspace summary. Preflight probes only report availability; they do not
install toolchains or mutate the machine.

### Actions

```yaml
actions:
  workflow: .github/workflows/crabbox.yml
  job: test
  ref: main
  fields:
    - crabbox_docker_cache=true
  runnerLabels:
    - crabbox
  ephemeral: true
  runnerVersion: latest
```

### Cache

```yaml
cache:
  pnpm: true
  npm: true
  docker: true
  git: true
  maxGB: 80
  purgeOnRelease: false
  volumes:
    - name: pnpm-store
      key: my-app-linux-amd64-node24-pnpm10-lockhash
      path: /var/cache/crabbox/pnpm
      sizeGB: 80
      required: false
```

See [Cache volumes](cache-volumes.md) for key design, provider support, and
existing-lease reuse rules.

### Results

```yaml
results:
  auto: false
  junit:
    - junit.xml
    - reports/junit.xml
```

### SSH

```yaml
ssh:
  key: ~/.ssh/id_ed25519
  user: crabbox
  port: "2222"
  fallbackPorts:
    - "22"
```

The default SSH user is `crabbox`, the default port is `2222`, and the default
fallback port is `22`.

### Tailscale

```yaml
tailscale:
  enabled: false
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: ""
  exitNodeAllowLanAccess: false
```

See [Tailscale](tailscale.md) and [Network and reachability](network.md) for
the connectivity contract.

### Mediated egress

Mediated egress is a browser/app QA feature where a lease exits to the internet
through an operator machine over the coordinator mediator. It is opt-in
and profile-based.

```yaml
egress:
  enabled: false
  listen: 127.0.0.1:3128
  browserProxy: true
  profiles:
    discord:
      allow:
        - discord.com
        - "*.discord.com"
    slack:
      allow:
        - slack.com
        - "*.slack.com"
```

See [Mediated egress](egress.md) for the design, security model, and command
surface. The CLI ships built-in `discord` and `slack` profiles; the YAML shape
above is the intended surface for making those profiles user-configurable.

## Machine classes

A machine class is a provider-agnostic name for capacity. Each provider maps
the class to a list of concrete instance/server types and falls back through
the list when the first candidate cannot be provisioned.

| Class | Intent |
|:------|:-------|
| `standard` | typical CI lane |
| `fast` | ~2x more cores than standard for parallel-friendly suites |
| `large` | memory-heavy or many-process workloads |
| `beast` | maximum capacity within the provider's burstable family |

Class-to-type mappings live in [Providers](providers.md). When you set
`serverType:` (or `--type`), that exact provider type wins and the class is
ignored. The `serverType:` and `--type` paths intentionally do not fall back;
they fail loud if the provider rejects the type.

Set `architecture: arm64` (or `--arch arm64`) for Linux ARM capacity on AWS or
Azure. Explicit ARM provider types also select matching ARM Linux images when
no provider-specific image override is set.

## Environment variables

Most YAML keys have a matching `CRABBOX_*` env override that takes precedence
over both config files. The full list is in
[CLI](../cli.md#environment-variables). Common ones:

```text
CRABBOX_CONFIG                  explicit config path (replaces file search)
CRABBOX_COORDINATOR             broker URL
CRABBOX_COORDINATOR_TOKEN       broker user token
CRABBOX_COORDINATOR_TOKEN_COMMAND
                               JSON argv array that prints a fresh broker bearer token
CRABBOX_COORDINATOR_ADMIN_TOKEN broker admin token (also CRABBOX_ADMIN_TOKEN)
CRABBOX_ACCESS_CLIENT_ID        Cloudflare Access service-token id
CRABBOX_ACCESS_CLIENT_SECRET    Cloudflare Access service-token secret
CRABBOX_PROVIDER                default provider
CRABBOX_TARGET                  default target OS
CRABBOX_ARCH                    default architecture: amd64 or arm64
CRABBOX_OS                      default OS image
CRABBOX_PROFILE                 default profile
CRABBOX_DEFAULT_CLASS           default machine class
CRABBOX_SERVER_TYPE             explicit provider type
CRABBOX_IDLE_TIMEOUT            idle timeout
CRABBOX_TTL                     lease TTL
CRABBOX_NETWORK                 network mode
CRABBOX_OWNER                   usage owner override
CRABBOX_ORG                     usage org override
```

Provider credentials live outside the Crabbox env namespace where the provider
SDK or CLI already defines them:

```text
HCLOUD_TOKEN / HETZNER_TOKEN
AWS_PROFILE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET / AZURE_SUBSCRIPTION_ID
GOOGLE_APPLICATION_CREDENTIALS / GOOGLE_CLOUD_PROJECT
CRABBOX_PROXMOX_TOKEN_ID / CRABBOX_PROXMOX_TOKEN_SECRET
CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN
DAYTONA_API_KEY / DAYTONA_JWT_TOKEN / DAYTONA_ORGANIZATION_ID
SEMAPHORE_API_TOKEN / CRABBOX_SEMAPHORE_TOKEN
E2B_API_KEY / CRABBOX_E2B_API_KEY
MODAL_TOKEN_ID / MODAL_TOKEN_SECRET
```

## What belongs where

| Setting | User config | Repo config | Profile | Notes |
|:--------|:------------|:------------|:--------|:------|
| `broker.url` and `broker.token` | yes | no | no | Per-machine identity. |
| `provider`, `class`, `architecture`, `serverType` | optional default | yes | yes | Per-repo defaults; profiles for lanes. |
| `sync.exclude`, `sync.fingerprint`, `sync.baseRef` | no | yes | yes | Lives with the repo. |
| `env.allow` | no | yes | yes | Repo decides what is safe to forward. |
| Per-user SSH key path | yes | no | no | Personal preference. |
| `aws.region`, `aws.ami` | optional | yes | yes | Repos can pin region. |
| Tailscale tags and template | yes | yes | yes | Both layers can set this. |
| Profiles | yes | yes | n/a | Either layer can define profiles. |

Rule of thumb: anything other repos should inherit when they clone goes in repo
config; anything tied to one operator's machine goes in user config.

## Validation

The CLI validates config eagerly while loading it:

- `parseNetworkMode` rejects `--network` values outside `auto|tailscale|public`;
- `validateNetworkConfig` requires well-formed `tailscale.tags` when
  `tailscale.enabled` is true and rejects managed Tailscale provisioning on
  non-Linux targets, on Blacksmith, and on static providers;
- `validateTargetConfig` rejects unsupported `target`/`windows.mode`
  combinations and provider-specific target requirements;
- `validateRequestedCapabilities` rejects `--desktop`, `--browser`, or `--code`
  for providers whose `Spec.Features` does not list the matching feature flag;
- `crabbox doctor` runs a richer set of checks against config, network
  reachability, and SSH keys.

When validation fails, `crabbox` exits with code 2 and a message that names the
offending field.

Related docs:

- [CLI](../cli.md)
- [config command](../commands/config.md)
- [doctor command](../commands/doctor.md)
- [Sync](sync.md)
- [Providers](providers.md)
- [Capacity and fallback](capacity-fallback.md)
- [Network and reachability](network.md)
