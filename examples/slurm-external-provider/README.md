# Slurm External Provider Example

This directory contains a reference `provider: external` adapter for campus
Slurm clusters. It is intentionally site-adaptable example code, not a built-in
Crabbox provider.

The adapter turns a Slurm allocation into a normal Crabbox SSH lease:

1. `slurm-cbx.py` receives the Crabbox external provider JSON request.
2. `acquire` submits a Slurm batch job with `sbatch --parsable`.
3. The batch job runs `runner-unprivileged-sshd.sh`, which starts a per-job SSH
   endpoint and writes `endpoint.json` to a shared state directory.
4. The adapter waits for that endpoint, then returns a normal external-provider
   lease with host, port, user, key, and optional proxy metadata.
5. Crabbox uses its standard SSH path for sync, run, Actions hydration, logs,
   artifacts, and `stop`.
6. `release` cancels the Slurm job with `scancel` and removes local state.

Most production sites should fork this adapter. Common site-specific changes
include replacing the sample unprivileged `sshd` runner with an approved gateway
command, container wrapper, Open OnDemand-style connect script, Apptainer image,
or Pyxis/Enroot launch policy.

## Crabbox Configuration

Use an absolute path to the adapter and runner:

```yaml
provider: external
target: linux

external:
  command: python3
  args:
    - /opt/crabbox-slurm/slurm-cbx.py
    - --state-dir
    - /home/alice/.crabbox/slurm
    - --runner-script
    - /opt/crabbox-slurm/runner-unprivileged-sshd.sh
  capabilities:
    idempotentLeaseId: true
  config:
    account: example-lab
    partition: batch
    qos: normal
    cpus: 16
    mem: 64G
    timeLimit: 02:00:00
    gres: gpu:1
    sshMode: proxy-through-login
    loginHost: login.cluster.example.edu
    acquireTimeoutSeconds: 3600
    runnerWorkRoot: /scratch/example-lab/crabbox
  workRoot: /scratch/example-lab/crabbox
```

Then run:

```sh
crabbox doctor --provider external
crabbox warmup --provider external --slug slurm-smoke --keep --ttl 2h --idle-timeout 30m
crabbox run --provider external --id slurm-smoke --preflight -- hostname
crabbox stop --provider external slurm-smoke
```

Use `warmup` for interactive or agent workflows. Slurm may accept the job
quickly while the allocation remains queued; the adapter waits until the
allocation publishes SSH before returning the lease to Crabbox.

## Adapter Config Keys

All keys below live under `external.config`.

| Key | Meaning |
| --- | --- |
| `account` | Slurm account passed as `--account`. |
| `partition` | Slurm partition passed as `--partition`. |
| `qos` | Slurm QOS passed as `--qos`. |
| `cpus` | CPU count passed as `--cpus-per-task`. |
| `mem` | Memory request passed as `--mem`. |
| `timeLimit` | Wall clock limit passed as `--time`. |
| `gres` | Generic resources, for example `gpu:1`, passed as `--gres`. |
| `nodes` | Node count passed as `--nodes`. |
| `constraint` | Node constraint passed as `--constraint`. |
| `reservation` | Reservation passed as `--reservation`. |
| `extraSbatchArgs` | Additional non-secret Slurm arguments as a string array. |
| `sshMode` | `direct` or `proxy-through-login`. |
| `loginHost` | Login or bastion host used when `sshMode=proxy-through-login`. |
| `loginUser` | Optional login host username for the proxy command. |
| `proxyCommand` | Optional explicit SSH `ProxyCommand` template. |
| `sshUser` | SSH user for the compute endpoint; defaults to the Slurm job user. |
| `sshPrivateKey` | Existing private key path; omit to generate a per-lease key. |
| `readyCheck` | Crabbox SSH readiness check. |
| `acquireTimeoutSeconds` | Seconds to wait for scheduler allocation and endpoint publication. |
| `runnerWorkRoot` | Work root the sample batch runner creates; keep it aligned with `external.workRoot`. |

`extraSbatchArgs` must not contain secrets. Slurm arguments are visible in
process and scheduler metadata on many clusters.

`proxyCommand` supports these template variables:

```text
{host} {port} {loginHost} {loginUser} {leaseId} {slug} {name} {jobId}
```

If `sshMode=proxy-through-login` and no explicit `proxyCommand` is configured,
the adapter returns:

```text
ssh -W %h:%p <loginHost>
```

or `ssh -W %h:%p <loginUser>@<loginHost>` when `loginUser` is set.

## Shared State

`--state-dir` must be visible to both the submit host and the compute job when
using the sample runner, because the job writes `endpoint.json` there. Home,
project, or scratch filesystems usually work. A site-owned gateway can replace
this with a different publication mechanism as long as `slurm-cbx.py` can read
the resulting endpoint.

The adapter creates:

```text
<state-dir>/jobs/<lease-id>/state.json
<state-dir>/jobs/<lease-id>/id_ed25519
<state-dir>/jobs/<lease-id>/id_ed25519.pub
<state-dir>/jobs/<lease-id>/endpoint.json
<state-dir>/jobs/<lease-id>/slurm-<job-id>.out
```

Private directories and key files are created with restrictive modes. The
adapter removes the job directory on successful `release`.

## Endpoint Contract

The runner publishes this JSON:

```json
{
  "host": "node123.cluster.example.edu",
  "port": "39022",
  "user": "alice",
  "readyCheck": "command -v bash && command -v python3 && command -v git && command -v rsync && command -v tar"
}
```

The adapter combines that endpoint with the generated or configured SSH key and
optional proxy settings before returning the Crabbox external-provider lease.

## Security Notes

- Do not expose `slurmrestd` or Slurm controller services to Crabbox clients.
- Keep Slurm credentials in the user's normal cluster login mechanism, Kerberos
  ticket, SSH agent, or site credential helper.
- Keep `--state-dir` private to the user or project service account.
- Use a per-job SSH key when possible and remove it on `release`.
- Validate that `scancel` only targets the persisted Slurm job for the exact
  Crabbox lease ID.
- Replace the sample `runner-unprivileged-sshd.sh` if unprivileged `sshd` is not
  allowed by site policy.

## Local Syntax Checks

```sh
python3 -m py_compile examples/slurm-external-provider/slurm-cbx.py
bash -n examples/slurm-external-provider/runner-unprivileged-sshd.sh
```
