#!/usr/bin/env node
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import fs from "node:fs/promises";
import net from "node:net";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { chromium } from "playwright";

// This intentionally installs services. Never run on a developer or shared host.
assert.equal(process.env.GITHUB_ACTIONS, "true");
assert.equal(process.env.RUNNER_ENVIRONMENT, "github-hosted");
assert.equal(process.env.RUNNER_OS, "Linux");
assert.match(process.env.GITHUB_SHA || "", /^[a-f0-9]{40}$/);
process.umask(0o077);
const root = process.cwd();
const output = path.join(root, "dist/desktop-resize-proof");
const work = await fs.mkdtemp(path.join(process.env.RUNNER_TEMP, "desktop-resize-"));
const binary = path.join(root, "bin/crabbox");
const lease = `cbx_${randomBytes(6).toString("hex")}`;
const slug = `desktop-proof-${randomBytes(6).toString("hex")}`;
const env = { PATH: process.env.PATH, HOME: work, XDG_CONFIG_HOME: work, XDG_CACHE_HOME: work,
  XDG_STATE_HOME: path.join(work, "state"),
  USER: process.env.USER, LANG: "C.UTF-8", LC_ALL: "C.UTF-8", CRABBOX_CONFIG: path.join(work, "config.yaml") };
await fs.writeFile(env.CRABBOX_CONFIG, "{}\n");
await fs.mkdir(output, { recursive: true });
const children = new Set();
const proof = { source: process.env.GITHUB_SHA, run: process.env.GITHUB_RUN_ID, cases: [], screenshots: [], cleanup: {},
  coverage: { localContainer: "candidate_cli_ssh_tunnel_and_guest_novnc",
    installer: "released_xvfb_to_candidate_tigervnc_upgrade_reset_then_depth8",
    legacyFit: "actual_legacy_novnc_with_captured_cli_url_settings",
    legacySSHStartup: false } };
const releasedInstaller = {
  tag: "v0.57.0",
  tagObject: "a57d956766076368baa0e67f1f21f1de4f0517b0",
  commit: "baa6c9a783f55f7af65d7f5d1868ecaf9260f371",
  blob: "5058f4952e9763f2913c60299d9d005c30969ed5",
  sha256: "e8125c3c44a355f1b00a83e36aad9cfc534f4b3053c5bf6e8c51af527dc323bd",
  bytes: 6668,
};
let phase = "preflight";
let container = "";
let releaseClaim;
let acquisitionStarted = false;
let installerStarted = false;
let browser;
let failure;
let cleaning = false;
let interrupted;
let cleanupDeadline;
const jobStarted = Number(process.env.DESKTOP_PROOF_JOB_STARTED_AT) * 1000;
assert.ok(Number.isSafeInteger(jobStarted) && jobStarted > 0 && jobStarted <= Date.now());
const deadline = Math.min(Date.now() + 30 * 60_000, jobStarted + 37 * 60_000);

class ProofFailure extends Error {
  constructor(code, details) { super(code); this.code = code; this.details = details; }
}

function check(value, name, details) {
  if (!value) {
    const error = new ProofFailure(name, details);
    if (!cleaning) failure ??= error;
    throw error;
  }
}

function project(error) {
  if (error instanceof ProofFailure) return { code: error.code, details: error.details };
  const nativeCode = ["EACCES", "EPERM", "ENOENT", "ENOSPC", "EMFILE", "EIO"].includes(error?.code) ? error.code : undefined;
  return { code: "operation_failed", nativeCode };
}

function shutdown(code) {
  if (cleaning || interrupted) return;
  interrupted = new ProofFailure(code);
  for (const item of children) terminate(item, "SIGTERM");
  if (browser) void browser.close().catch(() => {});
}
const deadlineTimer = setTimeout(() => shutdown("proof_deadline"), Math.max(1, deadline - Date.now()));
process.on("SIGINT", () => shutdown("signal_interrupt"));
process.on("SIGTERM", () => shutdown("signal_terminate"));

