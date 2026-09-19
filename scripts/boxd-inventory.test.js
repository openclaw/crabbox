import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import https from "node:https";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");
const key = "bxd_" + "F1xtureF1xtureF1xtureF1xtureF1xtureF1xture0";
const token = "fixture-boxd-jwt";

function run(args, env) {
  return new Promise((resolve) => {
    const child = spawn("python3", [path.join(root, "scripts/boxd-inventory.py"), ...args], { env });
    let stdout = "", stderr = "";
    child.stdout.on("data", (data) => stdout += data);
    child.stderr.on("data", (data) => stderr += data);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
}

test("Boxd independent inventory uses TLS, rejects redirects, and proves scoped absence", async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "boxd-inventory-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const config = path.join(dir, "openssl.cnf");
  fs.writeFileSync(config, "[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\n");
  const generated = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", config, "-keyout", path.join(dir, "key.pem"), "-out", path.join(dir, "cert.pem")], { encoding: "utf8" });
  assert.equal(generated.status, 0, "could not create local TLS test certificate");
  let mode = "normal";
  const calls = [];
  const server = https.createServer({ key: fs.readFileSync(path.join(dir, "key.pem")), cert: fs.readFileSync(path.join(dir, "cert.pem")) }, async (req, res) => {
    calls.push(req.url);
    let body = "";
    for await (const data of req) body += data;
    res.setHeader("Content-Type", "application/json");
    if (mode === "redirect") {
      res.writeHead(307, { Location: `https://127.0.0.1:${server.address().port}/stolen?token=${token}` });
      res.end(token);
      return;
    }
    if (mode === "unsafe") { res.writeHead(500); res.end(`${key} ${token}`); return; }
    if (mode === "rate-limited-once" && calls.filter((c) => c === "/api/v1/auth/token").length === 1) {
      res.writeHead(429);
      res.end("rate limited");
      return;
    }
    if (req.url === "/api/v1/auth/token") {
      assert.equal(JSON.parse(body).api_key, key, "exchange did not carry the API key");
      res.end(JSON.stringify({ token, expires_at: Math.floor(Date.now() / 1000) + 3600, user_id: "exchange-account" }));
      return;
    }
    assert.equal(req.headers.authorization, `Bearer ${token}`);
    if (req.url === "/api/v1/whoami") { res.end(JSON.stringify({ user_id: mode === "wrong-account" ? "other" : "alice" })); return; }
    assert.equal(req.url, "/api/v1/vms", "org must come from the key's fencing, not a name parameter");
    res.end(JSON.stringify(mode === "null" ? null : mode === "missing-existing" ? [] : [
      { id: "existing", name: "crabbox-existing", status: "running" },
      ...(mode === "tombstone" ? [{ id: "gone", name: "crabbox-gone:gone", status: "destroyed" }] : []),
      ...(mode === "live-residue" ? [{ id: "leftover", name: "crabbox-leftover", status: "stopped" }] : []),
    ]));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const origin = `https://127.0.0.1:${server.address().port}`;
  const env = { ...process.env, CRABBOX_BOXD_VERIFY_ATTEMPTS: "2", CRABBOX_BOXD_VERIFY_DELAY: "0", CRABBOX_BOXD_API_KEY: key, BOXD_API_KEY: "bxd_" + "unusedUnusedUnusedUnusedUnusedUnusedUnused0", CRABBOX_BOXD_API_URL: origin, CRABBOX_BOXD_ORG: "my org", SSL_CERT_FILE: path.join(dir, "cert.pem"), NO_PROXY: "127.0.0.1" };
  const snapshot = await run(["snapshot"], env);
  assert.equal(snapshot.code, 0, snapshot.stderr);
  const baseline = path.join(dir, "before.json");
  fs.writeFileSync(baseline, snapshot.stdout);
  const proof = await run(["verify", baseline], env);
  assert.equal(proof.code, 0, proof.stderr);
  assert.match(proof.stdout, /zero residue/);
  const withTombstone = JSON.parse(snapshot.stdout);
  withTombstone.machines.push({ id: "purged", name: "crabbox-purged:purged", status: "destroyed" });
  const tombstoneBaseline = path.join(dir, "before-tombstone.json");
  fs.writeFileSync(tombstoneBaseline, JSON.stringify(withTombstone));
  const purged = await run(["verify", tombstoneBaseline], env);
  assert.equal(purged.code, 0, "flagged an expected tombstone purge as a lost machine");
  mode = "rate-limited-once";
  const rateLimited = await run(["verify", baseline], { ...env, CRABBOX_BOXD_VERIFY_ATTEMPTS: "3" });
  assert.equal(rateLimited.code, 0, "transient rate limit failed the verify window");
  mode = "tombstone";
  const tombstone = await run(["verify", baseline], env);
  assert.equal(tombstone.code, 0, "counted a destroyed tombstone as residue");
  mode = "live-residue";
  const residue = await run(["verify", baseline], env);
  assert.equal(residue.code, 1, "missed live residue");
  mode = "missing-existing";
  const lostExisting = await run(["verify", baseline], env);
  assert.equal(lostExisting.code, 1, "reported clean inventory after losing a preexisting machine");
  for (const nextMode of ["redirect", "unsafe", "null"]) {
    mode = nextMode;
    const before = calls.length;
    const failed = await run(["snapshot"], env);
    assert.equal(failed.code, 1);
    assert.doesNotMatch(failed.stdout + failed.stderr, new RegExp(`${key}|${token}|token=`));
    if (mode === "redirect") assert.equal(calls.length, before + 1, "followed credentialed redirect");
  }
  mode = "wrong-account";
  const wrongAccount = await run(["verify", baseline], env);
  assert.equal(wrongAccount.code, 1, "accepted a different authenticated account");
  mode = "normal";
  const before = calls.length;
  for (const invalid of ["", "bxd_short", token, key + "x"]) {
    const failed = await run(["snapshot"], { ...env, CRABBOX_BOXD_API_KEY: invalid, BOXD_API_KEY: "" });
    assert.equal(failed.code, 1, "accepted an invalid API key");
  }
  for (const badOrigin of [origin.replace("https:", "http:"), `${origin}/api`, `${origin}?token=${token}`, origin.replace("//", "//user:pass@")]) {
    const failed = await run(["snapshot"], { ...env, CRABBOX_BOXD_API_URL: badOrigin });
    assert.equal(failed.code, 1);
    assert.doesNotMatch(failed.stderr, /user:pass|token=/);
  }
  assert.equal(calls.length, before, "invalid origin reached authentication");
  const previous = JSON.parse(snapshot.stdout);
  previous.org = "different-org";
  fs.writeFileSync(baseline, JSON.stringify(previous));
  const changed = await run(["verify", baseline], env);
  assert.equal(changed.code, 1, "accepted proof from another organization");
});
