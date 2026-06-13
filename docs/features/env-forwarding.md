# Environment Forwarding

Read this when you are:

- adding a new environment variable that a remote command needs to see;
- debugging "why is `$CI` empty inside `crabbox run`?";
- loading tunable values from a local profile file instead of flags;
- reviewing a change that loosens or tightens the env allowlist.

`crabbox run` does **not** forward your whole local environment to the remote
command. Forwarding is opt-in and name-based: an allowlist of variable names
decides what may cross over, and Crabbox forwards only the allowlisted names
that are actually present locally (or supplied by a profile file).

## Why an allowlist

Agent and CI environments are full of tokens, credentials, terminal paths, and
vendor-specific debug flags. Forwarding everything would leak secrets to remote
runners, introduce drift between local and CI runs, and make it impossible to
reason about what actually affects a remote command. The allowlist makes the
contract explicit: the repo decides what counts as input to a remote command,
and the value is auditable in config.

## The allowlist

The allowlist is a list of variable **names** (not values). It is assembled
from these sources, highest precedence last:

1. Built-in default: `CI` and `NODE_OPTIONS` are always on the list.
2. The repo allowlist in config under `env.allow` (replaces the default).
3. The `CRABBOX_ENV_ALLOW` environment variable (replaces the list).
4. `--allow-env` flags on `crabbox run` (appends to the list).

### Config

```yaml
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
```

Setting `env.allow` replaces the built-in default. Profile-scoped allowlists are
also supported through the profile's own `env.allow`.

### `CRABBOX_ENV_ALLOW`

A comma-separated override that **replaces** the configured list for that run:

```sh
CRABBOX_ENV_ALLOW='CI,NODE_OPTIONS,PROJECT_*' crabbox run -- pnpm test
```

Use it for one-off experiments; persistent allowances belong in `env.allow`.

### `--allow-env`

Repeatable (or comma-separated) flag that **appends** names to the resolved
allowlist for a single run:

```sh
crabbox run --allow-env PYTEST_ADDOPTS --allow-env 'VITEST_*' -- pytest
```

### Matching rules

- Entries are names, not values.
- A trailing `*` is a prefix wildcard (`PROJECT_*` matches `PROJECT_FOO` and
  `PROJECT_BAR`); inline wildcards such as `PROJECT_*_DEBUG` are not supported,
  and a bare `*` is ignored.
- Otherwise the match is exact and case-sensitive.
- Empty entries are ignored.
- Names must be valid shell identifiers (`[A-Za-z_][A-Za-z0-9_]*`); anything
  else is dropped during forwarding.

## What gets forwarded

For each allowlisted name, Crabbox checks whether the variable is set locally.
If it is, the variable is forwarded to the remote command with the same name
and value. If it is not set, nothing is forwarded — Crabbox does not invent
values. Variables that match the allowlist but are unset locally simply do not
appear in the forwarded set.

Inline-allowlisted variables (those resolved from the local environment) are
passed through the provider's remote-command transport. SSH-backed providers
pass them as part of the SSH command itself, with quoting and escaping handled
automatically so values containing shell metacharacters pass through safely.
Delegated-run providers may use a provider-specific transport instead; for
example, Docker Sandbox writes the selected values to a temporary local env file
and passes only that file path to `sbx exec --env-file` so the values do not
appear in local process arguments.

## Profiles: `--env-from-profile`

`--env-from-profile <file>` loads variable **values** from a local `.env`-style
file. Only names that also match the allowlist are kept, so the profile is a
value source, not a second allowlist. The flag is repeatable; when files set the
same key, later files win.

```sh
crabbox run \
  --allow-env 'PROJECT_*' \
  --env-from-profile .env.local \
  -- pnpm test
```

The profile parser is intentionally conservative:

- `KEY=value` lines, with an optional leading `export `;
- `#` comments (at line start or after whitespace) and blank lines are ignored;
- single- and double-quoted values are unquoted (`\"` and `\\` honored inside
  double quotes);
- bare values must be a single token (no unquoted spaces);
- values containing command substitution (`$(...)` or backticks) are skipped;
- entries with invalid variable names are skipped.

