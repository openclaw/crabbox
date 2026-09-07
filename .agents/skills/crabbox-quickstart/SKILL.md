---
name: crabbox-quickstart
description: "First contact with Crabbox: run your repository's tests inside a disposable Docker or Podman container on your own machine, no account and no cloud spend, then stop the box. Use when someone asks what Crabbox is or how to try it, wants a throwaway sandbox for a repo with no crabbox.yaml yet, or is about to run crabbox init here; hand off to the crabbox skill for config that already exists, leased remote machines, jobs, secrets, or artifacts."
license: MIT
---

# Crabbox Quickstart

Crabbox runs your repository's commands on a disposable box — a container on
your own machine, or a remote machine you lease. It syncs your working tree,
runs one command, streams the output back, and exits with that command's code.

## The loop

```text
lease -> sync -> run -> read output -> stop
```

- **Lease** a box from a provider. `--provider local-container` uses Docker or
  Podman on your own machine: no account, no login, no spend.
- **Sync** your current checkout onto it. Crabbox builds the file list from
  Git, so the directory must be a repository.
- **Run** one command there. Its exit code becomes Crabbox's exit code.
- **Read** the streamed output, and optionally timing and test-result files.
- **Stop** the box. A one-shot `run` does this for you; a warm box does not.

## Install and check

```sh
crabbox --version                  # already installed? skip the install
brew install openclaw/tap/crabbox  # or github.com/openclaw/crabbox/releases
crabbox doctor --provider local-container
```

`doctor` with a provider is the readiness check that matters: it names the
container runtime, the leases you already hold, and the `image=` your commands
will run inside.

## First run, no account needed

With Docker or Podman running, this works in any Git repository with no config
file, no login, and no cloud spend:

```sh
git init                                  # only if not already a repository
crabbox run --provider local-container -- uname -a
```

Crabbox provisioned a container, synced the dirty checkout, ran the command
there, streamed its output, propagated its exit code, and deleted the lease.
Budget 30-45 seconds once the base image is local, nearly all of it container
startup; the very first run adds a one-time image pull.

## The box is bare

Read this before swapping `uname -a` for real work. The default image is plain
Ubuntu with `git`, `curl`, `tar`, `python3`, `rsync`, and passwordless `sudo`.
No node, npm, make, gcc, go, cargo, or java. A command needing a runtime fails
before your code does — usually exit 127 and `make: not found`, or for npm a
preflight that stops the run first. Install what you need once, on a warm box:

```sh
crabbox warmup --provider local-container          # prints the <slug>
crabbox run --provider local-container --id <slug> -- \
  sudo apt-get install -y make
```

Your argv runs through a shell on the box, so `&&`, pipes, and redirects work.
Making the setup permanent is a `.crabbox.yaml` job — that is the `crabbox`
skill, not this page.

## Warm a box and reuse it

One-shot runs pay for container startup every time, and throw away whatever
you installed. Keep one box and send several commands to it instead:

```sh
crabbox warmup --provider local-container
crabbox run --provider local-container --id <slug> -- ./run-tests.sh
crabbox status --provider local-container --id <slug>
```

`warmup` prints both a `cbx_...` lease id and a friendly slug; either works as
`--id`. A cold run measured 33-45 seconds, the same run warm about 4 seconds.
Above and below, `./run-tests.sh` stands for your own test command.

## What actually gets synced

Your dirty working tree filtered by Git, not a committed ref, so
untracked-but-not-ignored files do get uploaded — minus a built-in exclude
list: `node_modules`, `dist`, `target`, `.venv`, `__pycache__`, and other
dependency and build output. That never travels; rebuild it in the box.
`crabbox sync-plan` prints the file count, total bytes, and the largest files
and directories without starting a container. Exclude anything else surprising
in `.crabboxignore`.

## Errors you will meet first

Exit 2 is a missing `--provider`, 6 a directory that is not a Git repository,
7 a runtime Crabbox cannot reach, 4 an `--id` naming no live lease. Your own
command's code passes through verbatim too, so the number alone never says
which failed — read the message, then rerun the `doctor` command above.

When your command itself fails, Crabbox exits with its code, prints a
failure digest with `next:` commands, and drops a bundle in
`.crabbox/captures/`. Sync excludes it; add `.crabbox/` to `.gitignore`.

## Environment and secrets

Nothing from your shell crosses into the box automatically; forwarding is an
allowlist by name, set with `--allow-env` or `env.allow` in the repo config.
Never put a token on a command line; the `crabbox` skill covers the rest.

## Evidence from a run

Evidence flags attach to any `run` and cost no extra time:

```sh
crabbox run --provider local-container --results-auto -- ./run-tests.sh
```

If your command writes a JUnit XML report, `--results-auto` finds it without
being told the path and summarizes it in one line:

```text
test results files=1 tests=3 failures=1 errors=0 skipped=1
```

If nothing writes a JUnit file the flag is a silent no-op.

## Stop what you started

Boxes from `warmup` outlive the command; so do runs given `--keep`. Stop them:

```sh
crabbox stop <slug>
crabbox list --provider local-container
```

An empty `list` means no leases remain on that provider. Local containers kept
by `warmup` or `--keep` require explicit `stop`; they do not expire on their own.

## When you outgrow this page

Stop here and load the full `crabbox` skill as soon as the task involves any of:

- a repository that already has `crabbox.yaml` or `.crabbox.yaml`, including
  one `crabbox init --detect` has just written
- any provider other than `local-container`, or a broker login
- a toolchain the base image lacks that you want present on every run
- named jobs, pools, prewarming, a fresh PR checkout, or Windows targets
- environment or secret forwarding, artifacts, or desktop and UI proof
- a failure that `crabbox doctor` output does not explain
