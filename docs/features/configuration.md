# Configuration

Read when:

- adding a new config key, env override, or flag;
- debugging "why is Crabbox using value X here?";
- onboarding a repo and choosing what belongs in repo config vs user config;
- reviewing the YAML schema that `crabbox config show` and `crabbox init`
  emit.

Crabbox configuration is layered. The CLI loads values from five sources and
merges them in a deterministic order. Each source is optional - the binary
boots with sane defaults for everything.

## Precedence

```text
flags > env > repo-local crabbox.yaml/.crabbox.yaml > user config > defaults
```

Reading order is the lowest precedence first: defaults are applied, then
overridden by user config, then repo config, then env vars, then flags. Every
override only replaces fields that are explicitly set; unset fields fall
through.

`crabbox config show` prints the merged configuration as the CLI sees it after
all five layers run. `--json` is stable enough to diff in scripts.
`crabbox config path` prints the user config file path so other tools can
edit it without parsing prose.

## File Locations

```text
macOS user:    ~/Library/Application Support/crabbox/config.yaml
Linux user:    ~/.config/crabbox/config.yaml
XDG override:  $XDG_CONFIG_HOME/crabbox/config.yaml
repo:          ./crabbox.yaml or ./.crabbox.yaml at repo root
explicit:      $CRABBOX_CONFIG (any path)
```

If `CRABBOX_CONFIG` is set, it overrides the repo-local search and replaces
the effective repo config. User config is never replaced by the env override.

State that does not belong in either YAML file:

- live lease records (those are coordinator-owned);
- per-lease SSH private keys (those live under the user config dir but not in
  `config.yaml`);
- provider secrets (those live in the broker environment, your shell env, or
  a credential manager).

## YAML Schema

The full schema below merges what `crabbox init` emits and what advanced
operators set in user config. Most repos only need a small subset.

### Top-level

```yaml
broker:
  url: https://crabbox.openclaw.ai
  provider: aws
  token: <signed-github-token-or-shared-token>
  access:
    clientId: <cloudflare-access-service-token-id>
    clientSecret: <cloudflare-access-service-token-secret>

provider: aws            # default provider when --provider is not set
target: linux            # default target OS
windows:
  mode: normal           # normal or wsl2 when target=windows

profile: project-check
class: beast             # standard | fast | large | beast
type: c7a.48xlarge       # explicit provider type, overrides class fallback
network: auto            # auto | tailscale | public

lease:
  idleTimeout: 30m
  ttl: 90m
```

### Jobs

Named jobs live in repo config and describe reusable Crabbox orchestration,
not project logic baked into the binary. Use them for common "warm a box,
hydrate it with GitHub Actions, run the repo command, clean up" flows.
See [Jobs](jobs.md) for lifecycle details and the field contract.

```yaml
jobs:
  openclaw-wsl2:
    provider: aws
    target: windows
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
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
crabbox job run openclaw-wsl2
```

`job run --dry-run <name>` prints the underlying `warmup`, `actions hydrate`,
`run`, and `stop` commands.

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
  largeClasses:
    - large
    - beast
```

### AWS

```yaml
aws:
  region: eu-west-1
  ami: ami-0123456789abcdef0
  securityGroupId: sg-0abcdef0123456789
  subnetId: subnet-0abcdef0123456789
  instanceProfile: crabbox-runner
  rootGB: 400
  sshCidrs:
    - 203.0.113.0/24
  macHostId: h-0123456789abcdef0
```

### Hetzner

Hetzner credentials and image come from broker-side config. Repos do not need
a `hetzner:` block unless they pin a class or location.

### Static SSH

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: steipete
  port: "22"
  workRoot: /Users/steipete/crabbox
```

### Blacksmith Testbox

```yaml
provider: blacksmith-testbox
blacksmith:
  org: openclaw
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
  repository: github.com/openclaw/crabbox
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
  snapshot: openclaw-crabbox
  apiKey: <daytona-api-key>      # prefer DAYTONA_API_KEY env
```

### E2B

```yaml
provider: e2b
e2b:
  template: base
  workdir: crabbox
  apiUrl: https://api.e2b.app
  domain: e2b.app
```

Keep `E2B_API_KEY` or `CRABBOX_E2B_API_KEY` in the shell or credential
manager. Repo config should select templates and workdirs, not hold API keys.

### Semaphore

```yaml
provider: semaphore
semaphore:
  host: myorg.semaphoreci.com
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

### Env Forwarding

```yaml
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
```

`env.allow` is name-based and supports trailing wildcards. Crabbox forwards
matching local env vars to the remote command. Secrets do not belong in
`env.allow`; pass them through provider-side mechanisms.

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
```

### Results

