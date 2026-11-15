// Cleanup-only transition for an already-finalizing owner. No deployment,
// enrollment, credentials, arbitrary caller adoption or replacement authority.
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  Cloudflare,
  recoverActiveQualification,
  verifyAttestationIdentity,
} from "./image-qualification-control.mjs";
import {
  authorityName,
  controllerName,
  bindingShape,
  canonical,
  digest,
  readAdmission,
  verifyAuthority,
  verifyOwned,
  ownershipTags,
} from "./image-qualification-isolation.mjs";
import { readPrivateJSON, withQualificationJournal } from "./image-qualification-journal.mjs";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const same = (a, b) => canonical(a) === canonical(b);
const sha = /^[a-f0-9]{64}$/;
const uuid = /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/;
const fail = () => {
  throw new Error("qualification recovery identity, state or boundary invalid");
};
export const recoveryBinding = (a) =>
  Object.fromEntries(
    [
      "runId",
      "candidateSha",
      "candidateWorker",
      "deploymentHash",
      "expiresAt",
      "enrolledAt",
      "authoritySha",
      "authorityVersion",
      "policyHash",
    ].map((key) => [key, a[key]]),
  );
const admission = (record) =>
  readAdmission({
    CLOUDFLARE_ACCOUNT_ID: record?.accountId,
    QUALIFICATION_ISOLATION_JSON: JSON.stringify(record),
    QUALIFICATION_ISOLATION_SHA256: digest(canonical(record)),
  });
const tombstone = "export default {fetch(){return new Response(null,{status:410})}};\n";

export function validateRecoveryPlan(plan) {
  if (
    plan?.profile !== "active-cleanup-transition/v1" ||
    !/^[a-f0-9]{40}$/.test(plan.sourceSha ?? "") ||
    !sha.test(plan.originalAttestationSha256 ?? "") ||
    !sha.test(plan.originalHandoffSha256 ?? "")
  )
    fail();
  const original = admission(plan.originalAdmission),
    corrected = admission(plan.correctedAdmission);
  const stable = ({ authority, ...rest }) => ({
    ...rest,
    authority: Object.fromEntries(
      Object.entries(authority).filter(
        ([key]) => !["version", "sourceSha", "logicalVersion", "bindingDigest"].includes(key),
      ),
    ),
  });
  if (
    !same(stable(plan.originalAdmission), stable(plan.correctedAdmission)) ||
    corrected.authority.version === original.authority.version ||
    corrected.authority.sourceSha !== plan.sourceSha ||
    corrected.authority.logicalVersion === original.authority.logicalVersion ||
    !same(plan.binding, recoveryBinding(plan.binding))
  )
    fail();
  const e = plan.binding;
  if (
    !/^image-qualification-[0-9]+-[0-9]+$/.test(e.runId ?? "") ||
    e.candidateWorker !== `crabbox-${e.runId}` ||
    !/^[a-f0-9]{40}$/.test(e.candidateSha ?? "") ||
    !sha.test(e.deploymentHash ?? "") ||
    e.authoritySha !== original.authority.sourceSha ||
    e.authorityVersion !== original.authority.logicalVersion ||
    e.policyHash !== original.authority.policyHash ||
    !Number.isFinite(Date.parse(e.enrolledAt)) ||
    !Number.isFinite(Date.parse(e.expiresAt))
  )
    fail();
  const names = [
    e.candidateWorker,
    e.candidateWorker.replace("qualification-", "qualification-relay-"),
    controllerName,
  ].sort();
  if (
    !Array.isArray(plan.callers) ||
    !same(plan.callers.map((c) => c.name).sort(), names) ||
    plan.callers.some((c) =>
      c.absent === true
        ? c.name === controllerName || !same(Object.keys(c).sort(), ["absent", "name"])
        : Object.hasOwn(c, "absent") ||
          !uuid.test(c.version ?? "") ||
          !sha.test(c.settingsSha256 ?? "") ||
          !sha.test(c.module?.sha256 ?? "") ||
          c.module.count !== 1 ||
          !/^[A-Za-z0-9_.-]+$/.test(c.module.module ?? ""),
    ) ||
    !sha.test(plan.authorityModule?.sha256 ?? "") ||
    plan.authorityModule.count !== 1 ||
    !/^[A-Za-z0-9_.-]+$/.test(plan.authorityModule.module ?? "")
  )
    fail();
  const start = Date.parse(plan.handoffStartedAt),
    deadline = Date.parse(plan.deadline);
  if (
    !Number.isFinite(start) ||
    deadline !== start + 270 * 60_000 ||
    Date.parse(e.expiresAt) !== start + 150 * 60_000 ||
    !(Date.parse(plan.sessionExpiresAt) >= deadline + 60 * 60_000) ||
    plan.maxRequests !== 800
  )
    fail();
  return { original, corrected };
}