// Native commands may print credentials. Keep both streams private and bounded;
// errors expose only our phase and exit class, never command output or arguments.
function start(label, file, args, timeout = 60_000) {
  if (!cleaning && interrupted) throw interrupted;
  timeout = Math.min(timeout, cleaning ? Math.min(15_000, cleanupDeadline - Date.now()) : deadline - Date.now());
  check(timeout > 0, cleaning ? "cleanup_deadline" : "proof_deadline");
  const child = spawn(file, args, { cwd: root, env, detached: true, stdio: ["ignore", "pipe", "pipe"] });
  const item = { child, label, stdout: "", stderr: "", bytes: 0, gone: false, timedOut: false, overflow: false };
  children.add(item);
  item.termination = new Promise((resolve) => { item.finishTermination = resolve; });
  item.closed = new Promise((resolve) => {
    child.once("error", () => resolve(-1));
    child.once("close", (code) => resolve(code ?? -1));
  });
  for (const stream of ["stdout", "stderr"]) child[stream].on("data", (data) => {
    item.bytes += data.length;
    if (item.bytes > 8 * 1024 * 1024) {
      item.overflow = true;
      terminate(item, "SIGTERM");
    } else item[stream] += data.toString();
  });
  item.timer = setTimeout(() => {
    item.timedOut = true;
    terminate(item, "SIGTERM");
  }, timeout);
  return item;
}

function exists(item) {
  if (item.gone || !item.child.pid) return false;
  try { process.kill(-item.child.pid, 0); return true; }
  catch (error) {
    if (error.code !== "ESRCH") { item.signalError = true; return true; }
    item.gone = true; return false;
  }
}

function signal(item, name) {
  if (!exists(item)) return;
  try { process.kill(-item.child.pid, name); }
  catch (error) { if (error.code === "ESRCH") item.gone = true; else item.signalError = true; }
}

function terminate(item, name) {
  if (item.killTimer || item.gone) return;
  signal(item, name);
  item.killTimer = setTimeout(() => signal(item, "SIGKILL"), 3000);
  item.pipeTimer = setTimeout(() => item.finishTermination(-2), 6000);
}

async function stop(item) {
  if (item.stopping) return item.stopping;
  item.stopping = stopGroup(item);
  return item.stopping;
}

async function stopGroup(item) {
  clearTimeout(item.timer);
  clearTimeout(item.killTimer);
  clearTimeout(item.pipeTimer);
  if (exists(item)) signal(item, "SIGINT");
  for (let i = 0; i < 30 && exists(item); i++) await delay(100);
  if (exists(item)) signal(item, "SIGKILL");
  for (let i = 0; i < 30 && exists(item); i++) await delay(100);
  check(!exists(item), "owned_process_group_remains");
  try {
    await Promise.race([item.closed, delay(1000).then(() => { throw new ProofFailure("owned_pipe_remains"); })]);
  } catch (error) {
    // The owned group is gone, but an inherited writer may still hold our pipes.
    // Close only our readers and retain the failure; never signal a replacement group.
    item.child.stdout.destroy();
    item.child.stderr.destroy();
    throw error;
  }
  children.delete(item);
}

async function run(label, file, args, timeout) {
  const item = start(label, file, args, timeout);
  const code = await Promise.race([item.closed, item.termination]);
  if (code !== 0 || item.timedOut || item.overflow) {
    const error = new ProofFailure(code === -2 ? "command_pipe_deadline" : "command_failed",
      { label, code, timedOut: item.timedOut, overflow: item.overflow });
    if (!cleaning) failure ??= error;
    // Preserve the command failure; the independent final pass still checks its group.
    try { await stop(item); } catch {}
    throw error;
  }
  await stop(item);
  return item.stdout;
}

const cb = (label, ...args) => run(label, binary, args, label === "warmup" ? 22 * 60_000 : 90_000);
const docker = (label, ...args) => run(label, "docker", args, 90_000);
const guest = (label, ...args) => docker(label, "exec", "--user", "crabbox", "--env", "DISPLAY=:99", container, ...args);
const host = (label, ...args) => run(label, "sudo", ["-n", "-u", "crabbox", "env", "DISPLAY=:99", ...args]);
const containerIDs = () => docker("container_identity", "ps", "-aq", "--no-trunc", "--filter", `label=lease=${lease}`);
const claimPath = path.join(env.XDG_STATE_HOME, "crabbox", "claims", `${lease}.json`);
const keyDirectory = path.join(env.XDG_STATE_HOME, "crabbox", "testboxes", lease);

