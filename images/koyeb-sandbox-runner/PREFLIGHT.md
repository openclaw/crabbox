# Local runner preflight

Build and load the candidate `linux/amd64` runner image, then run this gate from
the same Crabbox checkout before publication:

```sh
npm ci --prefix worker --ignore-scripts --no-audit --no-fund
node images/koyeb-sandbox-runner/preflight.mjs --image crabbox-runner:candidate
```

The image argument is required. The gate uses `--pull=never` and resolves the
local image ID before starting it. It needs Docker, Node.js, `ssh`, and
`ssh-keygen`. It uses no cloud credentials and does not call the Koyeb API.

The coordinator's actual Koyeb provisioning code calls the image's real
`/usr/bin/sandbox-executor`. Only the cloud control-plane responses and mapping
of the private hostname to the local container's published port are substituted.
The executor handles authentication, writes the SSH public key, checks the
working directory, runs the actual bootstrap script, and binds its TCP proxy.
The test then connects with a newly generated SSH key through that proxy, uses
the workspace published by the coordinator, writes and reads a file, executes
Python, executes the browser and terminal hooks, requires browser CDP on
loopback port 9222, and inspects the effective SSH configuration. Negative
requests prove that the public `/koyeb-sandbox` prefix,
the obsolete workspace root, and anonymous command execution are rejected by
the real executor. Binding SSH before successful bootstrap also fails the gate.

The container uses a private local bridge without outbound masquerading and
publishes only ephemeral loopback ports. The test removes its exact container,
network, and temporary key directory on success or failure; cleanup failures
fail the gate. Output records the image ID, image and Docker host architectures,
seccomp profile hash, and executor binary hash. SSH failures include bounded
stderr and the browser launch log. The ordinary Worker unit suite skips this
Docker test; this explicit command never
succeeds by silently skipping a missing image.

Release validation requires a native `amd64` runner. A successful ARM Docker run
provides executable image, executor, SSH, and browser/terminal hook coverage
through translation. It does not replace native production-architecture
validation or the live Koyeb route checks described below. Container namespace
policy can prevent Chrome's sandbox from starting; inspect the launch log before
attributing a failure to architecture. Browser failures remain terminal; this
gate does not disable the browser sandbox or retry a failed provisioning contract.

Docker's default namespace restrictions can prevent non-root Chromium from
starting. The test container therefore uses Playwright's documented seccomp
profile, which retains `SCMP_ACT_ERRNO` as its default action and adds an allow
rule for `clone`, `setns`, and `unshare`. Chrome keeps its own sandbox; the test
adds no capabilities or privileged mode. This is a Docker fixture adaptation,
not proof of Koyeb's effective namespace or seccomp policy.

`preflight-seccomp.json` is copied without modification from
[Microsoft Playwright at d78a9e71f2565e474b627baae00b6da71ebc2ea8](https://github.com/microsoft/playwright/blob/d78a9e71f2565e474b627baae00b6da71ebc2ea8/utils/docker/seccomp_profile.json).
The source Git blob is `fddc05fb520affb145404e6f6f647ca96af8087d` and SHA-256 is
`cc3e61cabda6bbc1e53e54d27ba4d55a9d3be829b6dd1a596f4a7b31b1cc7849`.
The upstream Apache 2.0 license and notice are preserved in
`preflight-seccomp.LICENSE` and `preflight-seccomp.NOTICE` (license line endings
normalized to LF). Its documented Docker
baseline is older than current Docker; this is the pinned Playwright policy,
not a claim that it equals the host engine's current default plus three rules.
See the [upstream rationale](https://github.com/microsoft/playwright/blob/d78a9e71f2565e474b627baae00b6da71ebc2ea8/docs/src/docker.md#L62-L94).
These files are test inputs and are not copied into the production image.

For local source diagnosis using an already built image, `--mount-source`
stages the checkout's runner scripts with the Dockerfile's executable modes and
mounts them read-only. The output marks that mode. It does **not** prove image
packaging; publication validation must omit this option.

This test cannot establish live Koyeb private DNS, edge routing, deployment
selection, or cleanup of a billed Sandbox. Release acceptance must still run a
bounded, explicitly authorized lifecycle from the deployed coordinator/Gateway
network and confirm deletion of the exact allocated service.