Profile values are not inlined into the SSH command. Instead they are written to
a per-run file at `.crabbox/env/<run-or-lease-id>.env` inside the workdir
(`chmod 600`), which the remote command sources before it runs. The file is
removed after the run, and a one-line probe reports which names landed:

```text
env profile remote=.crabbox/env/cbx_ab12cd34ef56.env vars=PROJECT_FOO=set,PROJECT_BAR=set
```

## Reusable helpers: `--env-helper`

`--env-helper <name>` persists the selected profile env as a reusable remote
helper instead of deleting it after the run. It requires profile values
(`--env-from-profile` selected by the allowlist) and cannot be combined with
`--sync-only`. The name must be a simple identifier — no path separators.

It writes two files under `.crabbox/env/` in the workdir: the profile
`<name>.env` (`chmod 600`) and an executable wrapper `<name>` (`chmod 700`) that
sources the profile and `exec`s whatever command you pass it:

```sh
crabbox run --allow-env 'PROJECT_*' --env-from-profile .env.local \
  --env-helper ci -- true
# later, on the box:
./.crabbox/env/ci pnpm test
```

`--env-helper` is not supported on native Windows targets yet.

## Capability-injected env

A small set of variables is injected by Crabbox when the matching
[capability](capabilities.md) is requested. These are not part of the
allowlist — Crabbox owns them — and they **take precedence over** allowlisted
values when names overlap:

```text
CRABBOX_DESKTOP=1         when --desktop
DISPLAY=:99               when --desktop (X11; omitted on Wayland)
CRABBOX_BROWSER=1         when --browser
BROWSER=<path>            when --browser, after probing the box
CHROME_BIN=<path>         when --browser, after probing the box
```

On Wayland/GNOME desktops the probe contributes `WAYLAND_DISPLAY` (and friends)
instead of `DISPLAY`. Because capability env wins on overlap, allowlisting
`BROWSER` will not override a `--browser` probe result for that run; drop the
capability flag if you need to supply your own value.

## Secrets

Do not put secrets in the allowlist even when forwarding seems convenient.
Crabbox forwards whatever it finds locally, so a secret in the allowlist leaks
on every run of every contributor who has it set. Secrets belong in:

- coordinator secret injection for brokered provider credentials;
- the operator's credential store for short-lived tokens;
- a per-runner image bake when the secret should be on every lease;
- post-bootstrap secret injection in repo-owned setup (devcontainer, `bin/setup`).

As a guardrail, Crabbox treats names containing `KEY`, `TOKEN`, `SECRET`,
`PASSWORD`, `PASS`, `CREDENTIAL`, or `AUTH` as secret-shaped: in summaries and
probes their values are replaced with `secret=true` plus a length, never the
value itself. This redacts diagnostics; it does not stop forwarding, so keep
secrets out of the allowlist in the first place.

## Inspecting forwarding

Crabbox prints a one-line forwarding summary whenever you explicitly engage
forwarding — that is, when `CRABBOX_ENV_ALLOW` is set, or `--allow-env` /
`--env-from-profile` is used:

```text
env forwarding provider=hetzner behavior=forwarded vars=CI=set,PROJECT_FOO=set,PROJECT_BAR=set
```

If nothing matched, the line reads `matched=none allow=<list>`. Secret-shaped
names render as `NAME=set len=<n> secret=true`. This line is the source of truth
for "what did the remote command actually see" — variables on the allowlist but
unset locally never appear.

## Examples

A repo allowlist tuning common test knobs:

```yaml
env:
  allow:
    - CI                    # mark a remote command as CI-driven
    - NODE_OPTIONS          # adjust Node memory in test suites
    - PYTEST_ADDOPTS        # tune pytest flags from the local env
    - PROJECT_*             # repo's own debug knobs
    - VITEST_*              # let agents override vitest config
    - DEBUG                 # `debug` package selector
```

Things you usually do **not** allowlist:

```text
HOME, USER, PATH, SHELL    the runner already has its own
SSH_*                       leaks SSH agent state
GITHUB_TOKEN                use Actions hydration or runner setup
AWS_*                       use IAM roles or an instance profile
*_API_KEY, *_TOKEN          use a secret manager
```

## Related docs

- [Sync](sync.md)
- [Configuration](configuration.md)
- [run command](../commands/run.md)
- [Capabilities](capabilities.md)
- [Security](../security.md)