async function absent(file) {
  try { await fs.lstat(file); return false; }
  catch (error) { if (error.code === "ENOENT") return true; throw error; }
}

async function readClaim() {
  if (await absent(claimPath)) return null;
  const stat = await fs.lstat(claimPath);
  check(stat.isFile() && stat.size < 1024 * 1024, "lease_claim_shape");
  const claim = JSON.parse(await fs.readFile(claimPath, "utf8"));
  check(claim.leaseID === lease && claim.slug === slug && claim.provider === "local-container-fixed-v1" &&
    claim.fixedCreateIntent?.version === 1 && claim.fixedCreateIntent.slug === slug &&
    /^[a-f0-9]{64}$/.test(claim.fixedCreateIntent.fingerprint) &&
    claim.providerScope && claim.providerScope === claim.fixedCreateIntent.providerScope, "lease_claim_identity");
  return claim;
}

async function ownedContainer() {
  const id = (await containerIDs()).trim();
  if (!id) return "";
  check(/^[a-f0-9]{64}$/.test(id), "ambiguous_container");
  const labels = JSON.parse(await docker("container_labels", "inspect", "--format", "{{json .Config.Labels}}", id));
  check(labels.crabbox === "true" && labels.provider === "local-container" && labels.lease === lease && labels.slug === slug,
    "container_labels_drifted");
  check(labels.ssh_key_owned === "true" && labels.bootstrap_owned === "true", "container_ownership_missing");
  return id;
}

async function port() {
  const server = net.createServer();
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  const value = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return value;
}

async function viewer() {
  // Explicitly reveal this fresh fixture's URL only into our private buffers.
  // The product default intentionally omits the whole credential-bearing URL.
  const item = start("direct_ssh_viewer", binary,
    ["webvnc", "--provider", "local-container", "--id", lease, "--redact-credentials=false", "--local-port", String(await port())], 10 * 60_000);
  for (let i = 0; i < 300; i++) {
    const match = item.stdout.match(/^webvnc: (http:\/\/127\.0\.0\.1:\d+\/vnc\.html\?[^\r\n]+)$/m);
    if (match) {
      const url = new URL(match[1]);
      check(url.searchParams.get("resize") === "scale", "direct_ssh_default_not_fit");
      check(url.searchParams.has("password"), "direct_ssh_credential_missing");
      return { item, url };
    }
    check(!/^webvnc: \[redacted\]$/m.test(item.stdout), "viewer_output_redacted");
    check(exists(item) && !item.timedOut && !item.overflow, "viewer_start_failed");
    await delay(200);
  }
  throw new ProofFailure("viewer_start_deadline");
}

async function geometry(exec) {
  const text = await exec("server_geometry", "xrandr", "--current");
  const match = text.match(/current (\d+) x (\d+)/);
  check(match, "server_geometry_missing");
  return { width: Number(match[1]), height: Number(match[2]) };
}

async function frame(page) {
  return page.evaluate(async () => {
    const { default: UI } = await import("/app/ui.js");
    const canvas = document.querySelector("#noVNC_container canvas");
    const rect = canvas.getBoundingClientRect();
    const pixels = canvas.getContext("2d").getImageData(0, 0, canvas.width, canvas.height).data;
    const colors = new Set();
    for (let i = 0; i < pixels.length; i += Math.max(4, Math.floor(pixels.length / 4096 / 4) * 4)) {
      colors.add(`${pixels[i]},${pixels[i + 1]},${pixels[i + 2]}`);
    }
    return { width: canvas.width, height: canvas.height, cssWidth: rect.width, cssHeight: rect.height,
      left: rect.left, top: rect.top, colors: colors.size, scale: UI.rfb.scaleViewport, resize: UI.rfb.resizeSession,
      connected: UI.connected };
  });
}

