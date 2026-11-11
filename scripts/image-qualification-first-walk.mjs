// Protected, credentialless executor for the existing qualification lifecycle.
// A synthetic source is captured inside this run; campaign captures are not inputs.
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { readFirstWalkRecipe } from "./image-qualification-control.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const required = (name) => {
  if (!process.env[name]) throw new Error(`missing ${name}`);
  return process.env[name];
};
for (const name of [
  "AWS_ACCESS_KEY_ID",
  "AWS_SECRET_ACCESS_KEY",
  "AWS_SESSION_TOKEN",
  "CLOUDFLARE_API_TOKEN",
  "QUALIFICATION_CONTROLLER_TOKEN",
  "QUALIFICATION_ADMIN_TOKEN",
  "QUALIFICATION_SHARED_TOKEN",
])
  if (process.env[name] !== undefined) throw new Error(`${name} must be absent`);
const relay = required("QUALIFICATION_RELAY_URL");
const token = required("QUALIFICATION_EXECUTOR_TOKEN");
const proof = required("QUALIFICATION_PROOF_DIR");
const artifact = required("QUALIFICATION_ARTIFACT_DIR");
// Reject missing or substituted recipe data before any guest or relay activity.
const recipe = readFirstWalkRecipe(path.join(artifact, "candidate"));
const runner = fs.readFileSync(path.join(root, "scripts/qualification-first-walk-guest.py"));
const region = required("QUALIFICATION_AWS_REGION");
const workCutoff = Date.parse(required("QUALIFICATION_EXPIRES_AT")) - 20 * 60_000;
if (!Number.isFinite(workCutoff) || Date.now() >= workCutoff)
  throw new Error("first-walk work window expired");
