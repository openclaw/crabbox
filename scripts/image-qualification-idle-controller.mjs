// One idle-only profile at the existing qualification owner. Never a generic
// props bypass, candidate verifier, registry owner or authority deployment path.
import fs from "node:fs";
import path from "node:path";
import { randomBytes } from "node:crypto";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  authorityName,
  controllerName,
  canonical,
  digest,
  readAdmission,
  verifyAuthority,
} from "./image-qualification-isolation.mjs";
import { readProbeResponse } from "./image-qualification-binding-probe.mjs";
import { readPrivateJSON, withQualificationJournal } from "./image-qualification-journal.mjs";

const directory = path.dirname(fileURLToPath(import.meta.url));
const sourceFiles = [
  "image-qualification-control.mjs",
  "image-qualification-isolation.mjs",
  "image-qualification-controller-worker.mjs",
  "image-qualification-binding-probe.mjs",
  "image-qualification-journal.mjs",
  "image-qualification-idle-controller.mjs",
];
const equal = (a, b) => canonical(a) === canonical(b);
const sha = /^[0-9a-f]{64}$/;
const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const fail = (reason) => {
  throw new Error(`qualification idle controller: ${reason}`);
};
const script = `/workers/scripts/${controllerName}`;
const shape = (value) => Object.keys(value ?? {}).sort();
export const idleSourceIdentity = () =>
  Object.fromEntries(
    sourceFiles.map((file) => [file, digest(fs.readFileSync(path.join(directory, file)))]),
  );
const stateHash = ({ integrity, ...state }) => digest(canonical(state));
const seal = (state) => ({ ...state, integrity: stateHash(state) });
export const idleInitialState = (plan) =>
  seal({
    profile: "idle-controller/v1",
    planHash: digest(canonical(plan)),
    sequence: 0,
    phase: "prepared",
    events: [],
    requests: 0,
    settled: false,
  });

export function validateIdlePlan(plan) {
  if (
    !plan ||
    plan.profile !== "idle-controller/v1" ||
    !/^[0-9a-f]{24}$/.test(plan.attemptId ?? "") ||
    !/^[0-9a-f]{40}$/.test(plan.sourceSha ?? "") ||
    !/^[0-9a-f]{32}$/.test(plan.tokenId ?? "") ||
    !/^[0-9a-f]{32}$/.test(plan.authorId ?? "") ||
    !equal(plan.sources, idleSourceIdentity()) ||
    !sha.test(plan.originalInventorySha256 ?? "") ||
    !sha.test(plan.authoritySettingsSha256 ?? "") ||
    !sha.test(plan.custodyReceiptSha256 ?? "") ||
    !sha.test(plan.authorityModule?.sha256 ?? "") ||
    !/^[a-zA-Z0-9_.-]+$/.test(plan.authorityModule?.module ?? "") ||
    plan.authorityModule?.count !== 1 ||
    plan.defaultEnvironment !== "production" ||
    !sha.test(plan.props?.deploymentHash ?? "") ||
    !equal(shape(plan.props), ["deploymentHash"]) ||
    !plan.owner ||
    !plan.fallback ||
    !Array.isArray(plan.uploadExposure) ||
    !plan.uploadExposure.length ||
    plan.uploadExposure.length > 4 ||
    plan.uploadExposure.some(
      (v) =>
        !equal(shape(v), ["enabled", "previews_enabled"]) ||
        typeof v.enabled !== "boolean" ||
        typeof v.previews_enabled !== "boolean",
    ) ||
    plan.maxRequests !== 800 ||
    !Number.isFinite(Date.parse(plan.startsAt)) ||
    Date.parse(plan.forwardStopsAt) !== Date.parse(plan.startsAt) + 10 * 60_000 ||
    Date.parse(plan.deadline) !== Date.parse(plan.startsAt) + 20 * 60_000
  )
    fail("invalid frozen plan/source/custody");
  // Existing full isolation admission still owns the authority and its namespaces.
  return readAdmission({
    CLOUDFLARE_ACCOUNT_ID: plan.isolation?.accountId,
    QUALIFICATION_ISOLATION_JSON: JSON.stringify(plan.isolation),
    QUALIFICATION_ISOLATION_SHA256: digest(canonical(plan.isolation)),
  });
}