async function screenshot(page, name, failureImage = false) {
  const bytes = await page.screenshot({ path: path.join(output, `${name}.png`), timeout: 5000 });
  check(bytes.length > (failureImage ? 0 : 4096) && bytes.length < 8 * 1024 * 1024, "screenshot_size");
  proof.screenshots.push({ file: `${name}.png`, bytes: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex") });
}

async function renderedFrame(page, name) {
  // noVNC sets canvas dimensions and emits connect before its first framebuffer
  // arrives. Observe pixels within the existing readiness budget, not a sleep.
  const started = Date.now();
  const readyUntil = Math.min(started + 30_000, deadline);
  let actual = null;
  let sampledAt = 0;
  let finished = false;
  let timer;
  // Evaluation has no native timeout. The context's finally cancels unfinished
  // calls; finished prevents their late results from changing this receipt.
  try {
    await Promise.race([
      (async () => {
        while (!finished && !interrupted && Date.now() < readyUntil) {
          const sample = await frame(page);
          if (finished) return;
          actual = sample;
          sampledAt = Date.now();
          if (sampledAt >= readyUntil || !sample.connected || sample.colors > 8) return;
          await delay(Math.min(200, Math.max(1, readyUntil - Date.now())));
        }
      })(),
      new Promise((resolve) => { timer = setTimeout(resolve, Math.max(0, readyUntil - Date.now())); }),
    ]);
  } finally {
    finished = true;
    clearTimeout(timer);
  }
  if (interrupted) throw interrupted;
  if (!actual || actual.colors <= 8 || sampledAt >= readyUntil) {
    const details = { canvas: actual, frameUnavailable: !actual, elapsedMs: Date.now() - started };
    const error = new ProofFailure("blank_canvas", details);
    failure ??= error;
    try { await screenshot(page, `${name}-unready`, true); }
    catch { details.failureImageUnavailable = true; }
    throw error;
  }
  return actual;
}

async function fit(page, exec, name, expected) {
  for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport);
    await page.waitForFunction(() => document.querySelector("#noVNC_container canvas")?.width > 0);
    await delay(800);
    const actual = await renderedFrame(page, `${name}-fit-${viewport.width}`);
    const server = await geometry(exec);
    check(server.width === expected.width && server.height === expected.height, "fit_resized_server", { server, expected });
    check(actual.connected && actual.scale && !actual.resize, "fit_flags");
    check(actual.width === expected.width && actual.height === expected.height, "fit_changed_framebuffer");
    check(actual.left >= -1 && actual.top >= -1 && actual.cssWidth > 100 && actual.cssHeight > 50 &&
      actual.left + actual.cssWidth <= viewport.width + 1 && actual.top + actual.cssHeight <= viewport.height + 1,
      "fit_canvas_clipped", { viewport, canvas: actual });
    check(actual.colors > 8, "blank_canvas");
    proof.cases.push({ name: `${name}_fit_${viewport.width}`, viewport, server: expected, canvas: actual });
    await screenshot(page, `${name}-fit-${viewport.width}`);
  }
}

async function input(page, exec, name) {
  await exec("input_marker_reset", "rm", "-f", "/tmp/crabbox-resize-input-ok");
  const args = ["xterm", "-geometry", "80x20+40+40", "-title", "Resize input proof", "-e", "sh", "-c",
    'printf "Resize input proof\\n"; IFS= read -r line; test "$line" = desktop-resize-proof && touch /tmp/crabbox-resize-input-ok; sleep 30'];
  const terminal = exec === guest
    ? start("input_terminal", "docker", ["exec", "--user", "crabbox", "--env", "DISPLAY=:99", container, ...args])
    : start("input_terminal", "sudo", ["-n", "-u", "crabbox", "env", "DISPLAY=:99", ...args]);
  try {
    await exec("input_focus", "xdotool", "search", "--sync", "--name", "^Resize input proof$", "windowactivate", "--sync");
    const size = await frame(page);
    await page.locator("#noVNC_container canvas").click({ position: { x: 100 * size.cssWidth / size.width, y: 100 * size.cssHeight / size.height } });
    await page.keyboard.type("desktop-resize-proof");
    await page.keyboard.press("Enter");
    await delay(500);
    await exec("input_arrived", "test", "-f", "/tmp/crabbox-resize-input-ok");
    proof.cases.push({ name: `${name}_keyboard`, passed: true });
  } finally { await stop(terminal); }
}

