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
    installer: "clean_systemd_tigervnc_then_depth8_transition",
    legacyFit: "actual_legacy_novnc_with_captured_cli_url_settings",
    legacySSHStartup: false } };
let phase = "preflight";
let container = "";
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
  return error instanceof ProofFailure ? { code: error.code, details: error.details } : { code: "operation_failed" };
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
const keyDirectory = path.join(env.XDG_CONFIG_HOME, "crabbox", "testboxes", lease);

async function absent(file) {
  try { await fs.lstat(file); return false; }
  catch (error) { if (error.code === "ENOENT") return true; throw error; }
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
  const item = start("direct_ssh_viewer", binary,
    ["webvnc", "--provider", "local-container", "--id", lease, "--local-port", String(await port())], 10 * 60_000);
  for (let i = 0; i < 300; i++) {
    const match = item.stdout.match(/^webvnc: (http:\/\/127\.0\.0\.1:\d+\/vnc\.html\?[^\r\n]+)$/m);
    if (match) {
      const url = new URL(match[1]);
      check(url.searchParams.get("resize") === "scale", "direct_ssh_default_not_fit");
      check(url.searchParams.has("password"), "direct_ssh_credential_missing");
      return { item, url };
    }
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

async function screenshot(page, name) {
  const bytes = await page.screenshot({ path: path.join(output, `${name}.png`) });
  check(bytes.length > 4096 && bytes.length < 8 * 1024 * 1024, "screenshot_size");
  proof.screenshots.push({ file: `${name}.png`, bytes: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex") });
}

async function fit(page, exec, name, expected) {
  for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport);
    await page.waitForFunction(() => document.querySelector("#noVNC_container canvas")?.width > 0);
    await delay(800);
    const actual = await frame(page);
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
      const actual = await frame(page);
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
  check((await containerIDs()).trim() === "", "lease_preexists");
  check(await absent(claimPath) && await absent(keyDirectory), "lease_state_preexists");
  for (const file of ["/var/lib/crabbox", "/etc/systemd/system/crabbox-xvfb.service", "/etc/systemd/system/crabbox-desktop.service",
    "/etc/systemd/system/crabbox-x11vnc.service", "/usr/local/bin/crabbox-start-desktop", "/etc/sudoers.d/crabbox-desktop-reset",
    "/tmp/.X99-lock", "/tmp/.X11-unix/X99"]) check(await absent(file), "installer_path_preexists");
  await run("installer_account_preimage", "sh", ["-c", "! getent passwd crabbox"]);
  check((await run("installer_port_preimage", "ss", ["-ltnH", "sport = :5900"])).trim() === "", "installer_port_preexists");
  browser = await chromium.launch({ headless: true });
  phase = "local_container_bootstrap";
  acquisitionStarted = true;
  await cb("warmup", "warmup", "--provider", "local-container", "--lease-id", lease, "--slug", slug,
    "--desktop", "--desktop-env", "xfce", "--ttl", "35m", "--idle-timeout", "10m");
  container = await ownedContainer();
  check(container, "acquired_container_missing");
  await cb("desktop_doctor", "desktop", "doctor", "--provider", "local-container", "--id", lease);
  await guest("tigervnc_running", "pgrep", "-x", "Xtigervnc");
  const direct = await viewer();
  try {
    await authentication(direct.url, guest, "local-container");
    await exercise(direct.url, guest, "local-container", true);
  }
  finally { await stop(direct.item); }
  phase = "installer_tigervnc";
  installerStarted = true;
  await run("install_desktop", "sudo", ["-n", "bash", path.join(root, "scripts/install-linux-desktop.sh")], 12 * 60_000);
  await run("installer_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service"]);
  await host("installer_tigervnc_running", "pgrep", "-x", "Xtigervnc");
  await loopback("installer-tigervnc");
  const password = (await run("installer_credential", "sudo", ["-n", "cat", "/var/lib/crabbox/vnc.password"])).trim();
  const webPort = await port();
  const web = start("installer_novnc", "websockify", ["--web", "/usr/share/novnc", `127.0.0.1:${webPort}`, "127.0.0.1:5900"], 12 * 60_000);
  const installedURL = new URL(direct.url);
  installedURL.port = String(webPort);
  installedURL.searchParams.set("port", String(webPort));
  installedURL.searchParams.set("password", password);
  await delay(500);
  try {
    await authentication(installedURL, host, "installer-tigervnc");
    await exercise(installedURL, host, "installer-tigervnc", true);
    phase = "installer_depth8_transition";
    await run("install_depth8", "sudo", ["-n", "env", "CRABBOX_DESKTOP_GEOMETRY=1920x1080x8", "bash", path.join(root, "scripts/install-linux-desktop.sh")], 12 * 60_000);
    await run("depth8_services", "systemctl", ["is-active", "--quiet", "crabbox-xvfb.service", "crabbox-desktop.service", "crabbox-x11vnc.service"]);
    await host("depth8_xvfb_running", "pgrep", "-x", "Xvfb");
    await loopback("installer-depth8-transition");
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
      if (actual || !(await absent(claimPath))) await cb("release_owned_lease", "stop", "--provider", "local-container", lease);
      check((await containerIDs()).trim() === "", "container_remains");
    }
  });
  await cleanup("leaseState", async () => {
    check(await absent(claimPath) && await absent(keyDirectory), "lease_state_remains");
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