export function initialRecoveryState(plan) {
  return {
    profile: plan.profile,
    planSha256: digest(canonical(plan)),
    sequence: 0,
    requests: 0,
    uploadIntents: {},
    attestation: null,
    retired: false,
    settled: false,
  };
}

// Existing cleanup sequence remains the owner. This adapter only separates its
// original caller identity from the corrected serving authority identity.
export async function recoverTransition(
  plan,
  state,
  {
    callerCF,
    authorityCF,
    token,
    save,
    signal,
    now = Date.now,
    run = recoverActiveQualification,
  } = {},
) {
  const { original, corrected } = validateRecoveryPlan(plan);
  if (
    state?.profile !== plan.profile ||
    state.planSha256 !== digest(canonical(plan)) ||
    !Number.isSafeInteger(state.sequence) ||
    state.sequence < 0 ||
    !Number.isSafeInteger(state.requests) ||
    state.requests < 0 ||
    !state.uploadIntents ||
    typeof token !== "string" ||
    token.length < 32
  )
    fail();
  if (state.attestation) {
    verifyAttestationIdentity(state.attestation, plan.binding, { finalized: true });
    if (
      !same(state.attestation.finalReceipt?.finalCounts, {
        images: 0,
        instances: 0,
        keyPairs: 0,
        snapshots: 0,
        volumes: 0,
      }) ||
      !same(state.attestation.finalReceipt?.failureCodes, [])
    )
      fail();
  }
  if ((state.retired && !state.attestation) || (state.settled && !state.retired)) fail();
  callerCF.admission = original;
  authorityCF.admission = corrected;
  const check = (limit = 30_000) => {
    if (signal?.aborted || now() >= Date.parse(plan.deadline) || state.requests >= plan.maxRequests)
      fail();
    return Math.min(limit, Date.parse(plan.deadline) - now());
  };
  const persist = async () => {
    state.sequence++;
    await save(structuredClone(state));
  };
  const bound = (cf) => {
    const request = cf.request.bind(cf);
    return async (route, init = {}, options) => {
      const duration = check();
      state.requests++;
      await persist();
      return await request(
        route,
        {
          ...init,
          redirect: "error",
          signal: AbortSignal.any([AbortSignal.timeout(duration), ...(signal ? [signal] : [])]),
        },
        options,
      );
    };
  };
  const originalRequest = bound(callerCF);
  authorityCF.request = bound(authorityCF);
  callerCF.beforeRecoveryCall = async () => {
    check();
    state.requests++;
    await persist();
  };
  callerCF.recoverySignal = (action) =>
    // A fresh finalization includes 120s of mandatory inventory backoffs alone.
    // Only that existing owner operation gets 5min; the absolute clock never resets.
    AbortSignal.any([
      AbortSignal.timeout(check(action === "finalize" ? 300_000 : 30_000)),
      ...(signal ? [signal] : []),
    ]);
  const moduleIdentity = (cf, name) =>
    cf.request(
      `/workers/scripts/${name}/content/v2`,
      {},
      {
        // The original bundled candidate is larger than 1MiB. Other owned modules
        // retain their smaller bound; the shared reader still defaults to 64KiB.
        contentIdentityBytes: (name === plan.binding.candidateWorker ? 4 : 1) * 1024 * 1024,
      },
    );
  const fence = async () => {
    check();
    await verifyAuthority(authorityCF);
    const settings = await authorityCF.settings(authorityName);
    const bindings = settings.bindings.filter(
      (b) => b.name === "CRABBOX_AWS_QUALIFICATION_RECOVERY_JSON",
    );
    if (
      bindings.length !== 1 ||
      bindings[0].type !== "plain_text" ||
      bindings[0].text !== canonical(plan.binding) ||
      !same(await moduleIdentity(authorityCF, authorityName), plan.authorityModule) ||
      (await authorityCF.latestVersion(authorityName)) !== corrected.authority.version
    )
      fail();
  };
  const inspect = async (name) => {
    const expected = plan.callers.find((c) => c.name === name);
    if (!expected) fail();
    const settings = await verifyOwned(callerCF, name, { allowAbsent: true });
    if (!settings) return undefined;
    // Absence is a fail-closed observation, never authority to adopt a later
    // Worker with this name. The original controller is always required.
    if (expected.absent) fail();
    const version = await callerCF.latestVersion(name);
    const module = await moduleIdentity(callerCF, name);
    const intent = state.uploadIntents[name];
    const originalMatches =
      version === expected.version &&
      same(module, expected.module) &&
      digest(
        canonical({
          bindings: bindingShape(settings.bindings),
          tags: [...(settings.tags ?? [])].sort(),
        }),
      ) === expected.settingsSha256;
    if (
      !originalMatches &&
      !(
        intent &&
        name === plan.binding.candidateWorker &&
        same(module, { module: "tombstone.mjs", count: 1, sha256: digest(tombstone) }) &&
        same(bindingShape(settings.bindings), bindingShape(intent.bindings)) &&
        same([...(settings.tags ?? [])].sort(), [...intent.tags].sort())
      )
    )
      fail();
    if ((await callerCF.latestVersion(name)) !== version) fail();
    return settings;
  };
  callerCF.request = async (route, init = {}, options) => {
    if ((init.method ?? "GET") !== "GET") {
      check();
      await fence();
      const match = /^\/workers\/scripts\/([^/?]+)(\/subdomain)?$/.exec(route);
      if (!match || !plan.callers.some((c) => c.name === match[1])) fail();
      const name = match[1],
        method = init.method;
      const existing = await inspect(name);
      if (method === "PUT" && !match[2]) {
        if (!existing || name !== plan.binding.candidateWorker || !(init.body instanceof FormData))
          fail();
        const entries = [...init.body.entries()];
        const meta = JSON.parse(await init.body.get("metadata").text());
        const module = init.body.get("tombstone.mjs");
        const expectedBindings = existing.bindings.filter((b) =>
          [
            "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
            "QUALIFICATION_TRANSPORT_PROFILE",
            "QUALIFICATION_TRANSPORT_IDENTITY",
          ].includes(b.name),
        );
        const retained = bindingShape(existing.bindings.filter((b) => b.type === "secret_text"));
        if (
          entries.length !== 2 ||
          typeof module === "string" ||
          !module ||
          digest(Buffer.from(await module.arrayBuffer())) !== digest(tombstone) ||
          !same(
            { ...meta, tags: [...(meta.tags ?? [])].sort() },
            {
              main_module: "tombstone.mjs",
              compatibility_date: "2026-09-03",
              keep_bindings: ["secret_text"],
              bindings: expectedBindings,
              migrations: {
                old_tag: "qualification-v1",
                new_tag: "qualification-delete",
                deleted_classes: ["FleetDurableObject"],
              },
              tags: ownershipTags(callerCF, name, [...expectedBindings, ...retained]).sort(),
            },
          )
        )
          fail();
        state.uploadIntents[name] = { bindings: [...meta.bindings, ...retained], tags: meta.tags };
        await persist(); // arm before effect, including an uncertain upload response
      } else if (method === "POST" && match[2]) {
        const value = JSON.parse(init.body);
        if (!same(value, { enabled: name === controllerName, previews_enabled: false })) fail();
      } else if (method !== "DELETE" || match[2]) fail();
    }
    return await originalRequest(route, init, options);
  };
  // Ordinary deletion only. The existing owner still checks all references,
  // exact ownership and namespace absence; no dependency-breaking force query.
  callerCF.deleteScript = async (name) => {
    await callerCF.request(`/workers/scripts/${name}`, { method: "DELETE" }, { allow404: true });
    if (
      (await callerCF.request(`/workers/scripts/${name}/settings`, {}, { allow404: true })) !==
      undefined
    )
      fail();
    if (name === controllerName) callerCF.controllerVersion = undefined;
  };
  const verifyAbsentCallers = async () => {
    await fence();
    for (const caller of plan.callers) if (await inspect(caller.name)) fail();
  };
  await fence();
  for (const caller of plan.callers) await inspect(caller.name);
  const recovery = {
    callerCF,
    authorityCF,
    token,
    state,
    verifyAbsentCallers,
    checkpoint: async (phase, attestation) => {
      check();
      verifyAttestationIdentity(attestation, plan.binding, { finalized: true });
      if (
        !same(attestation.finalReceipt?.finalCounts, {
          images: 0,
          instances: 0,
          keyPairs: 0,
          snapshots: 0,
          volumes: 0,
        }) ||
        !same(attestation.finalReceipt?.failureCodes, [])
      )
        fail();
      state.attestation = attestation;
      if (phase === "retired") state.retired = true;
      await persist();
    },
  };
  if (state.settled) {
    await verifyAbsentCallers();
    return state;
  }
  const result = await run(recovery, plan.binding);
  if (result.result !== "finalized") throw new Error("original owner recovery remains incomplete");
  await verifyAbsentCallers();
  if (!state.retired) fail();
  state.settled = true;
  await persist();
  return state;
}

