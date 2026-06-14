# Slurm Academic Sandboxes

Read this when:

- an academic, lab, or research group wants Crabbox on an existing Slurm
  cluster;
- you are deciding whether to use `provider: ssh`, `provider: external`, or a
  future built-in `provider: slurm`;
- you need the product contract before writing a Slurm adapter.

Crabbox does not currently ship a built-in Slurm provider. The first supported
offer should be a site-local external provider adapter that submits a Slurm job,
waits for a reachable SSH endpoint inside the allocation, and then lets Crabbox
use the normal SSH sync/run path. That gives campuses a practical path without
putting cluster-specific scheduler, account, security, or filesystem policy in
Crabbox core.

## Product Offer

Position the feature as "Crabbox for academic clusters":

- researchers and agents run `crabbox warmup` or `crabbox run` against existing
  institutional capacity instead of public cloud;
- the site keeps Slurm accounts, partitions, QOS, GPU policy, fair-share, and
  filesystem layout in its own adapter or config;
- Crabbox keeps its normal value: git-aware sync, command streaming, Actions
  hydration on Linux SSH leases, result collection, logs, artifacts, SSH access,
  and optional registered coordinator visibility;
- no Slurm controller, login node, or `slurmrestd` endpoint has to become
  internet-facing.

This is not a replacement for Open OnDemand, JupyterHub, or batch workflows.
Those tools are still the right interface for interactive notebooks, desktops,
teaching portals, and long scientific jobs. Crabbox is the CLI/agent proof
runner for software tests, repros, smoke runs, and short-lived development
tasks that benefit from the same cluster resources.

## Recommended Path

Use the least-specific integration that satisfies the workflow:

| Need | Crabbox path |
| --- | --- |
| Run on a fixed login, lab, or bastion host | `provider: ssh` |
| Allocate compute through Slurm and then SSH into the allocation | `provider: external` with a Slurm adapter |
| Share inventory, WebVNC bridges, and run history without giving the broker Slurm credentials | external provider plus registered coordinator mode |
| Reusable public Slurm support after multiple sites converge on one contract | future built-in `provider: slurm` |

Avoid running heavy tests directly on a Slurm login node through static SSH.
Login nodes are usually shared control-plane machines. The useful Crabbox shape
is "submit through the login node, run inside a scheduled allocation."

## External Adapter Shape

The first adapter should implement the versioned external provider protocol,
not core provider code. The adapter owns Slurm-specific behavior:

1. `doctor` verifies `sbatch`, `squeue`, `scancel`, cluster auth, the configured
   account/partition/QOS, and the chosen SSH/proxy mode without creating a job.
2. `acquire` submits an `sbatch --parsable` job with a Crabbox-owned job name,
   resource limits, and a site-owned runner script.
3. The batch script starts an SSH-reachable runner inside the allocation. Common
   variants are a per-job `sshd` on an allocated node, an SSH proxy through the
   submit host, or a site-managed gateway command.
4. The adapter polls `squeue` or `sacct` and a private state file until the job
   publishes host, port, user, key/proxy, and readiness metadata.
5. The adapter returns a normal external provider lease with `cloudId` set to
   the Slurm job identity.
6. Crabbox waits for SSH, claims the lease, rsyncs the repo, runs commands, and
   collects results through the ordinary SSH lease path.
7. `release` uses `scancel` and removes adapter state, temporary keys, and
   per-job routing files.

`sbatch` submits work and returns after the controller accepts the script; the
job may still sit pending before resources are allocated. Prefer `warmup` for
interactive or agent workflows so queue time is paid before the first command.

## Configuration Sketch

The concrete adapter name is site-owned. A generic repo or user config should
look like this:

```yaml
provider: external
target: linux

external:
  command: slurm-cbx
  args:
    - --state-dir
    - /home/example-user/.crabbox/slurm
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
  workRoot: /scratch/example-lab/crabbox
```

The adapter should return the resolved endpoint as protocol JSON:

```json
{
  "protocolVersion": 1,
  "lease": {
    "leaseId": "cbx_0123456789ab",
    "slug": "bright-coral",
    "name": "crabbox-bright-coral-012345",
    "cloudId": "slurm/job/123456",
    "serverType": "slurm:batch cpu=16 mem=64G time=02:00:00 gres=gpu:1",
    "ssh": {
      "user": "crabbox",
      "host": "login.cluster.example.edu",
      "port": "22",
      "key": "/home/example-user/.crabbox/slurm/cbx_0123456789ab/id_ed25519",
      "proxyCommand": "ssh -W node123.cluster.example.edu:39022 login.cluster.example.edu",
      "readyCheck": "command -v bash && command -v python3 && command -v git && command -v rsync && command -v tar"
    }
  }
}
```

