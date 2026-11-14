import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { idleFixture, start } from "./fixtures/qualification-idle-controller.mjs";
import {
  idleBindingProjection,
  validateIdlePlan,
  idleController,
} from "./image-qualification-idle-controller.mjs";
import controller from "./image-qualification-controller-worker.mjs";
import { withQualificationJournal, readPrivateJSON } from "./image-qualification-journal.mjs";

test("runtime-only props permit only proven idle discovery and ordinary settlement", async () => {
  const f = idleFixture();
  await f.run();
  assert.equal(f.state.settled, true);
  assert.equal(f.state.failed, false);
  assert.equal(f.state.discovery, "idle");
  assert.equal(f.model.worker, null);
  assert.equal(f.model.calls.filter((c) => c.method === "PUT").length, 1);
  assert.deepEqual(
    f.model.runtime.map((v) => v.split("/").at(-1)),
    ["binding-identity", "discover"],
  );
  assert.ok(!JSON.stringify(f.state).includes("f".repeat(64)));
  const before = f.model.runtime.length;
  await f.recover();
  assert.equal(f.model.runtime.length, before);
  assert.equal(f.model.calls.filter((c) => c.method === "PUT").length, 1);
});
for (const options of [
  { keepProps: true },
  { lostUpload: true },
  { lostDelete: true },
  { lostExposure: true },
  { discover: { run: { active: true } } },
  { props: { deploymentHash: "0".repeat(64) } },
  { reply: {} },
]) {
  test(`known-owned caller settles without proof fabrication ${JSON.stringify(options)}`, async () => {
    const f = idleFixture(options);
    await f.run();
    assert.equal(f.state.settled, true);
    assert.equal(f.model.worker, null);
    if (options.props || options.reply)
      assert.ok(!f.model.runtime.some((v) => v.endsWith("/discover")));
  });
}
test("unobserved uncertain upload plus absence stays unknown through recovery", async () => {
  const f = idleFixture({ lostUploadAbsent: true });
  await assert.rejects(f.run(), /unobserved create/);
  await assert.rejects(f.recover(), /unobserved create/);
  assert.equal(f.state.settled, false);
  assert.equal(f.model.calls.filter((c) => c.method === "PUT").length, 1);
});
for (const options of [
  { binding: { environment: "foreign" } },
  { binding: { props: { deploymentHash: "0".repeat(64) } } },
  { binding: { namespace: "foreign" } },
  { authorId: "0".repeat(32) },
  { module: "0".repeat(64) },
  { number: 2 },
]) {
  test(`creation drift cannot authorize cleanup ${JSON.stringify(options)}`, async () => {
    const f = idleFixture(options);
    await assert.rejects(f.run());
    assert.equal(f.state.settled, false);
    assert.equal(f.model.calls.filter((c) => c.method === "DELETE").length, 0);
    assert.equal(f.model.runtime.length, 0);
  });
}
test("denied deletion recovers only exact caller without invoking or recreating", async () => {
  const f = idleFixture({ refuseDelete: true });
  await assert.rejects(f.run(), /deletion unconfirmed/);
  const count = f.model.runtime.length;
  f.options.refuseDelete = false;
  await f.recover();
  assert.equal(f.state.settled, true);
  assert.equal(f.model.runtime.length, count);
  assert.equal(f.model.calls.filter((c) => c.method === "PUT").length, 1);
});
test("cancellation during identity never reaches discovery but retains cleanup", async () => {
  const stop = new AbortController();
  const f = idleFixture({ signal: stop.signal, invoke: async () => stop.abort() });
  await f.run();
  assert.equal(f.state.settled, true);
  assert.equal(f.state.failed, true);
  assert.equal(f.model.runtime.length, 1);
});
test("deadline and subsequent failed verification cannot inherit settled success", async () => {
  const f = idleFixture();
  await f.run();
  f.model.time = start + 1200000;
  await assert.rejects(f.recover(), /clock expired/);
  assert.equal(f.state.settled, false);
});
test("invalid source, corrupt state or missing custody fail before management", async () => {
  const f = idleFixture();
  f.state.sequence = 5;
  await assert.rejects(f.run(), /receipt/);
  assert.equal(f.model.calls.length, 0);
  f.plan.sourceSha = "bad";
  assert.throws(() => validateIdlePlan(f.plan));
});
test("request budget is persisted before dispatch; failed persistence never uploads", async () => {
  let writes = 0;
  const f = idleFixture({
    save: async () => {
      if (++writes === 3) throw new Error("disk failure");
    },
  });
  await f.run();
  assert.equal(f.state.failed, true);
  assert.equal(f.state.settled, true);
  assert.equal(f.model.calls.filter((c) => c.method !== "GET").length, 0);
});
test("visible projection rejects unexpected non-service fields", () => {
  const f = idleFixture();
  assert.throws(() =>
    idleBindingProjection(f.plan, [
      { name: "CONTROLLER_TOKEN", type: "secret_text", namespace: "hidden" },
    ]),
  );
});
test("private journal rejects missing recovery, symlinks, locks and corruption", async (t) => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "idle-custody-")));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const config = { stateName: "idle-controller-state.json", lockName: "idle-controller.lock" };
  await assert.rejects(
    withQualificationJournal(dir, config, () => assert.fail()),
    /existing durable/,
  );
  await withQualificationJournal(
    dir,
    { ...config, create: true, initial: () => ({ test: true }) },
    async (state, save) => {
      await assert.rejects(
        withQualificationJournal(dir, config, () => assert.fail()),
        /EEXIST/,
      );
      await save({ test: false });
      assert.equal(readPrivateJSON(path.join(dir, config.stateName)).test, false);
    },
  );
  fs.chmodSync(path.join(dir, config.stateName), 0o644);
  assert.throws(() => readPrivateJSON(path.join(dir, config.stateName)), /private file/);
});
test("idle HTTP controller restricts actions and forwards only a bounded challenge", async () => {
  let calls = 0;
  const challenge = "c".repeat(64);
  const version = "11111111-1111-4111-8111-111111111111";
  const env = {
    CONTROLLER_TOKEN: "a".repeat(64),
    IDLE_CONTROLLER_ONLY: "1",
    CF_VERSION_METADATA: { id: version },
    AUTHORITY: {
      bindingIdentity: async (actual) => {
        calls++;
        assert.equal(actual, challenge);
        return { actual };
      },
    },
  };
  const request = (url, value, token = env.CONTROLLER_TOKEN) =>
    new Request(`https://fixture.invalid/${url}`, {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: JSON.stringify(value),
    });
  for (const action of ["claim", "finalize", "retire", "attest", "begin-finalization"])
    assert.equal((await controller.fetch(request(action, {}), env)).status, 404);
  assert.equal(
    (await controller.fetch(request("binding-identity", { challenge }, "wrong"), env)).status,
    403,
  );
  assert.equal(
    (await controller.fetch(request("binding-identity", { challenge, expected: {} }), env)).status,
    409,
  );
  assert.equal(
    (await controller.fetch(request("binding-identity", { challenge: "a".repeat(5000) }), env))
      .status,
    409,
  );
  const response = await controller.fetch(request("binding-identity", { challenge }), env);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store");
  assert.equal(calls, 1);
});