async function exercise(url, exec, name, resizable) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  page.setDefaultTimeout(30_000);
  const errors = [];
  page.on("pageerror", () => errors.push(true));
  try {
    await page.goto(url.href, { waitUntil: "domcontentloaded" });
    await page.waitForFunction(() => document.documentElement.classList.contains("noVNC_connected"));
    const initial = await geometry(exec);
    await fit(page, exec, name, initial);
    await input(page, exec, name);
    if (resizable) {
      await page.setViewportSize({ width: 1100, height: 740 });
      if (!(await page.locator("#noVNC_control_bar").evaluate((element) => element.classList.contains("noVNC_open")))) {
        await page.locator("#noVNC_control_bar_handle").click();
      }
      await page.locator("#noVNC_settings_button").click();
      await page.locator("#noVNC_setting_resize").selectOption("remote");
      await page.locator("#noVNC_settings_button").click();
      await page.waitForFunction((previous) => {
        const c = document.querySelector("#noVNC_container canvas");
        return c && (c.width !== previous.width || c.height !== previous.height);
      }, initial);
      await delay(800);
      const actual = await renderedFrame(page, `${name}-remote`);
      const server = await geometry(exec);
      check(actual.resize && !actual.scale && actual.connected, "remote_flags");
      check(server.width === actual.width && server.height === actual.height &&
        server.width === 1100 && server.height === 740 && actual.colors > 8, "remote_geometry", { server, canvas: actual });
      proof.cases.push({ name: `${name}_remote`, server, canvas: actual });
      await screenshot(page, `${name}-remote`);
    }
    check(errors.length === 0, "browser_error");
  } finally { await context.close(); }
}

async function authentication(url, exec, name, legacy = false) {
  const files = ["/var/lib/crabbox/vnc.password", ...legacy ? [] : ["/var/lib/crabbox/vnc.pass"]];
  const modes = (await exec("credential_modes", "stat", "-c", "%a:%U:%h", ...files)).trim().split("\n");
  check(modes.length === files.length && modes.every((mode) => mode === "600:crabbox:1"), "credential_permissions");
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(30_000);
  try {
    const wrong = new URL(url);
    wrong.searchParams.set("autoconnect", "false");
    wrong.searchParams.set("reconnect", "false");
    // VNC compares only the first eight bytes, so change the first byte deliberately.
    const password = wrong.searchParams.get("password");
    check(password?.length > 8, "credential_missing");
    wrong.searchParams.set("password", (password[0] === "a" ? "b" : "a") + password.slice(1));
    await page.goto(wrong.href, { waitUntil: "domcontentloaded" });
    await page.waitForFunction(() => !document.documentElement.classList.contains("noVNC_loading"));
    await page.evaluate(async () => {
      const { default: UI } = await import("/app/ui.js");
      window.authProof = { rejected: false, connected: false, disconnected: false };
      UI.connect();
      UI.rfb.addEventListener("securityfailure", () => { window.authProof.rejected = true; });
      UI.rfb.addEventListener("connect", () => { window.authProof.connected = true; });
      UI.rfb.addEventListener("disconnect", () => { window.authProof.disconnected = true; });
    });
    await page.waitForFunction(() => window.authProof.disconnected || window.authProof.connected);
    const result = await page.evaluate(() => window.authProof);
    check(result.rejected && result.disconnected && !result.connected, "wrong_password_not_rejected");
    proof.cases.push({ name: `${name}_authentication`, wrongPasswordRejected: true, privateCredentialFiles: true });
  } finally { await context.close(); }
}

async function loopback(name) {
  const listeners = await run("installer_loopback", "ss", ["-ltnH", "sport = :5900"]);
  check(listeners.trim() && listeners.trim().split("\n").every((line) => /(?:127\.0\.0\.1|\[::1\]):5900\s/.test(line)), "non_loopback_vnc");
  proof.cases.push({ name: `${name}_loopback`, passed: true });
}

async function dependencyIdentity(exec, name, legacy = false) {
  // The installer source is release-pinned; distro packages are current. Bind
  // the actual server and noVNC bytes separately instead of claiming old packages.
  const files = ["/usr/share/novnc/app/ui.js", "/usr/share/novnc/core/rfb.js",
    ...legacy ? ["/usr/bin/Xvfb", "/usr/bin/x11vnc"] : ["/usr/bin/Xtigervnc"]];
  const lines = (await exec("dependency_identity", "sha256sum", ...files)).trim().split("\n");
  check(lines.length === files.length, "dependency_identity_count");
  const identities = lines.map((line, index) => {
    check(/^[a-f0-9]{64}  /.test(line) && line.slice(66) === files[index], "dependency_identity_shape");
    return { file: files[index], sha256: line.slice(0, 64) };
  });
  proof.cases.push({ name: `${name}_dependencies`, identities });
}