The external protocol currently does not pass Crabbox's generated public key to
the adapter. A Slurm adapter should therefore either generate a per-job SSH key
itself and return the mode-`0600` private key path, or use a site-managed SSH
proxy/auth contract. Do not print private key contents in protocol JSON.

## Runner Script Contract

Keep the batch script small and site-owned. It should:

- request only the resources configured for the lease profile;
- create a per-job work root on approved storage, usually scratch or project
  storage rather than a shared source checkout;
- start from a site-approved environment, module set, Apptainer/Singularity
  image, Pyxis/Enroot container, or bare host image;
- ensure `bash`, `python3`, `git`, `rsync`, `tar`, and OpenSSH server/client
  pieces needed by the selected SSH mode are available;
- publish one private endpoint state file once SSH is reachable;
- clean temporary keys, sockets, and state on normal exit, `scancel`, and job
  timeout where Slurm epilog/trap policy permits.

Container support should stay site-configured. Slurm itself exposes container
options in `srun`, and many HPC sites use Apptainer/Singularity or Pyxis/Enroot
policies. Crabbox should not choose a default container runtime for all
clusters.

## Security Boundaries

Slurm is usually inside a trusted campus network. Keep it that way:

- do not expose `slurmrestd` directly to the public internet; if a site uses it,
  put it behind site TLS, SSO, monitoring, and network controls;
- keep cluster credentials in SSH agents, Kerberos/OIDC helpers, module
  environments, or the adapter's credential store, not repo YAML;
- keep adapter state and returned key files under a private directory with
  mode `0700` directories and mode `0600` key/routing files;
- prefer a login-node `ProxyCommand`, site gateway, or tailnet path when compute
  nodes cannot accept inbound SSH from developer machines;
- scope cleanup by Crabbox lease ID and Slurm job ID so one user cannot cancel
  unrelated jobs by reusing a slug;
- make time limits and idle timeouts shorter than the Slurm wall clock limit so
  Crabbox has a chance to collect results before the scheduler kills the job.

Registered coordinator mode is optional and provider-neutral. Use it when the
site wants portal inventory, sharing, history, or outbound WebVNC bridges, but
the coordinator still must not receive Slurm credentials.

## Acceptance Contract

Before treating a site adapter as production-ready, prove this sequence from a
normal user account:

```sh
crabbox doctor --provider external
crabbox warmup --provider external --slug slurm-smoke --keep --ttl 2h --idle-timeout 30m
crabbox inspect --provider external --id slurm-smoke --json
crabbox run --provider external --id slurm-smoke --preflight -- hostname
crabbox actions hydrate --provider external --id slurm-smoke
crabbox stop --provider external slurm-smoke
```

Also verify failure cleanup:

- an invalid partition or account fails before creating a job, or cancels the
  failed job during rollback;
- a queued job can be stopped before it starts running;
- a job whose SSH endpoint never appears is cancelled unless `--keep` was
  requested;
- `list` and `status` show the Slurm job ID and scheduler state;
- temporary keys and endpoint files disappear after `stop`;
- registered mode outage does not block `scancel`.

## Future Built-In Provider Criteria

Do not add `provider: slurm` until the external shape has survived real site
use. A built-in adapter is justified when:

- at least two Slurm sites can use the same public configuration model;
- the SSH endpoint pattern is stable enough to document without assuming one
  campus network;
- cleanup and status can be implemented without dangerous scheduler queries;
- the provider can expose honest feature flags, probably `ssh`,
  `crabbox-sync`, and `cleanup` for Linux only;
- tests can cover request rendering, status parsing, release safety, and
  idempotent acquisition without a live cluster.

Potential core follow-up: add an optional external protocol field containing
the Crabbox-generated SSH public key. That would let adapters authorize the
normal Crabbox key instead of creating their own key pair.

## References

- [Slurm `sbatch`](https://slurm.schedmd.com/sbatch.html)
- [Slurm `srun` container options](https://slurm.schedmd.com/srun.html)
- [Slurm REST API security guidance](https://slurm.schedmd.com/rest.html)
- [Slurm REST quick start](https://slurm.schedmd.com/rest_quickstart.html)
- [JupyterHub BatchSpawner](https://github.com/jupyterhub/batchspawner)
- [Open OnDemand example campus documentation](https://www.chpc.utah.edu/documentation/software/ondemand.php)
