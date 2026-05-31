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

- live lease records (those are coordinator-owned);
- per-lease SSH private keys (they live under the user config dir, but not in
  `config.yaml`);
- provider secrets (they live in the broker environment, your shell env, or a
  credential manager).

## YAML schema

The schema below merges what `crabbox init` emits with what advanced operators
set in user config. Most repos only need a small subset.

### Top-level

```yaml
broker:
  url: https://broker.example.com
  provider: aws
  token: <signed-github-token-or-shared-token>
  adminToken: <broker-admin-token>     # operators only
  access:
    clientId: <cloudflare-access-service-token-id>
    clientSecret: <cloudflare-access-service-token-secret>

provider: aws            # default provider when --provider is unset
target: linux            # default target OS: linux | macos | windows
architecture: amd64      # amd64 | arm64; arm64 is Linux-only on AWS/Azure
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

### Hetzner

Hetzner credentials and image come from broker-side config. Repos do not need a
`hetzner:` block unless they pin a location or image:

```yaml
provider: hetzner
hetzner:
  location: fsn1
  image: <hetzner-image>
```

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
so Docker Desktop, OrbStack, Colima, and similar local runtimes work when their
Docker context is active. Set `dockerSocket: true` only when commands inside
the lease must use the host Docker daemon; Crabbox then mounts the active local
Unix Docker socket and rejects remote Docker contexts. With the socket enabled
and no explicit work root, Crabbox chooses a host-visible cache work root so
nested Docker bind mounts can see the synced checkout.

Use `--desktop --browser` to bootstrap Xvfb, XFCE, x11vnc, noVNC/websockify,
desktop input tools, screenshot tools, ffmpeg, and a packaged browser inside
the container.

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
through an operator machine over the Cloudflare Worker mediator. It is opt-in
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