fs.mkdirSync(proof, { recursive: true, mode: 0o700 });
const raw = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-executor-"));
const binary = path.join(artifact, "bin/crabbox");
// upload/download-artifact preserves bytes, but not executable permissions.
fs.chmodSync(binary, 0o700);
const env = {
  ...process.env,
  CRABBOX_COORDINATOR: relay,
  CRABBOX_COORDINATOR_TOKEN: token,
  CRABBOX_COORDINATOR_ADMIN_TOKEN: token,
  CRABBOX_ENV_ALLOW: "CI",
};
const scope = ["--provider", "aws", "--target", "linux", "--region", region];
const sha = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
function remaining() {
  const n = workCutoff - Date.now();
  if (n <= 0) throw new Error("first-walk cutoff");
  return n;
}
async function api(method, endpoint, body) {
  remaining();
  const response = await fetch(`${relay}${endpoint}`, {
    method,
    headers: {
      authorization: `Bearer ${token}`,
      ...(body ? { "content-type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(Math.min(30_000, remaining())),
  });
  const data = await response.json();
  if (!response.ok) throw new Error(`candidate ${endpoint}: http ${response.status}`);
  return data;
}
async function cli(name, args, input) {
  const output = path.join(raw, name + ".log");
  const fd = fs.openSync(output, "wx", 0o600);
  try {
    await new Promise((resolve, reject) => {
      const child = spawn(binary, args, {
        env,
        stdio: [input ? "pipe" : "ignore", fd, fd],
        timeout: remaining(),
        killSignal: "SIGTERM",
      });
      child.on("error", reject);
      child.on("exit", (code, signal) =>
        code === 0 ? resolve() : reject(new Error(`${name} exit=${code} signal=${signal}`)),
      );
      if (input) child.stdin.end(input);
    });
  } finally {
    fs.closeSync(fd);
  }
  return fs.readFileSync(output, "utf8");
}
async function create(id, name) {
  const output = await cli(name, [
    "warmup",
    ...scope,
    "--type",
    "t3.small",
    "--ttl",
    "2h",
    "--idle-timeout",
    "100m",
    "--class",
    "standard",
    "--market",
    "on-demand",
    "--desktop=false",
    "--browser=false",
    "--timing-json",
    "--lease-id",
    id,
  ]);
  // Warmup returns only after stock readiness. Retain its exact receipt digest.
  return { lease: (await api("GET", `/v1/leases/${id}`)).lease, readiness: sha(output) };
}
async function run(name, id, script) {
  return await cli(name, ["run", ...scope, "--id", id, "--no-sync", "--script-stdin"], script);
}
let successful = false;
try {
  const suffix = sha(`${process.env.GITHUB_RUN_ID}:${process.env.GITHUB_RUN_ATTEMPT}`).slice(0, 10);
  const sourceID = `cbx_${suffix}01`,
    targetID = `cbx_${suffix}02`;
  const { lease: source } = await create(sourceID, "source-stock-ready");
  const work = source.workRoot;
  if (typeof work !== "string" || !/^\/[a-zA-Z0-9/_-]+$/.test(work))
    throw new Error("saved work root not canonical");
  await run(
    "populate-source",
    sourceID,
    `set -eu\nsudo python3 - <<'PY'\nimport os,pathlib,pwd,grp,subprocess\nuid=pwd.getpwnam('crabbox').pw_uid; gid=grp.getgrnam('crabbox').gr_gid\nroots=[pathlib.Path(${JSON.stringify(work)}),pathlib.Path('/var/cache/crabbox')]\nif len({subprocess.check_output(['findmnt','-n','-o','MAJ:MIN','-T',str(p)],text=True).strip() for p in roots})!=1: raise RuntimeError('capture requires one filesystem')\nfor root in roots:\n p=root/'first-walk-synthetic'; p.mkdir(exist_ok=False)\n for i in range(1000):\n  q=p/str(i); q.write_text('fixture\\n')\n  if i%2: os.chown(q,uid,gid)\n (p/'dangling').symlink_to('missing')\nos.sync()\nPY\n`,
  );
  const checkpointID = `chk_first_walk_${suffix}`;
  await api("POST", "/v1/checkpoints", {
    id: checkpointID,
    leaseID: sourceID,
    name: "first-walk-synthetic",
    strategy: "image",
    retention: { mode: "manual" },
    workdir: work,
  });
  // Existing coordinator checkpoint creation owns recovery, snapshot tagging and
  // completion. A pending/error result is not a reason to create another capture.
  let capture;
  for (let i = 0; i < 600; i++) {
    capture = (await api("GET", `/v1/checkpoints/${checkpointID}`)).checkpoint;
    if (capture?.state === "ready") break;
    if (capture?.state === "failed") throw new Error("synthetic capture failed");
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  if (capture?.state !== "ready" || capture.image?.snapshotIDs?.length !== 1)
    throw new Error("single synthetic capture not ready");
  await cli("source-release", ["stop", ...scope, sourceID]);
  const { readiness } = await create(targetID, "target-stock-ready");
  const use = await api("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" });
  const intent = {
    action: "prepare",
    claim: use.claim,
    leaseID: targetID,
    readinessReceiptSha256: readiness,
  };
  let attachment;
  for (let i = 0; i < 120; i++) {
    try {
      attachment = await api("POST", `/v1/checkpoints/${checkpointID}/first-walk`, intent);
      break;
    } catch (error) {
      if (i === 119) throw error;
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
  }
  if (attachment?.phase !== "attached" || !/^vol-[a-f0-9]+$/.test(attachment.volumeID ?? ""))
    throw new Error("copy attachment not proven");
  const script = `set -eu\nbase=$(mktemp -d /tmp/first-walk-input.XXXXXXXX)\ntrap 'sudo rm -rf -- "$base"' EXIT\nprintf '%s' '${recipe.toString("base64")}' | base64 -d >"$base/recipe"\nprintf '%s' '${runner.toString("base64")}' | base64 -d >"$base/runner.py"\nsudo systemd-run --quiet --wait --pipe --unit=crabbox-first-walk-${suffix} --property=MemoryMax=2G --property=TasksMax=32 --property=LimitNOFILE=65536 --property=RuntimeMaxSec=3600 --property=KillMode=control-group unshare --mount --propagation private -- python3 "$base/runner.py" --volume '${attachment.volumeID}' --saved-work-root '${work}' --recipe "$base/recipe" --recipe-sha256 '${sha(recipe)}' --output "$base/evidence"\n`;
  const output = await run("first-walk-once", targetID, script);
  const line = output.split("\n").find((line) => line.startsWith("CRABBOX_FIRST_WALK_RESULT="));
  if (!line) throw new Error("missing inner first-walk result");
  const result = JSON.parse(line.slice("CRABBOX_FIRST_WALK_RESULT=".length));
  if (
    !result.completed ||
    !result.unmounted ||
    result.volumeID !== attachment.volumeID ||
    result.recipeSha256 !== sha(recipe) ||
    result.ownership?.exit !== 0 ||
    result.verification?.mismatched !== 0 ||
    !result.ownership.summary?.includes("state=complete")
  )
    throw new Error("first walk did not qualify");
  // Synthetic identities stay private. The protected artifact contains no tokens;
  // it binds the complete result and retains counters/CPU/I/O/mount observations.
  fs.writeFileSync(path.join(proof, "first-walk-result.json"), JSON.stringify(result, null, 2), {
    mode: 0o600,
  });
  fs.writeFileSync(
    path.join(proof, "first-walk.json"),
    JSON.stringify(
      {
        firstWalk: {
          completed: true,
          launchCount: 2,
          exit: 0,
          recipeSha256: sha(recipe),
          runnerSha256: sha(runner),
          resultSha256: sha(JSON.stringify(result)),
          sourceCaptureDigest: sha(JSON.stringify(capture)),
          readinessReceiptSha256: readiness,
        },
      },
      null,
      2,
    ),
    { mode: 0o600 },
  );
  successful = true;
} finally {
  // No raw cloud mutation or independent delete. On any failure/cancellation the
  // existing controller fence/reaper settles the exact run, including source and
  // partial volumes. Preserve failed raw output in the protected artifact.
  for (const name of fs.readdirSync(raw)) {
    const text = fs
      .readFileSync(path.join(raw, name), "utf8")
      .replaceAll(token, "[executor-token]")
      .replace(/https?:\/\/[^\s"<>]+/g, "[url]")
      .replace(/\b(?:ami|i|vol|snap|key)-[0-9a-f]{8,}\b/g, "[aws-resource]")
      .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[ip]");
    fs.writeFileSync(path.join(proof, name), text, { mode: 0o600 });
  }
  fs.rmSync(raw, { recursive: true, force: true });
  const checksums = fs
    .readdirSync(proof)
    .filter((name) => name !== "checksums.sha256")
    .sort()
    .map((name) => `${sha(fs.readFileSync(path.join(proof, name)))}  ./${name}`)
    .join("\n");
  fs.writeFileSync(path.join(proof, "checksums.sha256"), checksums + "\n", { mode: 0o600 });
}
if (!successful) throw new Error("first-walk qualification incomplete");
// Exercise the same independent protected finalizer after executor death as the
// image workflow. It fences the candidate before terminating and inventorying.
process.kill(process.pid, "SIGKILL");