async function desktopServiceState() {
  const units = ["crabbox-xvfb.service", "crabbox-desktop.service", "crabbox-x11vnc.service"];
  const text = await run("desktop_service_state", "systemctl", ["show", ...units,
    "--property=Id,ActiveState,SubState,Result,ExecMainStatus,NRestarts"], 5000);
  const enums = {
    ActiveState: ["active", "inactive", "activating", "deactivating", "failed", "reloading", "maintenance"],
    SubState: ["running", "dead", "failed", "auto-restart", "start", "start-pre", "start-post", "stop", "stop-sigterm", "stop-sigkill", "stop-post", "exited"],
    Result: ["success", "exit-code", "signal", "core-dump", "timeout", "watchdog", "exec-condition", "start-limit-hit", "resources", "oom-kill", "protocol"],
  };
  const records = text.trim().split(/\n\n+/).map((block) => Object.fromEntries(block.split("\n").map((line) => {
    const split = line.indexOf("=");
    return [line.slice(0, split), line.slice(split + 1)];
  })));
  check(records.length === units.length && new Set(records.map((record) => record.Id)).size === units.length &&
    records.every((record) => units.includes(record.Id)), "service_diagnostic_identity");
  return records.map((record) => {
    const result = { unit: record.Id };
    for (const [key, values] of Object.entries(enums)) result[key] = values.includes(record[key]) ? record[key] : "unknown";
    for (const key of ["ExecMainStatus", "NRestarts"]) {
      const value = Number(record[key]);
      result[key] = /^\d+$/.test(record[key]) && Number.isSafeInteger(value) ? value : null;
    }
    return result;
  });
}

async function cleanup(name, action) {
  try { await action(); proof.cleanup[name] = true; }
  catch (error) {
    proof.cleanup[name] = false;
    proof.cleanup.failures ??= [];
    if (proof.cleanup.failures.length < 16) proof.cleanup.failures.push({ name, ...project(error) });
    proof.passed = false;
  }
}

