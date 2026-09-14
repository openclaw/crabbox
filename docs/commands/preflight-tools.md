# preflight-tools

List the preflight names accepted by this installed Crabbox binary, their
default membership, and supported targets. Discovery is offline: it does not
load configuration, select a provider, run a probe, or acquire a lease.

```sh
crabbox preflight-tools
crabbox preflight-tools --json
crabbox preflight-tools --help
crabbox help preflight-tools
```

## Output

Text output lists registered names alphabetically, with default membership and
target labels. It then prints the ordered default expansion and special
selectors. Target labels distinguish `linux`, `macos`, `windows/normal` and
`windows/wsl2`; WSL2 uses the Linux/POSIX probe contract, not native Windows.
This describes compiled support, not which tools are installed or ready on a
particular machine. Both registered spellings, such as `bubblewrap` and `bwrap`,
remain visible.

`--json` returns an object with these fields:

| Field | Meaning |
|---|---|
| `tools` | Alphabetically sorted records with `name`, boolean `default`, and a `targets` array. |
| `defaultTools` | The ordered expansion of the default selector, before target filtering. |
| `selectors` | Special selectors with `name`, an `aliases` array, and `description`. |

Use JSON for scripts. For example:

```sh
crabbox preflight-tools --json | jq -r '.tools[].name'
crabbox preflight-tools --json | jq -r '.tools[] | select(.default) | .name'
crabbox preflight-tools --json | jq -r '.tools[] | select(.targets | index("windows/wsl2")) | .name'
```

## Selection semantics

The same names work in `--preflight-tools` and `run.preflightTools`. Names are
trimmed and case-insensitive. `default` (alias `defaults`) expands the existing
ordered default list. `none` alone disables tool probes, retaining the workspace
summary; mixed `none,git` still selects `git`. Unsupported target-specific probes
are skipped during preflight.

Omitted configuration inherits the lower layer, which selects built-in defaults
when nothing overrides it. An explicit YAML `preflightTools: []` clears the list.
A raw empty-string CLI flag (`--preflight-tools ''`) preserves the resolved
configuration. Nonempty whitespace-only or comma-only CLI input selects defaults,
rather than acting like an explicit empty YAML list. Discovery does not change
any of these rules.

Unknown names still fail with exit 2 before lease acquisition. Their diagnostic
points to `crabbox preflight-tools`, and `crabbox run --help` includes the same
discovery hint. Arbitrary executable names are not accepted.

## Functional Python environment probe

`python3-venv` is opt-in for `linux`, `macos` and `windows/wsl2`, not native
Windows. It leaves the default list and literal `python`/`python3` version probes
unchanged. `default,python3-venv,python3-venv` expands the ordered defaults followed
by one functional probe. Listing it here does not execute it or establish readiness.

The probe creates a disposable pip-enabled environment and invokes its own Python
and pip. `ready` also requires confirmed worker quiescence, scratch cleanup and
exact transport-stage retirement. It does not install host tools, activate or
reuse project environments, or install project packages. Missing or broken
capability, worker failure and probe timeout are diagnostic only when cleanup is
confirmed. Unconfirmed cleanup is an operational failure that blocks the next
workload; confirmed cleanup does not clear an operational transport, setup or
envelope error (`unavailable cleanup=confirmed`). Caller cancellation is also
propagated before the workload. See
[run preflight](run.md#preflight) for every state, cleanup indication and timeout
budget. This functional probe is not supported by profile-doctor version-only
requirements.

## Command contract

`--json` is the only data-output flag; `-h`/`--help` shows help, including when
other arguments are invalid. There are no positional arguments, prompts,
configuration inputs, or side effects. Data goes to stdout and usage diagnostics
to stderr. Successful inspection/help exits 0; unknown options and unexpected
arguments exit 2. Output-write failures remain ordinary command errors.

See [run](run.md#preflight), [observability](../observability.md), and
[run configuration](../features/configuration.md#run-preflight) for execution
and individual probe semantics.
