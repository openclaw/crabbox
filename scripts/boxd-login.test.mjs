import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import https from "node:https";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { consoleOrigin, deviceLogin, loginURL } from "./boxd-login.mjs";

const session = "fixture-device-session-private";
const token = "fixture-operator-jwt-private";

test("Boxd login origin and approval URL validation", () => {
  assert.equal(consoleOrigin("https://APP.BOXD.SH:443/"), "https://app.boxd.sh");
  for (const raw of ["http://app.boxd.sh", "https://user:pass@app.boxd.sh", "https://app.boxd.sh/api", "https://app.boxd.sh/foo/..", "https://@app.boxd.sh", " https://app.boxd.sh", "https://app.boxd.sh?", "https://app.boxd.sh#", "https://app.boxd.sh\\evil"]) {
    assert.throws(() => consoleOrigin(raw), /HTTPS/);
  }
  const origin = "https://app.boxd.sh";
  assert.equal(loginURL(`${origin}/link?session=${session}`, origin, session), `${origin}/link?session=${session}`);
  for (const raw of [`http://app.boxd.sh/link?session=${session}`, `https://other.example/link?session=${session}`, `${origin}/other?session=${session}`, `${origin}/link?session=other`, `${origin}/link?session=${session}&session=${session}`, `${origin}/link?session=${session}&token=unsafe`, `${origin}/link?session=${session}#unsafe`]) {
    assert.throws(() => loginURL(raw, origin, session), /Boxd/);
  }
});

test("Boxd HTTPS device login writes a private session without CLI or API-key exchange", async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "boxd-login-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const config = path.join(dir, "openssl.cnf");
  fs.writeFileSync(config, "[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\n");
  const created = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", config, "-keyout", path.join(dir, "key.pem"), "-out", path.join(dir, "cert.pem")], { encoding: "utf8" });
  assert.equal(created.status, 0, "could not create local TLS fixture");
  let mode = "success";
  let polls = 0;
  const calls = [];
  let origin;
  const server = https.createServer({ key: fs.readFileSync(path.join(dir, "key.pem")), cert: fs.readFileSync(path.join(dir, "cert.pem")) }, async (req, res) => {
    const url = new URL(req.url, origin);
    calls.push(`${req.method} ${url.pathname}`);
    let data = "";
    for await (const chunk of req) data += chunk;
    res.setHeader("Content-Type", "application/json");
    if (mode === "redirect" || mode === "poll-redirect" && url.pathname.endsWith("/poll")) {
      res.writeHead(307, { Location: `${origin}/stolen?token=${token}` }); res.end(token); return;
    }
    if (mode === "disconnect") { req.socket.destroy(); return; }
    if (mode === "unsafe") { res.writeHead(500); res.end(session + token); return; }
    if (mode === "large") { res.end(JSON.stringify({ data: "x".repeat(65537) })); return; }
    if (mode === "malformed") { res.end(session + token); return; }
    if (url.pathname === "/api/v1/auth/device") {
      assert.equal(req.method, "POST");
      assert.deepEqual(JSON.parse(data), {});
      assert.equal(req.headers.authorization, undefined);
      const login = mode === "foreign-url" ? `https://other.example/link?session=${session}` : `${origin}/link?session=${session}`;
      res.end(JSON.stringify({ session, url: login, expires_in: mode === "expired" ? 0.03 : mode === "bad-expiry" ? -1 : 30 }));
    } else if (url.pathname === "/api/v1/auth/device/poll") {
      assert.equal(req.method, "GET");
      assert.equal(req.headers.authorization, undefined);
      assert.equal(url.searchParams.get("session"), session);
      polls++;
      if (["pending", "expired", "cancel"].includes(mode) || mode === "approve-later" && polls === 1) { res.end(JSON.stringify({ status: "pending", token: "", user_id: "", display_name: "" })); return; }
      if (mode === "denied") { res.end(JSON.stringify({ status: "denied", reason: token })); return; }
      res.end(JSON.stringify({ status: "authorized", token: mode === "api-key" ? "bxd_fixture-key" : token, user_id: "alice", display_name: "Fixture" }));
    } else if (url.pathname === "/api/v1/whoami") {
      assert.equal(req.headers.authorization, `Bearer ${token}`);
      res.end(JSON.stringify({ user_id: mode === "account-mismatch" ? "other" : "alice" }));
    } else { assert.fail("unexpected request: no vendor CLI or API-key exchange is supported"); }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  origin = `https://127.0.0.1:${server.address().port}`;
  const agent = new https.Agent({ ca: fs.readFileSync(path.join(dir, "cert.pem")) });
  t.after(() => { agent.destroy(); server.closeAllConnections(); server.close(); });
  const prompts = [];
  const options = { apiURL: origin, agent, prompt: (url) => prompts.push(url) };
  for (const successful of ["success", "approve-later"]) {
    mode = successful; polls = 0;
    const output = path.join(dir, successful + ".token");
    await deviceLogin({ ...options, output });
    assert.equal(fs.readFileSync(output, "utf8"), token + "\n");
    assert.equal(fs.statSync(output).mode & 0o777, 0o600);
    assert.equal(prompts.at(-1), `${origin}/link?session=${session}`);
    assert.ok(!prompts.some((text) => text.includes(token)), "token leaked into the operator prompt");
  }
  for (const failed of ["foreign-url", "redirect", "poll-redirect", "unsafe", "disconnect", "large", "malformed", "expired", "bad-expiry", "denied", "api-key", "account-mismatch", "cancel"]) {
    mode = failed; polls = 0;
    const output = path.join(dir, failed + ".token");
    const before = calls.length;
    const controller = new AbortController();
    const timer = failed === "cancel" ? setTimeout(() => controller.abort(), 50) : null;
    const start = Date.now();
    await assert.rejects(deviceLogin({ ...options, output, signal: controller.signal }), (error) => {
      assert.ok(!error.message.includes(token) && !error.message.includes(session), "unsafe diagnostic");
      return true;
    });
    if (timer) clearTimeout(timer);
    assert.equal(fs.existsSync(output), false, "failed login left a partial token file");
    if (["cancel", "expired"].includes(failed)) assert.ok(Date.now() - start < 1500, "polling ignored its bound");
    if (failed === "redirect") assert.equal(calls.length, before + 1, "followed redirect");
  }
  mode = "success";
  const existing = path.join(dir, "existing");
  const symlink = path.join(dir, "symlink");
  fs.writeFileSync(existing, "do not overwrite");
  fs.symlinkSync(existing, symlink);
  const before = calls.length;
  for (const output of [existing, symlink]) {
    await assert.rejects(deviceLogin({ ...options, output }), /new file/);
  }
  await assert.rejects(deviceLogin({ ...options, apiURL: origin.replace("https:", "http:"), output: path.join(dir, "insecure") }), /HTTPS/);
  assert.equal(calls.length, before, "unsafe file/origin reached device auth");
  assert.equal(fs.readFileSync(existing, "utf8"), "do not overwrite");
  const cli = spawnSync(process.execPath, [path.join(import.meta.dirname, "boxd-login.mjs"), "--output", path.join(dir, "no-tty"), "--api-url", origin], { encoding: "utf8" });
  assert.equal(cli.status, 1);
  assert.equal(cli.stdout, "");
  assert.doesNotMatch(cli.stderr, new RegExp(`${token}|${session}`));
  assert.equal(calls.length, before, "non-interactive login attempted authentication");
});