```yaml
results:
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

### Mediated Egress

Mediated egress is a browser/app QA feature where a lease exits to the internet
through an operator machine over the Cloudflare Worker mediator. The first
implementation is opt-in and profile-based.

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
        - discordcdn.com
        - "*.discordcdn.com"
    slack:
      allow:
        - slack.com
        - "*.slack.com"
        - slack-edge.com
        - "*.slack-edge.com"
```

See [Mediated egress](egress.md) for the design, security model, and command
surface. The current CLI ships built-in `discord` and `slack` profiles; the
YAML shape is the intended config surface for making those profiles
user-configurable.

## Profiles

Profiles are named bundles of config that get applied as a layer on top of
user/repo config. They live under a `profiles:` map and are selected by
`--profile` or `profile:` in repo config.

```yaml
profiles:
  project-check:
    class: beast
    sync:
      baseRef: main
    env:
      allow:
        - PROJECT_*
  smoke:
    class: standard
    lease:
      ttl: 30m
```

Use profiles when one repo has multiple test lanes with different machine
classes, sync rules, or env allowlists. A repo without profiles never needs
the block.

## Machine Classes

A machine class is a provider-agnostic name for "standard", "fast", "large",
or "beast" capacity. Each provider maps the class to a list of concrete
instance/server types and falls back through the list when the first
candidate cannot be provisioned.

| Class | Intent |
|:------|:-------|
| `standard` | typical CI lane |
| `fast` | ~2x more cores than standard for parallel-friendly suites |
| `large` | memory-heavy or many-process workloads |
| `beast` | maximum capacity within the provider's burstable family |

Class-to-type mappings live in [Providers](providers.md). When you set
`type:`, that exact provider type wins and the class is ignored. The
`--type` and `type:` paths intentionally do not fall back; they fail loud
if the provider rejects the type.

## Environment Variables

Every YAML key has a `CRABBOX_*` env override. The full list is in
[CLI](../cli.md#environment-variables). Common ones:

```text
CRABBOX_COORDINATOR
CRABBOX_COORDINATOR_TOKEN
CRABBOX_PROVIDER
CRABBOX_TARGET
CRABBOX_PROFILE
CRABBOX_DEFAULT_CLASS
CRABBOX_IDLE_TIMEOUT
CRABBOX_TTL
CRABBOX_NETWORK
CRABBOX_OWNER
CRABBOX_ORG
```

Provider credentials live outside the Crabbox env namespace because they are
provider-native:

```text
HCLOUD_TOKEN / HETZNER_TOKEN
AWS_PROFILE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
DAYTONA_API_KEY / DAYTONA_JWT_TOKEN
BLACKSMITH_*  (read by the Blacksmith CLI)
ISLO_API_KEY  (read by the Islo SDK)
SEMAPHORE_API_TOKEN / CRABBOX_SEMAPHORE_TOKEN
E2B_API_KEY / CRABBOX_E2B_API_KEY
```

## What Belongs Where

| Setting | User config | Repo config | Profile | Notes |
|:--------|:------------|:------------|:--------|:------|
| `broker.url` and `broker.token` | yes | no | no | Per-machine identity. |
| `provider`, `class`, `type` | optional default | yes | yes | Per-repo defaults; profiles for lanes. |
| `sync.exclude`, `sync.fingerprint`, `sync.baseRef` | no | yes | yes | Lives with the repo. |
| `env.allow` | no | yes | yes | Repo decides what is safe to forward. |
| Per-user SSH key path | yes | no | no | Personal preference. |
| `aws.region`, `aws.ami` | optional | yes | yes | Repos can pin region. |
| Tailscale tags and template | yes | yes | yes | Both layers can set this. |
| Profiles | yes | yes | n/a | Either layer can define profiles. |

The rule of thumb: anything other repos should inherit when they clone goes in
repo config; anything tied to one operator's machine goes in user config.

## Validation

The CLI validates config eagerly:

- `parseNetworkMode` rejects `--network` values outside `auto|tailscale|public`;
- `validateNetworkConfig` requires `tailscale.tags` when `tailscale.enabled`
  is true and rejects Tailscale on Blacksmith and static providers;
- `validateRequestedCapabilities` rejects `--desktop`, `--browser`, or
  `--code` for providers whose `Spec.Features` does not list the matching
  feature flag;
- `crabbox doctor` runs a richer set of checks against config, network
  reachability, and SSH keys.

When validation fails, `crabbox` exits with code 2 and a message that names
the offending field.

Related docs:

- [CLI](../cli.md)
- [config command](../commands/config.md)
- [doctor command](../commands/doctor.md)
- [Sync](sync.md)
- [Providers](providers.md)
- [Capacity and fallback](capacity-fallback.md)
- [Network and reachability](network.md)