function requestedBindings(plan) {
  return [
    {
      name: "AUTHORITY",
      type: "service",
      service: authorityName,
      entrypoint: "AWSQualificationController",
      props: plan.props,
    },
    { name: "CF_VERSION_METADATA", type: "version_metadata" },
    { name: "CONTROLLER_TOKEN", type: "secret_text" },
    { name: "IDLE_CONTROLLER_ONLY", type: "plain_text", text: "1" },
  ];
}
export function idleBindingProjection(plan, bindings) {
  if (!Array.isArray(bindings)) fail("missing bindings");
  const projected = bindings
    .map((binding) => {
      if (binding.name !== "AUTHORITY") return binding;
      const { props, environment, ...visible } = binding;
      if (
        (props !== undefined && !equal(props, plan.props)) ||
        (environment !== undefined && environment !== plan.defaultEnvironment)
      )
        fail("authority binding changed");
      return visible;
    })
    .sort((a, b) => a.name?.localeCompare(b.name));
  const expected = requestedBindings(plan)
    .map(({ props, ...binding }) => binding)
    .sort((a, b) => a.name.localeCompare(b.name));
  if (!equal(projected, expected)) fail("unexpected visible binding capability");
  return projected;
}
function tags(plan) {
  return [
    `qualification-idle-v1:${digest(canonical(plan))}`,
    `qualification-idle-request:${digest(canonical(requestedBindings(plan)))}`,
  ];
}
function refers(value) {
  return (
    value &&
    typeof value === "object" &&
    Object.entries(value).some(
      ([key, child]) =>
        (["script", "script_name", "service"].includes(key) && child === controllerName) ||
        refers(child),
    )
  );
}