export async function recoveryCommand(args, env = process.env) {
  if (args.length !== 1) fail();
  const directory = path.resolve(args[0]),
    plan = readPrivateJSON(path.join(directory, "recovery-plan.json"));
  const { corrected } = validateRecoveryPlan(plan);
  const originalReceipt = readPrivateJSON(
    path.join(directory, "original-attestation.json"),
    plan.originalAttestationSha256,
  );
  verifyAttestationIdentity(originalReceipt.attestation ?? originalReceipt, plan.binding);
  const handoff = readPrivateJSON(
    path.join(directory, "original-handoff.json"),
    plan.originalHandoffSha256,
  );
  if (
    handoff.startsAt !== plan.handoffStartedAt ||
    handoff.mode !== "first-walk" ||
    [
      "runId",
      "candidateSha",
      "candidateWorker",
      "authoritySha",
      "authorityVersion",
      "policyHash",
      "expiresAt",
    ].some((key) => handoff[key] !== plan.binding[key])
  )
    fail();
  const source = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
  if (
    source !== plan.sourceSha ||
    env.QUALIFICATION_RECOVERY_PLAN_SHA256 !== digest(canonical(plan)) ||
    env.CLOUDFLARE_ACCOUNT_ID !== corrected.accountId ||
    !env.CLOUDFLARE_API_TOKEN ||
    !env.QUALIFICATION_CONTROLLER_TOKEN ||
    execFileSync("git", ["status", "--porcelain", "--untracked-files=all"], {
      cwd: root,
      encoding: "utf8",
    }).trim()
  )
    fail();
  const abort = new AbortController(),
    cancel = () => abort.abort();
  process.on("SIGINT", cancel);
  process.on("SIGTERM", cancel);
  try {
    return await withQualificationJournal(
      directory,
      {
        stateName: "recovery-state.json",
        lockName: "recovery.lock",
        create: true,
        initial: () => initialRecoveryState(plan),
      },
      (state, save) =>
        recoverTransition(plan, state, {
          callerCF: new Cloudflare(corrected.accountId, env.CLOUDFLARE_API_TOKEN),
          authorityCF: new Cloudflare(corrected.accountId, env.CLOUDFLARE_API_TOKEN),
          token: env.QUALIFICATION_CONTROLLER_TOKEN,
          save,
          signal: abort.signal,
        }),
    );
  } finally {
    process.removeListener("SIGINT", cancel);
    process.removeListener("SIGTERM", cancel);
  }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url))
  recoveryCommand(process.argv.slice(2)).then(
    () => console.log("Original qualification owner settled. No qualification was started."),
    () => {
      console.error(
        "Recovery stopped; retain original access and journal. No raw credential or diagnostic was printed.",
      );
      process.exitCode = 1;
    },
  );