for (const point of ["create", "exposure", "identity", "discovery", "delete", "before-create"]) {
  test(`fresh process recovers crash at ${point} using original custody only`, async (t) => {
    const { spawnSync } = await import("node:child_process");
    const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "idle-crash-")));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const runner = new URL("./fixtures/qualification-idle-crash-runner.mjs", import.meta.url);
    const spawn = (mode, crash = "none") =>
      spawnSync(process.execPath, [runner.pathname, dir, mode, crash], {
        encoding: "utf8",
        timeout: 30000,
        env: {},
      });
    const first = spawn("run", point);
    assert.equal(first.status, 91, first.stderr);
    const before = fs.existsSync(path.join(dir, "synthetic-cloud.json"))
      ? JSON.parse(fs.readFileSync(path.join(dir, "synthetic-cloud.json")))
      : { calls: [], runtime: [] };
    const locked = spawn("recover");
    assert.equal(locked.status, 1);
    assert.match(locked.stderr, /EEXIST/);
    // Simulated operator confirms the original process exited; product code has
    // no stale-lock-reclaim switch and never performs this removal itself.
    fs.unlinkSync(path.join(dir, "idle-controller.lock"));
    const recovery = spawn("recover");
    const receipt = readPrivateJSON(path.join(dir, "idle-controller-state.json"));
    if (point === "before-create") {
      assert.equal(recovery.status, 1);
      assert.match(recovery.stderr, /unobserved create/);
      assert.equal(receipt.settled, false);
    } else {
      assert.equal(recovery.status, 0, recovery.stderr);
      assert.equal(receipt.settled, true);
    }
    const after = JSON.parse(fs.readFileSync(path.join(dir, "synthetic-cloud.json")));
    assert.equal(
      after.calls.filter((c) => c.method === "PUT").length,
      before.calls.filter((c) => c.method === "PUT").length,
    );
    assert.equal(after.runtime.length, before.runtime.length);
    assert.equal(after.worker, null);
  });
}