// save must resolve only after durable commit. State records effects, never tokens.
export async function idleController(
  cf,
  plan,
  state,
  { mode = "run", save, token, signal, now = Date.now, invoke = fetch } = {},
) {
  const admission = validateIdlePlan(plan);
  const allowedPhases = [
    "prepared",
    "create-intent",
    "owned",
    "normalize-intent",
    "private",
    "public-intent",
    "public",
    "identity-pending",
    "identity-verified",
    "discovery-pending",
    "discovery-complete",
    "delete-intent",
    "delete-result",
    "settlement-pending",
    "settled",
  ];
  if (
    !["run", "recover"].includes(mode) ||
    typeof save !== "function" ||
    cf.accountId !== admission.accountId ||
    state.profile !== plan.profile ||
    state.planHash !== digest(canonical(plan)) ||
    !allowedPhases.includes(state.phase) ||
    !Number.isSafeInteger(state.sequence) ||
    state.sequence < 0 ||
    !Array.isArray(state.events) ||
    state.events.length > state.sequence ||
    state.sequence > 2000 ||
    state.integrity !== stateHash(state) ||
    !Number.isSafeInteger(state.requests) ||
    state.requests < 0 ||
    state.requests > plan.maxRequests ||
    (state.owned && (!uuid.test(state.versionId ?? "") || !state.createAt)) ||
    (mode === "run" && (state.sequence !== 0 || state.phase !== "prepared")) ||
    (mode === "run" && (typeof token !== "string" || token.length !== 64))
  )
    fail("invalid original idle receipt/context");
  let cleaning = mode === "recover";
  let requests = state.requests;
  let writes = Promise.resolve();
  const persist = (phase, delta = {}, event = true) => {
    const operation = writes.then(async () => {
      const next = seal({
        ...structuredClone(state),
        ...delta,
        phase,
        requests,
        sequence: state.sequence + 1,
        events: event
          ? [...state.events, { phase, at: new Date(now()).toISOString() }]
          : state.events,
      });
      if (next.sequence > 2000) fail("journal transition bound exhausted");
      await save(next);
      Object.assign(state, next);
    });
    writes = operation.catch(() => {});
    return operation;
  };
  const limit = () => {
    const end = Date.parse(cleaning ? plan.deadline : plan.forwardStopsAt);
    if (now() < Date.parse(plan.startsAt) || now() >= end || (!cleaning && signal?.aborted))
      fail("clock expired or cancelled");
    if (++requests > (cleaning ? plan.maxRequests : plan.maxRequests / 2))
      fail("request budget exhausted");
    return AbortSignal.timeout(Math.max(1, Math.min(15_000, end - now())));
  };
  const api = Object.create(cf);
  api.admission = admission;
  api.request = async (route, init = {}, options = {}) => {
    const timeout = limit();
    await persist(state.phase, {}, false);
    return cf.request(
      route,
      {
        ...init,
        redirect: "error",
        signal: cleaning || !signal ? timeout : AbortSignal.any([timeout, signal]),
      },
      { boundedBytes: 1024 * 1024, ...options },
    );
  };
  api.list = async (route) => {
    const all = [];
    for (let page = 1; page <= 100; page++) {
      const result = await api.request(
        `${route}${route.includes("?") ? "&" : "?"}page=${page}&per_page=100`,
        {},
        { envelope: true },
      );
      const items = result.result?.items ?? result.result;
      const pages = result.result_info?.total_pages;
      if (
        !Array.isArray(items) ||
        (pages !== undefined && (!Number.isSafeInteger(pages) || pages < page || pages > 100))
      )
        fail("incomplete inventory");
      all.push(...items);
      if (pages !== undefined ? page === pages : items.length < 100) return all;
    }
    fail("inventory exceeded bound");
  };
  const authority = async () => {
    await verifyAuthority(api);
    if (
      digest(canonical(await api.settings(authorityName))) !== plan.authoritySettingsSha256 ||
      !equal(
        await api.request(
          `/workers/scripts/${authorityName}/content/v2`,
          {},
          { contentIdentityBytes: 1024 * 1024 },
        ),
        plan.authorityModule,
      ) ||
      (await api.request(`/workers/services/${authorityName}`))?.default_environment
        ?.environment !== plan.defaultEnvironment ||
      (await api.latestVersion(authorityName)) !== admission.authority.version
    )
      fail("authority/default environment drift");
  };
  const inventory = async () => {
    const scripts = await api.list("/workers/scripts");
    const names = scripts.map((v) => v.id ?? v.name);
    if (
      names.some((v) => !/^[a-zA-Z0-9_-]{1,63}$/.test(v ?? "")) ||
      new Set(names).size !== names.length
    )
      fail("invalid Worker inventory");
    const namespaces = await api.list("/workers/durable_objects/namespaces");
    const domains = await api.list("/workers/domains");
    if (
      [...namespaces, ...domains].some(
        (v) => v.script === controllerName || v.service === controllerName,
      )
    )
      fail("caller acquired namespace/domain");
    const incoming = [];
    for (const name of names) if (refers(await api.settings(name))) incoming.push(name);
    if (incoming.length) fail("incoming caller reference");
    return { scripts: names.sort(), namespaceIds: namespaces.map((v) => v.id).sort(), incoming };
  };
  const inspect = async () => {
    if (!state.createAt) fail("no durable create intent");
    const settings = await api.request(`${script}/settings`, {}, { allow404: true });
    if (settings === undefined) return undefined;
    if (
      settings.compatibility_date !== "2026-09-03" ||
      !equal(settings.compatibility_flags ?? [], []) ||
      !equal([...(settings.tags ?? [])].sort(), tags(plan).sort()) ||
      !equal(settings.tail_consumers ?? [], [])
    )
      fail("controller settings changed");
    const bindings = idleBindingProjection(plan, settings.bindings);
    const versionId = await api.latestVersion(controllerName);
    const version = await api.request(`${script}/versions/${versionId}`);
    const versions = await api.list(`${script}/versions`);
    if (
      !equal(
        versions.map((v) => v.id),
        [versionId],
      ) ||
      version.id !== versionId ||
      version.number !== 1 ||
      version.metadata?.author_id !== plan.authorId ||
      !Number.isFinite(Date.parse(version.metadata?.created_on)) ||
      Date.parse(version.metadata.created_on) < Date.parse(state.createAt) ||
      Date.parse(version.metadata.created_on) >= Date.parse(plan.forwardStopsAt) ||
      (state.versionId && state.versionId !== versionId)
    )
      fail("controller creation/version changed");
    if (
      !equal(idleBindingProjection(plan, version.resources?.bindings), bindings) ||
      !equal(await api.request(`${script}/content/v2`, {}, { contentIdentityBytes: 64 * 1024 }), {
        module: "controller.mjs",
        count: 1,
        sha256: plan.sources["image-qualification-controller-worker.mjs"],
      })
    )
      fail("controller module/bindings changed");
    const environment = (await api.request(`/workers/services/${controllerName}`))
      ?.default_environment?.environment;
    if (
      environment !== plan.defaultEnvironment ||
      !equal(
        await api.request(
          `/workers/services/${controllerName}/environments/${environment}/routes?show_zonename=true`,
        ),
        [],
      ) ||
      !equal((await api.request(`${script}/schedules`))?.schedules, [])
    )
      fail("controller routes changed");
    const exposure = await api.request(`${script}/subdomain`);
    const accepted =
      state.exposure === "public"
        ? [{ enabled: true, previews_enabled: false }]
        : state.exposure === "public-intent"
          ? [
              { enabled: false, previews_enabled: false },
              { enabled: true, previews_enabled: false },
            ]
          : state.exposure === "private"
            ? [{ enabled: false, previews_enabled: false }]
            : [...plan.uploadExposure, { enabled: false, previews_enabled: false }];
    if (
      !accepted.some((v) => equal(v, exposure)) ||
      (await api.latestVersion(controllerName)) !== versionId
    )
      fail("controller exposure/version changed");
    await persist(state.phase, {
      owned: true,
      versionId,
      visibleBindingDigest: digest(canonical(bindings)),
    });
    return versionId;
  };
  const fence = async () => {
    await authority();
    const version = await inspect();
    if (!version) fail("owned caller disappeared");
    return version;
  };
  const call = async (action, input) => {
    const timeout = limit();
    await persist(state.phase, {}, false);
    const response = await invoke(`${state.url}/${action}`, {
      method: "POST",
      redirect: "error",
      signal: signal ? AbortSignal.any([timeout, signal]) : timeout,
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: JSON.stringify(input),
    });
    if (!response.ok) fail("idle runtime request rejected");
    return JSON.parse(await readProbeResponse(response, 4096));
  };
  const credential = async () => {
    const value = await api.request("/tokens/verify");
    if (
      value.id !== plan.tokenId ||
      value.status !== "active" ||
      !Number.isFinite(Date.parse(value.expires_on)) ||
      Date.parse(value.expires_on) < Date.parse(plan.deadline) + 5 * 60_000
    )
      fail("credential identity/lifetime changed");
  };
  let failed = false;
  try {
    // A recovery invocation can only inspect/delete its exact journaled caller.
    if (mode === "run") {
      await authority();
      const before = await inventory();
      if (
        before.scripts.includes(controllerName) ||
        digest(canonical(before)) !== plan.originalInventorySha256 ||
        (await api.request(`${script}/settings`, {}, { allow404: true })) !== undefined
      )
        fail("original absence/inventory changed");
      await credential();
      await persist("create-intent", { createAt: new Date(now()).toISOString() });
      const form = new FormData();
      form.set(
        "metadata",
        new Blob(
          [
            JSON.stringify({
              main_module: "controller.mjs",
              compatibility_date: "2026-09-03",
              bindings: requestedBindings(plan).map((v) =>
                v.type === "secret_text" ? { ...v, text: token } : v,
              ),
              tags: tags(plan),
            }),
          ],
          { type: "application/json" },
        ),
      );
      form.set(
        "controller.mjs",
        new Blob(
          [fs.readFileSync(path.join(directory, "image-qualification-controller-worker.mjs"))],
          { type: "application/javascript+module" },
        ),
        "controller.mjs",
      );
      await api.request(script, { method: "PUT", body: form });
      await inspect();
      await inventory();
      await persist("owned");
      for (const [phase, enabled, outcome] of [
        ["normalize-intent", false, "private"],
        ["public-intent", true, "public"],
      ]) {
        await persist(phase, { exposure: phase });
        await api.request(`${script}/subdomain`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ enabled, previews_enabled: false }),
        });
        await persist(outcome, { exposure: outcome });
        await fence();
      }
      const subdomain = (await api.request("/workers/subdomain"))?.subdomain;
      if (!/^[a-z0-9-]+$/.test(subdomain ?? "")) fail("account subdomain missing");
      await persist("identity-pending", {
        url: `https://${controllerName}.${subdomain}.workers.dev`,
        challenge: randomBytes(32).toString("hex"),
      });
      const beforeVersion = await fence();
      const reply = await call("binding-identity", { challenge: state.challenge });
      if (
        !equal(reply, {
          controllerVersion: beforeVersion,
          identity: {
            protocol: 1,
            role: "controller",
            challenge: state.challenge,
            props: plan.props,
            authorityVersion: admission.authority.version,
          },
        })
      )
        fail("actual runtime binding identity mismatch");
      await fence();
      await persist("identity-verified", { identityDigest: digest(canonical(reply)) });
      await persist("discovery-pending");
      await fence();
      const discovered = await call("discover", {});
      await fence();
      await persist("discovery-complete", {
        discovery: equal(discovered, { run: null }) ? "idle" : "active-or-unknown",
      });
      if (state.discovery !== "idle") fail("authority registry is active or unknown");
    }
  } catch {
    failed = true;
  } finally {
    cleaning = true;
    // Preserve intent facts; never infer not-created from a missing/corrupt receipt.
    await persist("settlement-pending", { settled: false, failed: state.failed || failed });
    await credential();
    await authority();
    if (state.createAt) {
      const present = await inspect();
      await inventory();
      if (present) {
        await persist("delete-intent", { deleteVersion: present });
        try {
          await api.request(script, { method: "DELETE" }, { allow404: true });
          await persist("delete-result", { deleteResponse: "received" });
        } catch {
          await persist("delete-result", { deleteResponse: "unknown" });
        }
      }
      if ((await api.request(`${script}/settings`, {}, { allow404: true })) !== undefined)
        fail("caller deletion unconfirmed");
      const after = await inventory();
      if (
        after.scripts.includes(controllerName) ||
        !state.owned ||
        digest(canonical(after)) !== plan.originalInventorySha256
      )
        fail("unobserved create or incomplete settlement");
    }
    await authority();
    await persist("settled", { settled: true });
  }
  return state;
}