try {
  check((await run("source_identity", "git", ["rev-parse", "HEAD"])).trim() === proof.source, "source_drift");
  check((await run("source_clean", "git", ["status", "--porcelain", "--untracked-files=no"])).trim() === "", "source_dirty");
  const build = await run("binary_identity", "go", ["version", "-m", binary]);
  check(build.includes(`vcs.revision=${proof.source}`) && build.includes("vcs.modified=false"), "binary_source_mismatch");
  proof.binarySha256 = createHash("sha256").update(await fs.readFile(binary)).digest("hex");
  phase = "lease_preimage";
  check((await containerIDs()).trim() === "", "lease_preexists");
  check(await absent(claimPath) && await absent(keyDirectory), "lease_state_preexists");
  phase = "installer_preimage";
  // Root owns these paths; unprivileged lstat can fail inside sudoers.d even
  // when the requested file is absent. Check dangling symlinks as well.
  await run("installer_path_preimage", "sudo", ["-n", "sh", "-c",
    'for file do if [ -e "$file" ] || [ -L "$file" ]; then exit 1; fi; done', "sh",
    "/var/lib/crabbox", "/etc/systemd/system/crabbox-xvfb.service", "/etc/systemd/system/crabbox-desktop.service",
    "/etc/systemd/system/crabbox-x11vnc.service", "/usr/local/bin/crabbox-start-desktop", "/etc/sudoers.d/crabbox-desktop-reset",
    "/tmp/.X99-lock", "/tmp/.X11-unix/X99"]);
  await run("installer_account_preimage", "sh", ["-c", "! getent passwd crabbox"]);
  check((await run("installer_port_preimage", "ss", ["-ltnH", "sport = :5900"])).trim() === "", "installer_port_preexists");
  phase = "browser_launch";
  browser = await chromium.launch({ headless: true });
  phase = "local_container_bootstrap";
  acquisitionStarted = true;
  await cb("warmup", "warmup", "--provider", "local-container", "--lease-id", lease, "--slug", slug,
    "--desktop", "--desktop-env", "xfce", "--ttl", "35m", "--idle-timeout", "10m");
  container = await ownedContainer();
  check(container, "acquired_container_missing");
  await cb("desktop_doctor", "desktop", "doctor", "--provider", "local-container", "--id", lease);
  await guest("tigervnc_running", "pgrep", "-x", "Xtigervnc");
  await dependencyIdentity(guest, "local-container");
  proof.cases.push({ name: "local-container_bootstrap", desktopDoctor: true, tigerVNC: true });
  phase = "direct_ssh_viewer";
  const direct = await viewer();
  try {
    await authentication(direct.url, guest, "local-container");
    await exercise(direct.url, guest, "local-container", true);
  }
  finally { await stop(direct.item); }
  phase = "released_installer_source";
  const source = await run("released_installer_source", "curl", ["--disable", "--fail", "--silent", "--show-error",
    "--location", "--proto", "=https", "--proto-redir", "=https", "--connect-timeout", "10", "--max-time", "60",
    `https://raw.githubusercontent.com/openclaw/crabbox/${releasedInstaller.commit}/scripts/install-linux-desktop.sh`]);
  check(Buffer.byteLength(source) === releasedInstaller.bytes &&
    createHash("sha256").update(source).digest("hex") === releasedInstaller.sha256, "released_installer_identity");
  const releasedPath = path.join(work, "released-install-linux-desktop.sh");
  await fs.writeFile(releasedPath, source, { mode: 0o700, flag: "wx" });
  proof.releasedInstaller = releasedInstaller;
  phase = "released_installer_xvfb";
  installerStarted = true;
  await run("install_released_desktop", "sudo", ["-n", "bash", releasedPath], 12 * 60_000);
  await run("released_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service", "crabbox-x11vnc.service"]);
  await host("released_xvfb_running", "pgrep", "-x", "Xvfb");
  await loopback("released-installer");
  await dependencyIdentity(host, "released-installer", true);
  const password = (await run("installer_credential", "sudo", ["-n", "cat", "/var/lib/crabbox/vnc.password"])).trim();
  const webPort = await port();
  const web = start("installer_novnc", "websockify", ["--web", "/usr/share/novnc", `127.0.0.1:${webPort}`, "127.0.0.1:5900"], 12 * 60_000);
  const installedURL = new URL(direct.url);
  installedURL.port = String(webPort);
  installedURL.searchParams.set("port", String(webPort));
  installedURL.searchParams.set("password", password);
  await delay(500);
  try {
    await authentication(installedURL, host, "released-installer", true);
    await exercise(installedURL, host, "released-installer", false);
    phase = "installer_tigervnc_upgrade";
    await run("install_desktop", "sudo", ["-n", "bash", path.join(root, "scripts/install-linux-desktop.sh")], 12 * 60_000);
    await run("installer_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service"]);
    await host("installer_tigervnc_running", "pgrep", "-x", "Xtigervnc");
    const obsolete = await run("obsolete_exporter", "systemctl", ["show", "crabbox-x11vnc.service", "--property=ActiveState,UnitFileState"]);
    check(obsolete.trim().split("\n").sort().join("\n") === "ActiveState=inactive\nUnitFileState=disabled", "obsolete_exporter_active");
    await loopback("installer-upgrade");
    await dependencyIdentity(host, "installer-upgrade");
    installedURL.searchParams.set("password", (await run("upgrade_credential", "sudo", ["-n", "cat", "/var/lib/crabbox/vnc.password"])).trim());
    await authentication(installedURL, host, "installer-upgrade");
    await exercise(installedURL, host, "installer-upgrade", true);
    phase = "installer_reset";
    await host("installer_reset", "sudo", "-n", "/bin/bash", "/usr/local/bin/crabbox-start-desktop");
    await run("reset_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service"]);
    await host("reset_tigervnc_running", "pgrep", "-x", "Xtigervnc");
    await loopback("installer-reset");
    await authentication(installedURL, host, "installer-reset");
    await exercise(installedURL, host, "installer-reset", true);
    phase = "installer_depth8_transition";
    await run("install_depth8", "sudo", ["-n", "env", "CRABBOX_DESKTOP_GEOMETRY=1920x1080x8", "bash", path.join(root, "scripts/install-linux-desktop.sh")], 12 * 60_000);
    await run("depth8_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service", "crabbox-x11vnc.service"]);
    await host("depth8_xvfb_running", "pgrep", "-x", "Xvfb");
    await loopback("installer-depth8-transition");
    await dependencyIdentity(host, "installer-depth8-transition", true);
    installedURL.searchParams.set("password", (await run("depth8_credential", "sudo", ["-n", "cat", "/var/lib/crabbox/vnc.password"])).trim());
    await authentication(installedURL, host, "installer-depth8-transition", true);
    await exercise(installedURL, host, "installer-depth8-transition", false);
  } finally { await stop(web); }
  if (interrupted) throw interrupted;
  check(Date.now() < deadline, "proof_deadline");
  proof.passed = true;
} catch (error) {
  failure = interrupted || failure || error;
  proof.passed = false;
  proof.failure = { phase, ...project(failure) };
  if (installerStarted) {
    try { proof.desktopServiceState = await desktopServiceState(); }
    catch { proof.desktopServiceStateUnavailable = true; }
  }
} finally {
  cleaning = true;
  clearTimeout(deadlineTimer);
  cleanupDeadline = Date.now() + 3 * 60_000;
  await cleanup("browser", async () => {
    if (browser) await Promise.race([browser.close(), delay(5000).then(() => { throw new ProofFailure("browser_close_deadline"); })]);
  });
  for (const item of [...children]) await cleanup(`group_${item.label}`, () => stop(item));
  await cleanup("container", async () => {
    if (acquisitionStarted) {
      const actual = await ownedContainer();
      check(!container || !actual || actual === container, "cleanup_identity_drift");
      releaseClaim = await readClaim();
      if (actual || releaseClaim) await cb("release_owned_lease", "stop", "--provider", "local-container", lease);
      check((await containerIDs()).trim() === "", "container_remains");
    }
  });
  await cleanup("leaseState", async () => {
    check(await absent(keyDirectory), "lease_keys_remain");
    const terminal = await readClaim();
    if (!releaseClaim) check(!terminal, "unexpected_lease_claim");
    else {
      // Fixed IDs retain immutable create intent so replay cannot recreate a
      // released resource. Validate that tombstone before disposing private work.
      check(terminal?.fixedCreateIntent.state === "released" &&
        terminal.fixedCreateIntent.fingerprint === releaseClaim.fixedCreateIntent.fingerprint &&
        terminal.providerScope === releaseClaim.providerScope &&
        terminal.fixedCreateIntent.checkpointId === releaseClaim.fixedCreateIntent.checkpointId &&
        !terminal.fixedCreateIntent.attempt && !terminal.fixedCreateIntent.failedAttempts &&
        !terminal.cloudID && !terminal.labels && !terminal.sshHost && !terminal.sshPort,
        "lease_release_record_invalid");
      proof.cleanup.releasedIntentRetained = true;
    }
  });
  if (installerStarted) {
    for (const unit of ["crabbox-x11vnc.service", "crabbox-desktop.service", "crabbox-xvfb.service"]) {
      await cleanup(unit, async () => {
        if (!(await absent(`/etc/systemd/system/${unit}`))) await run("stop_installer_service", "sudo", ["-n", "systemctl", "stop", unit]);
      });
    }
    await cleanup("installerListener", async () => {
      check((await run("installer_stopped", "ss", ["-ltnH", "sport = :5900"])).trim() === "", "installer_listener_remains");
    });
  }
  for (const item of [...children]) await cleanup(`final_group_${item.label}`, () => stop(item));
  proof.cleanup.processGroups = children.size === 0;
  if (!proof.cleanup.processGroups) proof.passed = false;
  await cleanup("privateWork", async () => {
    check(!proof.cleanup.failures?.length && proof.cleanup.processGroups, "private_work_preserved_for_failed_cleanup");
    await fs.rm(work, { recursive: true });
  });
  await fs.writeFile(path.join(output, "proof.json"), `${JSON.stringify(proof, null, 2)}\n`);
  console.log(JSON.stringify(proof));
}
if (failure || !proof.passed) process.exitCode = 1;
