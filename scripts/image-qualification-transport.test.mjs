import assert from "node:assert/strict";
import test from "node:test";
import {
  proveCandidateTransport,
  settleCandidateTransport,
  transportMarkers,
  transportBindingShape,
} from "./image-qualification-transport.mjs";
import {
  assertOwnedSettings,
  ownershipTags,
  bindingShape,
  canonical,
} from "./image-qualification-isolation.mjs";
import { fixtureAdmission } from "./fixtures/qualification-isolation.mjs";
const version = "11111111-1111-1111-1111-111111111111";
function fixture(options = {}) {
  const request = {
    identity: {
      runId: "image-qualification-42-1",
      owner: "maintainer",
      candidateWorker: "crabbox-image-qualification-42-1",
      candidateSha: "a".repeat(40),
      deploymentHash: "d".repeat(64),
      expiresAt: new Date(Date.now() + 3600000).toISOString(),
    },
    challenge: "b".repeat(64),
    candidateVersion: version,
    authorityVersion: version,
    expiresAt: new Date(Date.now() + 540000).toISOString(),
  };
  let probe = null;
  const saved = [],
    calls = [];
  const controller = async (action, input) => {
    calls.push(action);
    if (options.failBefore === action) throw Error("before response");
    let result;
    if (action === "transport-open") {
      probe = { protocol: 1, ...request, state: "pending" };
      result = structuredClone(probe);
    } else if (action === "transport-discover") result = { probe: structuredClone(probe) };
    else if (action === "transport-settle") {
      assert.equal(input.challenge, request.challenge);
      assert.ok(probe);
      if (input.verified) assert.ok(["observed", "verified"].includes(probe.state));
      probe.state = input.verified ? "verified" : "cancelled";
      result = structuredClone(probe);
    } else throw Error("unexpected action");
    if (options.failAfter === action) throw Error("uncertain response");
    return result;
  };
  const args = {
    request,
    controller,
    invokeCandidate: async () => {
      if (!options.forge)
        probe = { ...probe, state: "observed", observedAt: new Date().toISOString() };
      return { identity: request.identity, challenge: request.challenge };
    },
    verifyFences: async () => {
      if (options.drift) throw Error("drift");
    },
    save: async (state) => {
      saved.push(structuredClone(state));
      if (options.failSave === state.phase) throw Error("custody write failed");
    },
    signal: options.signal,
  };
  return {
    request,
    args,
    calls,
    saved,
    get probe() {
      return probe;
    },
    set probe(value) {
      probe = value;
    },
    options,
  };
}
test("only canonical observation admits proof; candidate HTTP identity is ignored", async () => {
  const f = fixture();
  const result = await proveCandidateTransport(f.args);
  assert.equal(result.state, "verified");
  assert.equal(f.saved.at(-1).phase, "verified");
  assert.deepEqual(f.calls, [
    "transport-open",
    "transport-discover",
    "transport-settle",
    "transport-discover",
  ]);
});
test("forged expected HTTP reply without authority write fails closed", async () => {
  const f = fixture({ forge: true });
  await assert.rejects(proveCandidateTransport(f.args), /canonical/);
  assert.equal(f.probe.state, "pending");
  assert.ok(!f.calls.includes("transport-settle"));
  await settleCandidateTransport(f.args);
  assert.equal(f.probe.state, "cancelled");
});
for (const phase of ["opening", "dispatching", "verified"])
  test(`failed custody at ${phase} cannot claim`, async () => {
    const f = fixture({ failSave: phase });
    await assert.rejects(proveCandidateTransport(f.args), /custody/);
    if (phase === "opening") assert.equal(f.calls.length, 0);
    else {
      f.options.failSave = undefined;
      await settleCandidateTransport(f.args);
      assert.ok(["verified", "cancelled"].includes(f.probe.state));
    }
  });