export async function idleControllerCommand(command, args, Cloudflare, env = process.env) {
  if (args.length !== 1 || !["idle-controller-run", "idle-controller-recover"].includes(command))
    fail("requires original private recovery directory");
  const root = path.dirname(directory);
  const sourceSha = execFileSync("git", ["rev-parse", "HEAD"], {
    cwd: root,
    encoding: "utf8",
  }).trim();
  if (
    execFileSync("git", ["status", "--porcelain", "--untracked-files=all"], {
      cwd: root,
      encoding: "utf8",
    }).trim() ||
    env.QUALIFICATION_IDLE_SOURCE_SHA !== sourceSha
  )
    fail("clean admitted protected source required");
  const recoveryDirectory = path.resolve(args[0]);
  const planFile = path.join(recoveryDirectory, "idle-controller-plan.json");
  const plan = readPrivateJSON(planFile);
  const admission = validateIdlePlan(plan);
  if (
    plan.sourceSha !== sourceSha ||
    env.QUALIFICATION_IDLE_PLAN_SHA256 !== digest(canonical(plan)) ||
    env.CLOUDFLARE_ACCOUNT_ID !== admission.accountId ||
    !env.CLOUDFLARE_API_TOKEN
  )
    fail("source/plan/credential custody changed");
  // Prove retained custody is accessible from a fresh credential-free process.
  const witness = execFileSync(
    process.execPath,
    [
      "--input-type=module",
      "-e",
      'import {readFileSync} from "node:fs"; import {createHash} from "node:crypto"; process.stdout.write(createHash("sha256").update(readFileSync(process.argv[1])).digest("hex"))',
      planFile,
    ],
    { encoding: "utf8", env: {} },
  );
  if (witness !== digest(fs.readFileSync(planFile)))
    fail("independent retained custody unavailable");
  const cancellation = new AbortController();
  const stop = () => cancellation.abort();
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
  try {
    const state = await withQualificationJournal(
      recoveryDirectory,
      {
        stateName: "idle-controller-state.json",
        lockName: "idle-controller.lock",
        create: command === "idle-controller-run",
        initial: () => idleInitialState(plan),
      },
      (value, save) =>
        idleController(new Cloudflare(admission.accountId, env.CLOUDFLARE_API_TOKEN), plan, value, {
          mode: command === "idle-controller-run" ? "run" : "recover",
          token: randomBytes(32).toString("hex"),
          save,
          signal: cancellation.signal,
        }),
    );
    if (
      !state.settled ||
      (command === "idle-controller-run" && (state.failed || state.discovery !== "idle"))
    )
      fail("idle observation failed; inspect original recovery receipt");
  } finally {
    process.removeListener("SIGINT", stop);
    process.removeListener("SIGTERM", stop);
  }
}
