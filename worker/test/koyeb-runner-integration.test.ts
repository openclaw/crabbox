/* oxlint-disable eslint/no-await-in-loop -- Provisioning and owned-resource cleanup are sequential. */
import { execFile } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { expect, it } from "vitest";

import { leaseConfig } from "../src/config";
import { KoyebClient, KoyebResumableProvisioning } from "../src/koyeb";
import type { ProvisioningStep } from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type { Env, LeaseRecord } from "../src/types";
import {
  installPoolDiagnostic,
  readPoolDiagnostics,
  unavailablePoolDiagnostic,
} from "./fixtures/koyeb-pool-diagnostic";

const execute = promisify(execFile);
const image = process.env.CRABBOX_TEST_RUNNER_IMAGE;
const imageRoot = fileURLToPath(new URL("../../images/koyeb-sandbox-runner/", import.meta.url));
const dockerEnvironment = Object.fromEntries(
  ["PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "TMPDIR"]
    .filter((name) => process.env[name] !== undefined)
    .map((name) => [name, process.env[name]!]),
);
const docker = async (args: string[], env: Record<string, string> = {}) => {
  const result = await execute("docker", args, {
    env: { ...dockerEnvironment, ...env },
    timeout: 60_000,
    maxBuffer: 1024 * 1024,
  });
  return result.stdout.trim();
};
const removeContainer = async (id: string) => {
  await docker(["rm", "--force", id]);
  if (await docker(["ps", "--all", "--quiet", "--filter", `id=${id}`])) {
    throw new Error("The owned preflight container was not removed");
  }
};
const shellQuote = (value: string) => `'${value.replaceAll("'", "'\"'\"'")}'`;

// This gate uses the real executor supplied by the pinned koyeb/sandbox image.
// Its API is independently defined by sandbox-container v0.0.22:
// https://github.com/koyeb/sandbox-container/blob/v0.0.22/pkg/server/server.go
// https://github.com/koyeb/sandbox-container/blob/v0.0.22/pkg/server/handlers.go
// Only the cloud control plane and private hostname -> loopback port mapping are
// substituted. No executor responses, bootstrap results, SSH, or files are mocked.
it.skipIf(!image)(
  "provisions the runner through the real private executor and uses its published SSH workspace",
  async () => {
    const suffix = randomUUID();
    const containerName = `crabbox-preflight-${suffix}`;
    const networkName = `${containerName}-network`;
    const directory = await mkdtemp(join(tmpdir(), "crabbox-runner-preflight-"));
    let containerID = "";
    let networkID = "";
    let failure: unknown;
    const cleanupErrors: unknown[] = [];
    let poolDiagnosticInstalled = false;
    try {
      const key = join(directory, "id_ed25519");
      await execute("ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-f", key], {
        timeout: 10_000,
      });
      const publicKey = await readFile(`${key}.pub`, "utf8");
      const lease: LeaseRecord = {
        id: "cbx_abcdef123456",
        slug: "runner-preflight",
        provider: "koyeb",
        owner: "alice@example.com",
        providerOwner: "alice@example.com",
        org: "example-org",
        target: "linux",
        windowsMode: "normal",
        architecture: "amd64",
        os: "ubuntu:22.04",
        desktop: true,
        desktopEnv: "xfce",
        browser: true,
        code: true,
        profile: "default",
        class: "tiny",
        serverType: "large",
        requestedServerType: "large",
        cloudID: "",
        serverID: 0,
        host: "",
        sshUser: "crabbox",
        sshPort: "22",
        sshFallbackPorts: [],
        workRoot: "/home/crabbox/crabbox",
        keep: false,
        ttlSeconds: 3_600,
        idleTimeoutSeconds: 600,
        estimatedHourlyUSD: 0,
        maxEstimatedUSD: 0,
        state: "provisioning",
        createdAt: "2026-09-11T00:00:00.000Z",
        updatedAt: "2026-09-11T00:00:00.000Z",
        lastTouchedAt: "2026-09-11T00:00:00.000Z",
        expiresAt: "2026-09-11T01:00:00.000Z",
        createAttemptGeneration: suffix,
      };
      const appName = "preflight";
      const serviceName = leaseProviderName(lease.id, lease.slug!);
      const privateHost = `${serviceName}.${appName}.internal`;
      const serviceID = "11111111-1111-4111-8111-111111111111";
      const deploymentID = "22222222-2222-4222-8222-222222222222";
      const organizationID = "33333333-3333-4333-8333-333333333333";
      const appID = "44444444-4444-4444-8444-444444444444";
      const env = {
        KOYEB_API_TOKEN: "synthetic-control-plane-token",
        CRABBOX_KOYEB_API_URL: "https://koyeb.invalid",
        CRABBOX_KOYEB_ORGANIZATION_ID: organizationID,
        CRABBOX_KOYEB_APP_ID: appID,
        CRABBOX_KOYEB_REGION: "was",
        CRABBOX_KOYEB_INSTANCE_TYPE: "large",
        CRABBOX_KOYEB_IMAGE: `example.invalid/runner@sha256:${"a".repeat(64)}`,
        KOYEB_APP_ID: appID,
        KOYEB_APP_NAME: appName,
        KOYEB_ORGANIZATION_ID: organizationID,
        KOYEB_REGION: "was",
      } as Env;
      let definition: Record<string, unknown> | undefined;
      let lifeCycle: unknown;
      let managementOrigin = "";
      let bootstrapCompleted = false;
      let bootstrapDiagnostic = "";
      const operations: string[] = [];
      const service = () => ({
        id: serviceID,
        name: serviceName,
        type: "SANDBOX",
        organization_id: organizationID,
        app_id: appID,
        status: "HEALTHY",
        active_deployment_id: deploymentID,
        latest_deployment_id: deploymentID,
        life_cycle: lifeCycle,
      });
      const fetcher: typeof fetch = async (input, init) => {
        const request = new Request(input, init);
        const url = new URL(request.url);
        if (url.origin === "https://koyeb.invalid") {
          if (request.headers.get("authorization") !== "Bearer synthetic-control-plane-token") {
            throw new Error("Unexpected control-plane credential");
          }
          if (request.method === "GET" && url.pathname === "/v1/services") {
            return Response.json({ services: definition ? [service()] : [], has_next: false });
          }
          if (request.method === "POST" && url.pathname === "/v1/services") {
            const body = (await request.json()) as Record<string, unknown>;
            definition = body.definition as Record<string, unknown>;
            lifeCycle = body.life_cycle;
            return Response.json({ service: service() });
          }
          if (request.method === "GET" && url.pathname === `/v1/services/${serviceID}`) {
            return Response.json({ service: service() });
          }
          if (request.method === "GET" && url.pathname === `/v1/deployments/${deploymentID}`) {
            return Response.json({
              deployment: {
                id: deploymentID,
                service_id: serviceID,
                organization_id: organizationID,
                app_id: appID,
                status: "HEALTHY",
                definition,
                metadata: { sandbox: {} },
              },
            });
          }
          throw new Error("Unexpected local control-plane operation");
        }
        expect(url.origin).toBe(`http://${privateHost}:3030`);
        expect(request.headers.has("x-routing-key")).toBe(false);
        operations.push(`${request.method} ${url.pathname}`);
        if (url.pathname === "/bind_port" && !bootstrapCompleted) {
          throw new Error("SSH proxy binding preceded a successful runner bootstrap");
        }
        // Rewrite only the authority. Paths, methods, headers and body go to
        // the actual executor; adding the public prefix therefore gets a 404.
        const target = new URL(`${url.pathname}${url.search}`, managementOrigin);
        const response = await fetch(new Request(target, request), {
          signal: AbortSignal.timeout(120_000),
        });
        if (url.pathname === "/run" && response.ok) {
          const result = (await response.clone().json()) as { code: number; stderr: string };
          bootstrapCompleted = result.code === 0;
          bootstrapDiagnostic = result.stderr
            .replaceAll(prepared.material.providerSecret, "[redacted]")
            .slice(0, 2048);
        }
        return response;
      };
      const capability = new KoyebResumableProvisioning(env, fetcher);
      const config = leaseConfig({
        provider: "koyeb",
        sshPublicKey: publicKey,
        desktop: true,
        browser: true,
        code: true,
        tailscale: false,
        ttlSeconds: lease.ttlSeconds,
        idleTimeoutSeconds: lease.idleTimeoutSeconds,
      });
      config.serverType = env.CRABBOX_KOYEB_INSTANCE_TYPE!;
      config.sshPort = "22";
      config.sshFallbackPorts = [];
      config.workRoot = "/workspace/crabbox";
      const prepared = await capability.prepare(config, lease);
      lease.providerScope = prepared.plan.scope;
      const [imageID, imageArchitecture, dockerArchitecture] = await Promise.all([
        docker(["image", "inspect", image!, "--format", "{{.Id}}"]),
        docker(["image", "inspect", image!, "--format", "{{.Architecture}}"]),
        docker(["info", "--format", "{{.Architecture}}"]),
      ]);
      expect(imageID).toMatch(/^sha256:[a-f0-9]{64}$/);
      expect(imageArchitecture).toBe("amd64");
      const seccompProfile = join(imageRoot, "preflight-seccomp.json");
      const seccompProfileSha256 = createHash("sha256")
        .update(await readFile(seccompProfile))
        .digest("hex");
      expect(seccompProfileSha256).toBe(
        "cc3e61cabda6bbc1e53e54d27ba4d55a9d3be829b6dd1a596f4a7b31b1cc7849",
      );
      const runtime = {
        runnerImage: imageID,
        imageArchitecture,
        dockerArchitecture,
        seccompProfileSha256,
      };
      // Internal networks suppress published ports on some Docker engines.
      // Disable outbound masquerading on this private bridge and expose only
      // loopback ports to the host test. No cloud requests are forwarded.
      networkID = await docker([
        "network",
        "create",
        "--opt",
        "com.docker.network.bridge.enable_ip_masquerade=false",
        networkName,
      ]);
      const mounts: string[] = [];
      if (process.env.CRABBOX_TEST_RUNNER_MOUNT_SOURCE === "1") {
        // Match Dockerfile COPY --chmod=0755 without changing the checkout's
        // tracked file modes. This explicit development mode does not validate
        // that the supplied image packages these scripts; CI must omit it.
        const stagedScripts = join(directory, "scripts");
        await mkdir(stagedScripts);
        for (const [name, destination] of [
          ["bootstrap.sh", "/usr/local/libexec/crabbox-koyeb-sandbox/bootstrap.sh"],
          ["desktop-session.sh", "/usr/local/libexec/crabbox-koyeb-sandbox/desktop-session.sh"],
          ["healthcheck.sh", "/usr/local/libexec/crabbox-koyeb-sandbox/healthcheck.sh"],
          ["teardown.sh", "/usr/local/libexec/crabbox-koyeb-sandbox/teardown.sh"],
          ["project-state.py", "/usr/local/libexec/crabbox-koyeb-sandbox/project-state.py"],
          [
            "project_dependencies.py",
            "/usr/local/libexec/crabbox-koyeb-sandbox/project_dependencies.py",
          ],
          ["crabbox-browser", "/usr/local/bin/crabbox-browser"],
          ["crabbox-worker-browser", "/usr/local/bin/crabbox-worker-browser"],
          ["crabbox-worker-terminal", "/usr/local/bin/crabbox-worker-terminal"],
          ["tailscale-client", "/usr/local/bin/tailscale"],
        ]) {
          await writeFile(join(stagedScripts, name), await readFile(join(imageRoot, name)), {
            mode: 0o755,
          });
          // Individual mounts retain the image's packaged libexec binaries.
          mounts.push(
            "--mount",
            `type=bind,src=${join(stagedScripts, name)},dst=${destination},readonly`,
          );
        }
      }
      containerID = await docker(
        [
          "create",
          "--pull=never",
          "--platform",
          "linux/amd64",
          "--name",
          containerName,
          "--network",
          networkID,
          "--network-alias",
          privateHost,
          "--publish",
          "127.0.0.1::3030",
          "--publish",
          "127.0.0.1::3031",
          "--cpus",
          "2",
          "--memory",
          "3g",
          "--shm-size",
          "256m",
          // Playwright's pinned Docker profile permits non-root Chromium's
          // user namespace sandbox. This is a test-host adaptation, not an
          // assertion about Koyeb's effective microVM isolation policy.
          "--security-opt",
          `seccomp=${seccompProfile}`,
          "--env",
          "SANDBOX_SECRET",
          "--env",
          "PORT=3030",
          "--env",
          "PROXY_PORT=3031",
          "--env",
          "LOG_LEVEL=ERROR",
          ...mounts,
          "--entrypoint",
          "/usr/bin/sandbox-executor",
          imageID,
        ],
        { SANDBOX_SECRET: prepared.material.providerSecret },
      );
      await docker(["start", containerID]);
      try {
        await installPoolDiagnostic(docker, containerID);
        poolDiagnosticInstalled = true;
      } catch {
        // Diagnostics are ancillary; preserve every original preflight result.
        process.stdout.write(JSON.stringify(unavailablePoolDiagnostic()) + "\n");
      }
      // Source mounts must preserve the packaged client used by its wrapper.
      expect(await docker(["exec", containerID, "/usr/local/bin/tailscale", "version"])).toMatch(
        /^\d+\.\d+\.\d+/,
      );
      const managementPort = await docker(["port", containerID, "3030/tcp"]);
      const sshPort = await docker(["port", containerID, "3031/tcp"]);
      expect(managementPort).toMatch(/^127\.0\.0\.1:\d+$/);
      expect(sshPort).toMatch(/^127\.0\.0\.1:\d+$/);
      managementOrigin = `http://${managementPort}`;
      const bootDeadline = Date.now() + 60_000;
      let reachable = false;
      while (Date.now() < bootDeadline) {
        try {
          const response = await fetch(`${managementOrigin}/health`, {
            signal: AbortSignal.timeout(1000),
          });
          reachable = response.status === 200;
          await response.body?.cancel();
          if (reachable) break;
        } catch {
          // Only process-start readiness is polled. All contract failures below are terminal.
        }
        await delay(250);
      }
      expect(reachable).toBe(true);
      const request = (path: string, body?: unknown, authenticated = true) =>
        fetch(`${managementOrigin}${path}`, {
          method: body === undefined ? "GET" : "POST",
          headers: {
            "content-type": "application/json",
            ...(authenticated
              ? { authorization: `Bearer ${prepared.material.providerSecret}` }
              : {}),
          },
          ...(body === undefined ? {} : { body: JSON.stringify(body) }),
          signal: AbortSignal.timeout(20_000),
        });
      const prefixed = await request("/koyeb-sandbox/health");
      expect(prefixed.status).toBe(404);
      await prefixed.body?.cancel();
      const anonymous = await request("/run", { cmd: "true" }, false);
      expect(anonymous.status).toBe(401);
      await anonymous.body?.cancel();
      const oldWorkspace = await request("/run", { cmd: "pwd", cwd: "/home/crabbox/crabbox" });
      expect(oldWorkspace.status).toBe(400);
      await oldWorkspace.body?.cancel();

      let step: ProvisioningStep = prepared.step;
      for (let phase = 0; phase < 3; phase += 1) {
        step = await capability.advance({
          ...prepared,
          step,
          lease,
          deadline: Date.now() + 120_000,
          recovering: false,
          canceled: false,
        });
        if (step.phase === "blocked") {
          const diagnostic = await request("/run", {
            cmd:
              "python3 -c " +
              shellQuote(
                [
                  "import importlib.util,json,pathlib,sys,os,stat",
                  "p='/usr/local/libexec/crabbox-koyeb-sandbox'",
                  "sys.path.insert(0,p)",
                  "s=importlib.util.spec_from_file_location('state',p+'/project-state.py'); m=importlib.util.module_from_spec(s); s.loader.exec_module(m)",
                  "try: m.snapshot(pathlib.Path('/home/crabbox'))",
                  "except Exception as e: print(json.dumps({'class':type(e).__name__,'errno':getattr(e,'errno',None),'reason':str(e) if isinstance(e,ValueError) else 'filesystem-operation-failed'}))",
                  "for root,dirs,files in os.walk('/home/crabbox',followlinks=False):",
                  " for name in dirs+files:",
                  "  path=pathlib.Path(root,name); mode=path.lstat().st_mode",
                  "  if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode)): print(json.dumps({'relative':str(path.relative_to('/home/crabbox')),'kind':stat.S_IFMT(mode),'target':os.readlink(path) if stat.S_ISLNK(mode) else None}))",
                ].join("\n"),
              ),
          });
          const diagnosticBody = (await diagnostic.json()) as { stdout?: string };
          throw new Error(
            JSON.stringify({
              phase,
              reason: step.blockedReason,
              state: step.state,
              bootstrapDiagnostic,
              syntheticHomeDiagnostic: diagnosticBody.stdout?.slice(0, 4096),
            }),
          );
        }
      }
      expect(step.phase).toBe("ready-to-publish");
      expect(operations).toEqual([
        "GET /health",
        "POST /write_file",
        "POST /run",
        "POST /bind_port",
      ]);
      expect(definition?.ports).toEqual([
        { port: 3030, protocol: "http" },
        { port: 3031, protocol: "tcp" },
      ]);
      expect(definition?.routes).toEqual([]);
      const access = step.publication!.access;
      expect(access.sshPort).toBe("3031");
      Object.assign(lease, {
        state: "active",
        cloudID: serviceID,
        region: "was",
        image: step.publication!.image,
        host: privateHost,
        sshPort: access.sshPort,
        workRoot: access.workRoot,
      });
      const client = new KoyebClient(env, fetcher);
      await client.prepareReadyPoolLease(lease);
      await client.claimReadyPoolLease(lease, "a".repeat(64));
      await client.claimReadyPoolLease(lease, "a".repeat(64));
      // A consumed runner must return the helper's exact categorical denial,
      // not an unrelated transport/provider failure.
      await expect(client.prepareReadyPoolLease(lease)).rejects.toMatchObject({
        name: "ProjectCheckpointError",
        code: "checkpoint_project_state_failed",
        message: "checkpoint_project_state_failed",
      });
      await expect(client.claimReadyPoolLease(lease, "b".repeat(64))).rejects.toMatchObject({
        name: "ProjectCheckpointError",
        code: "checkpoint_project_state_failed",
        message: "checkpoint_project_state_failed",
      });
      const knownHosts = join(directory, "known_hosts");
      await writeFile(
        knownHosts,
        `[${sshPort.split(":")[0]}]:${sshPort.split(":")[1]} ${access.sshHostKey}\n`,
        { mode: 0o600 },
      );
      const ssh = async (command: string) =>
        execute(
          "ssh",
          [
            "-F",
            "/dev/null",
            "-p",
            sshPort.split(":")[1]!,
            "-i",
            key,
            "-o",
            "BatchMode=yes",
            "-o",
            "IdentitiesOnly=yes",
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            `UserKnownHostsFile=${knownHosts}`,
            "-o",
            "GlobalKnownHostsFile=/dev/null",
            "-o",
            "ConnectTimeout=10",
            `${access.sshUser}@127.0.0.1`,
            command,
          ],
          { timeout: 30_000, env: dockerEnvironment },
        );
      const project = `${access.workRoot}/checkpoint-project`;
      await ssh(
        [
          "set -eu",
          `mkdir -p ${shellQuote(project + "/bin")}`,
          `cd ${shellQuote(project)}`,
          "git init --quiet",
          "printf tracked > app.js",
          "git add app.js",
          "printf 'uncommitted edit' > app.js",
          "printf '.ignored-work\\n' > .gitignore",
          "printf 'untracked ignored work' > .ignored-work",
          "ln -s ../app.js bin/app",
          "printf metadata > .git/replacement-marker",
        ].join("; "),
      );
      const saved = JSON.parse(
        await client.runProjectState(lease, {
          action: "capture",
          root: project,
          allowedRoot: access.workRoot,
        }),
      ) as { content: string; sha256: string };
      await ssh(
        `cd ${shellQuote(project)} && printf changed > app.js && printf extra > discard-on-restore`,
      );
      const restored = JSON.parse(
        await client.runProjectState(lease, {
          action: "restore",
          root: project,
          allowedRoot: access.workRoot,
          ...saved,
        }),
      ) as { recovery: string; sha256: string };
      expect(restored).toMatchObject({ recovery: "filesystem-only", sha256: saved.sha256 });
      const files = await ssh(
        [
          "set -eu",
          `cd ${shellQuote(project)}`,
          'test "$(cat app.js)" = "uncommitted edit"',
          'test "$(cat bin/app)" = "uncommitted edit"',
          'test "$(cat .ignored-work)" = "untracked ignored work"',
          'test "$(cat .git/replacement-marker)" = metadata',
          "test ! -e discard-on-restore",
          "printf checkpoint-restored",
        ].join("; "),
      );
      expect(files.stdout).toBe("checkpoint-restored");
      // Only the test harness is copied. The implementation is loaded from the
      // packaged image, including Linux's nonempty atomic directory exchange.
      await docker([
        "cp",
        join(imageRoot, "project-state.test.py"),
        `${containerID}:/tmp/project-state.test.py`,
      ]);
      const packagedTests = await execute(
        "docker",
        [
          "exec",
          "--env",
          "PYTHONDONTWRITEBYTECODE=1",
          "--env",
          "CRABBOX_PROJECT_STATE_TEST_MODULE_DIR=/usr/local/libexec/crabbox-koyeb-sandbox",
          containerID,
          "python3",
          "/tmp/project-state.test.py",
          "-v",
        ],
        {
          env: dockerEnvironment,
          timeout: 60_000,
          maxBuffer: 1024 * 1024,
        },
      );
      expect(packagedTests.stderr).toMatch(/Ran \d+ tests[\s\S]*\bOK\b/);
      expect(packagedTests.stderr).not.toContain("skipped");
      process.stdout.write(packagedTests.stderr);
      const result = await ssh(
        [
          "set -eu",
          `cd ${shellQuote(access.workRoot)}`,
          "test -w .",
          "printf preflight > .preflight-write",
          'test "$(cat .preflight-write)" = preflight',
          "rm .preflight-write",
          "command -v crabbox-worker-browser >/dev/null",
          "command -v crabbox-worker-terminal >/dev/null",
          "crabbox-worker-browser",
          "crabbox-worker-terminal",
          "curl --fail --silent --max-time 2 http://127.0.0.1:9222/json/version >/dev/null",
          "crabbox-ready",
          "printf 'workspace=%s\\n' \"$(pwd)\"",
          "python3 -c 'print(6 * 7)'",
        ].join("; "),
      ).catch(async (error: unknown) => {
        let browserLaunchLog = "unavailable";
        try {
          const diagnostic = await request("/run", {
            cmd: `tail -c 4096 ${shellQuote(`/home/crabbox/.cache/crabbox/worker-browser/${serviceName}/launch.log`)}`,
          });
          if (diagnostic.ok) {
            const log = (await diagnostic.json()) as { stdout?: string };
            browserLaunchLog = (log.stdout || "unavailable")
              .replaceAll(prepared.material.providerSecret, "[redacted]")
              .slice(-4096);
          } else {
            browserLaunchLog = `Executor diagnostic HTTP ${diagnostic.status}`;
            await diagnostic.body?.cancel();
          }
        } catch {
          // A diagnostic failure must not obscure the original SSH failure.
        }
        const sshFailure = error as { code?: string | number; stderr?: string };
        throw new Error(
          JSON.stringify({
            event: "runner_ssh_preflight_failed",
            ...runtime,
            sourceMounted: mounts.length > 0,
            sshExitCode: sshFailure.code,
            sshStderr: String(sshFailure.stderr || "unavailable")
              .replaceAll(prepared.material.providerSecret, "[redacted]")
              .slice(-1024),
            browserLaunchLog,
          }),
        );
      });
      expect(result.stdout).toContain(`workspace=${access.workRoot}\n42\n`);
      const sshConfiguration = await request("/run", {
        cmd: "/usr/sbin/sshd -T -f /var/lib/crabbox-koyeb/sshd/sshd_config",
      });
      expect(sshConfiguration.status).toBe(200);
      const effective = (await sshConfiguration.json()) as { code: number; stdout: string };
      expect(effective.code).toBe(0);
      expect(effective.stdout).toContain("listenaddress 127.0.0.1:22");
      expect(effective.stdout).toContain("passwordauthentication no");
      expect(effective.stdout).toContain("permitrootlogin no");
      const executorDigest = await docker([
        "exec",
        containerID,
        "sha256sum",
        "/usr/bin/sandbox-executor",
      ]);
      process.stdout.write(
        JSON.stringify({
          event: "runner_preflight_boundaries_passed",
          ...runtime,
          executorSha256: executorDigest.split(" ")[0],
          sourceMounted: mounts.length > 0,
          executorPrefixRejected: true,
          anonymousRunRejected: true,
          bootstrapCompletedBeforeBind: true,
          browserCDPReady: true,
          terminalHookExecuted: true,
          cleanClaimSingleUse: true,
          checkpointThroughPrivateExecutor: true,
          uncommittedIgnoredFilesRecovered: true,
          linuxAtomicReplacement: true,
          packagedFilesystemDependencyTests: true,
          sshWorkspace: access.workRoot,
        }) + "\n",
      );
    } catch (error) {
      failure = error;
    } finally {
      if (containerID) {
        if (poolDiagnosticInstalled) {
          try {
            // CI tees stdout. Retain categories before the existing cleanup,
            // without another private-executor request or pool check/claim.
            for (const record of await readPoolDiagnostics(docker, containerID)) {
              process.stdout.write(JSON.stringify(record) + "\n");
            }
          } catch {
            process.stdout.write(JSON.stringify(unavailablePoolDiagnostic()) + "\n");
          }
        }
        try {
          await removeContainer(containerID);
        } catch (error) {
          cleanupErrors.push(error);
        }
      }
      if (networkID) {
        try {
          await docker(["network", "rm", networkID]);
        } catch (error) {
          cleanupErrors.push(error);
        }
      }
      try {
        await rm(directory, { recursive: true, force: true });
      } catch (error) {
        cleanupErrors.push(error);
      }
    }
    if (cleanupErrors.length)
      throw new AggregateError(
        [failure, ...cleanupErrors].filter(Boolean),
        failure
          ? "Runner preflight failed and cleanup was incomplete"
          : "Runner preflight cleanup failed",
      );
    if (failure) throw failure;
  },
  240_000,
);