for (const field of ["caller-version", "authority-version", "challenge", "extra"]) {
  test(`runtime ${field} mismatch cannot authorize discovery`, async () => {
    const f = idleFixture({
      reply: async (init) => {
        const challenge = JSON.parse(init.body).challenge;
        const reply = {
          controllerVersion: "22222222-2222-4222-8222-222222222222",
          identity: {
            protocol: 1,
            role: "controller",
            challenge,
            props: { deploymentHash: "c".repeat(64) },
            authorityVersion: "11111111-1111-4111-8111-111111111111",
          },
        };
        if (field === "caller-version")
          reply.controllerVersion = "33333333-3333-4333-8333-333333333333";
        if (field === "authority-version")
          reply.identity.authorityVersion = "33333333-3333-4333-8333-333333333333";
        if (field === "challenge") reply.identity.challenge = "0".repeat(64);
        if (field === "extra") reply.extra = true;
        return reply;
      },
    });
    await f.run();
    assert.equal(f.state.failed, true);
    assert.equal(f.state.settled, true);
    assert.equal(f.model.runtime.length, 1);
  });
}
for (const credential of [
  { id: "0".repeat(32) },
  { status: "inactive" },
  { expires_on: "invalid" },
]) {
  test(`recovery rechecks credential and clears prior settlement ${JSON.stringify(credential)}`, async () => {
    const f = idleFixture();
    await f.run();
    f.options.credential = credential;
    await assert.rejects(f.recover(), /credential identity/);
    assert.equal(f.state.settled, false);
  });
}
test("concurrent authority reads serialize durable budget writes", async () => {
  let saving = false;
  const f = idleFixture({
    save: async () => {
      assert.equal(saving, false, "only one durable replacement may be in flight");
      saving = true;
      await new Promise((resolve) => setImmediate(resolve));
      saving = false;
    },
  });
  await f.run();
  assert.equal(f.state.settled, true);
  assert.ok(
    f.saved.every((v, index) => index === 0 || v.sequence === f.saved[index - 1].sequence + 1),
  );
  assert.equal(f.model.calls.length + f.model.runtime.length, f.state.requests);
});
test("identity route bounds an oversized reply and a stalled pure RPC", async () => {
  const token = "a".repeat(64),
    challenge = "c".repeat(64);
  const request = () =>
    new Request("https://fixture.invalid/binding-identity", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: JSON.stringify({ challenge }),
    });
  const env = {
    CONTROLLER_TOKEN: token,
    IDLE_CONTROLLER_ONLY: "1",
    CF_VERSION_METADATA: { id: "11111111-1111-4111-8111-111111111111" },
    AUTHORITY: { bindingIdentity: async () => "a".repeat(5000) },
  };
  assert.equal((await controller.fetch(request(), env)).status, 409);
  env.AUTHORITY.bindingIdentity = () => new Promise(() => {});
  assert.equal((await controller.fetch(request(), env)).status, 409);
});
