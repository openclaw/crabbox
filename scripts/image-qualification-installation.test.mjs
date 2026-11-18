import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { once } from "node:events";
import { Cloudflare } from "./image-qualification-control.mjs";
import { canonical, digest } from "./image-qualification-isolation.mjs";
import {
  installationId,
  readInstallation,
  withInstallationJournal,
} from "./image-qualification-installation.mjs";

function descriptor() {
  const module = { module: "index.js", count: 1, sha256: "a".repeat(64) };
  const record = {
    profile: "qualification-installation/v1",
    accountId: "0".repeat(32),
    candidateSha: "b".repeat(40),
    candidate: { name: "crabbox-image-qualification-9-1", namespaceId: "c".repeat(32), module },
    controller: {
      name: "crabbox-image-qualification-controller",
      module: { ...module, module: "controller.mjs" },
    },
    relay: {
      name: "crabbox-image-qualification-relay-9-1",
      module: { ...module, module: "relay.mjs" },
    },
  };
  record.id = installationId(record);
  return record;
}

test("protected descriptor fails closed and stable ownership excludes only namespace allocation", () => {
  const record = descriptor();
  const env = (r) => ({
    CLOUDFLARE_ACCOUNT_ID: record.accountId,
    QUALIFICATION_INSTALLATION_JSON: JSON.stringify(r),
    QUALIFICATION_INSTALLATION_SHA256: digest(canonical(r)),
  });
  assert.deepEqual(readInstallation(env(record)), record);
  for (const change of [
    (r) => (r.id = "d".repeat(64)),
    (r) => (r.candidateSha = ""),
    (r) => (r.extra = true),
    (r) => (r.candidate.name = "production"),
    (r) => (r.controller.module.sha256 = ""),
    (r) => (r.relay.name = "foreign"),
  ]) {
    const value = structuredClone(record);
    change(value);
    assert.throws(() => readInstallation(env(value)));
  }
  assert.throws(() => readInstallation({ ...env(record), CLOUDFLARE_ACCOUNT_ID: "f".repeat(32) }));
  assert.throws(() =>
    readInstallation({ ...env(record), QUALIFICATION_INSTALLATION_SHA256: "e".repeat(64) }),
  );
  const changed = structuredClone(record);
  changed.candidate.namespaceId = "d".repeat(32);
  assert.equal(installationId(changed), record.id);
  assert.notEqual(
    digest(canonical(changed)),
    digest(canonical(record)),
    "protected descriptor still pins exact namespace",
  );
});

for (const fault of ["lost", "denied", "malformed", "cancel", "deadline", "success"]) {
  test(`HTTP durable operation ${fault} keeps pre-write identity, sanitized outcome and refuses replay`, async (t) => {
    const directory = fs.realpathSync(
      fs.mkdtempSync(path.join(os.tmpdir(), "qualification-installation-")),
    );
    t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
    let mutations = 0;
    const stop = new AbortController();
    const server = http.createServer(async (req, res) => {
      for await (const _ of req) {
        /* Consume request before acknowledging. */
      }
      mutations++;
      const intent = JSON.parse(
        fs.readFileSync(path.join(directory, "installation-operation.json")),
      );
      assert.equal(intent.events.at(-1).outcome, "intent");
      assert.equal(intent.modules.candidate.sha256, "a".repeat(64));
      if (fault === "lost") {
        res.destroy();
        return;
      }
      if (["cancel", "deadline"].includes(fault)) {
        if (fault === "cancel") stop.abort();
        return;
      }
      res.writeHead(fault === "denied" ? 403 : 200, { "cf-ray": "0123456789abcdef-DFW" });
      res.end(
        fault === "malformed"
          ? "synthetic-private-response"
          : JSON.stringify({
              success: fault !== "denied",
              result: {
                deployment_id: "11111111-1111-4111-8111-111111111111",
                ignored: "synthetic-private-response",
              },
            }),
      );
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    t.after(() => {
      server.closeAllConnections();
      server.close();
    });
    const cf = new Cloudflare("0".repeat(32), "synthetic-private-token");
    cf.base = `http://127.0.0.1:${server.address().port}`;
    cf.installation = descriptor();
    cf.admission = {
      authority: { version: "11111111-1111-4111-8111-111111111111", sourceSha: "c".repeat(40) },
    };
    const run = () =>
      withInstallationJournal(
        cf,
        directory,
        "prepare-run",
        (client) =>
          client.request(`/workers/scripts/${cf.installation.candidate.name}`, {
            method: "PUT",
            body: "synthetic-private-request",
          }),
        { signal: stop.signal, deadline: Date.now() + (fault === "deadline" ? 150 : 5000) },
      );
    if (fault === "success") await run();
    else await assert.rejects(run());
    const raw = fs.readFileSync(path.join(directory, "installation-operation.json"), "utf8");
    const state = JSON.parse(raw);
    assert.equal(state.outcome, fault === "success" ? "verified" : "unresolved");
    assert.equal(state.events[0].outcome, fault === "success" ? "acknowledged" : "uncertain");
    assert.equal(state.events[0].role, "candidate");
    if (fault === "denied") assert.equal(state.events[0].failure.status, 403);
    assert.ok(!raw.includes("synthetic-private"));
    await assert.rejects(run(), /never replay/);
    assert.equal(mutations, 1);
  });
}