for (const boundary of ["transport-open", "transport-discover", "transport-settle"])
  test(`unknown response after ${boundary} reconciles, never redispatches`, async () => {
    const f = fixture({ failAfter: boundary });
    await assert.rejects(proveCandidateTransport(f.args), /uncertain/);
    f.options.failAfter = undefined;
    const count = f.calls.filter((x) => x === "transport-open").length;
    await settleCandidateTransport(f.args);
    assert.equal(f.calls.filter((x) => x === "transport-open").length, count);
    await settleCandidateTransport(f.args);
    assert.ok(["verified", "cancelled"].includes(f.probe.state));
  });
test("cancellation before effect and during candidate dispatch remains recoverable", async () => {
  const stop = new AbortController();
  const f = fixture({ signal: stop.signal });
  stop.abort();
  await assert.rejects(proveCandidateTransport(f.args), /cancelled/);
  assert.equal(f.calls.length, 0);
  const stop2 = new AbortController();
  const g = fixture({ signal: stop2.signal });
  const invoke = g.args.invokeCandidate;
  g.args.invokeCandidate = async (...args) => {
    await invoke(...args);
    stop2.abort();
  };
  await assert.rejects(proveCandidateTransport(g.args), /cancelled/);
  await settleCandidateTransport(g.args);
  assert.equal(g.probe.state, "cancelled");
});
test("foreign canonical slot, extra fields and absence cannot be called settled", async () => {
  const f = fixture();
  await proveCandidateTransport(f.args);
  f.probe = { ...f.probe, extra: true };
  await assert.rejects(settleCandidateTransport(f.args), /mismatch/);
  delete f.probe.extra;
  f.probe.identity = { ...f.probe.identity, owner: "foreign" };
  await assert.rejects(settleCandidateTransport(f.args), /mismatch/);
  f.probe = null;
  await assert.rejects(settleCandidateTransport(f.args), /unobserved/);
});
test("explicit v2 ownership accepts only the witnessed missing props/default environment; legacy unchanged", () => {
  const f = fixture();
  const admission = fixtureAdmission().admission;
  const worker = f.request.identity.candidateWorker;
  const original = [
    ...transportMarkers("final-v2", f.request.identity),
    {
      name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
      type: "service",
      service: "crabbox-aws-qualification-authority",
      props: f.request.identity,
    },
  ];
  const settings = { bindings: original, tags: ownershipTags({ admission }, worker, original) };
  const visible = structuredClone(settings);
  delete visible.bindings.at(-1).props;
  visible.bindings.at(-1).environment = "production";
  assertOwnedSettings({ admission }, worker, visible);
  assert.equal(
    canonical(bindingShape(transportBindingShape(original))),
    canonical(bindingShape(transportBindingShape(visible.bindings))),
  );
  for (const patch of [
    { environment: "foreign" },
    { props: { ...f.request.identity, owner: "foreign" } },
    { entrypoint: "Other" },
    { namespace: "extra" },
  ]) {
    const changed = structuredClone(visible);
    Object.assign(changed.bindings.at(-1), patch);
    assert.throws(() => assertOwnedSettings({ admission }, worker, changed));
  }
  const mixed = structuredClone(visible);
  mixed.tags.push("qualification-worker:foreign");
  assert.throws(() => assertOwnedSettings({ admission }, worker, mixed));
  const legacy = original.filter((x) => !x.name.startsWith("QUALIFICATION_TRANSPORT_"));
  const bad = {
    bindings: structuredClone(legacy),
    tags: ownershipTags({ admission }, worker, legacy),
  };
  delete bad.bindings[0].props;
  assert.throws(() => assertOwnedSettings({ admission }, worker, bad));
});
test("staging ownership permits no service transport; failed proof does not lose creation custody", () => {
  const f = fixture();
  const admission = fixtureAdmission().admission;
  const worker = f.request.identity.candidateWorker;
  const bindings = transportMarkers("stage-v2", f.request.identity);
  const settings = { bindings, tags: ownershipTags({ admission }, worker, bindings) };
  assertOwnedSettings({ admission }, worker, settings);
  const extra = {
    name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
    type: "service",
    service: "crabbox-aws-qualification-authority",
    props: f.request.identity,
  };
  assert.throws(() => ownershipTags({ admission }, worker, [...bindings, extra]), /capability/);
});
