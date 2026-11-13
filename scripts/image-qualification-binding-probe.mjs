// Stateless, source-pinned provider characterization within the protected
// qualification helper. This is deliberately separate from run/authority
// ownership: missing props never become acceptable for privileged bindings.
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { randomBytes } from "node:crypto";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { canonical, digest, bindingShape } from "./image-qualification-isolation.mjs";

const directory = path.dirname(fileURLToPath(import.meta.url));
const roles = ["receiver", "caller"];
const cycleMs = 20 * 60_000;
const forwardMs = 10 * 60_000;
const date = "2026-09-03";
const fail = (reason) => {
  throw new Error(`qualification binding probe: ${reason}`);
};
const equal = (a, b) => canonical(a) === canonical(b);
const moduleName = (role) => `image-qualification-binding-${role}.mjs`;
const moduleBytes = (role) => fs.readFileSync(path.join(directory, moduleName(role)));
const nameFor = (id, role) => `crabbox-binding-probe-${id}-${role}`;

async function responseBytes(response, maximum) {
  const reader = response.body?.getReader();
  if (!reader) fail("response body missing");
  const chunks = [];
  let length = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.length;
      if (length > maximum) fail("response body exceeds bound");
      chunks.push(value);
    }
  } finally {
    await reader.cancel();
  }
  return Buffer.concat(chunks);
}

export async function readProbeResponse(response, maximum = 8192) {
  return (await responseBytes(response, maximum)).toString("utf8");
}

export async function probeContentIdentity(response) {
  const body = await responseBytes(response, 64 * 1024);
  const form = await new Response(body, {
    headers: { "content-type": response.headers.get("content-type") },
  }).formData();
  const entries = [...form.entries()];
  if (entries.length !== 1 || typeof entries[0][1] === "string")
    fail("unexpected module inventory");
  return {
    module: entries[0][0],
    count: 1,
    sha256: digest(Buffer.from(await entries[0][1].arrayBuffer())),
  };
}

export function prepareBindingProbe({ accountId, tokenId, sourceSha, id, startsAt }) {
  const start = Date.parse(startsAt);
  const plan = {
    version: 1,
    accountId,
    tokenId,
    sourceSha,
    id,
    startsAt,
    forwardStopsAt: new Date(start + forwardMs).toISOString(),
    deadline: new Date(start + cycleMs).toISOString(),
    workers: Object.fromEntries(
      roles.map((role) => [
        role,
        { name: nameFor(id, role), moduleSha256: digest(moduleBytes(role)) },
      ]),
    ),
    props: {
      deploymentHash: digest(id),
      identity: { purpose: "service-binding-contract", nonce: id },
    },
    maxRequests: 800,
    monitoredUSD: 1,
  };
  validatePlan(plan);
  return plan;
}

function validatePlan(plan) {
  if (
    plan.version !== 1 ||
    !/^[0-9a-f]{32}$/.test(plan.accountId ?? "") ||
    !/^[0-9a-f]{32}$/.test(plan.tokenId ?? "") ||
    !/^[0-9a-f]{40}$/.test(plan.sourceSha ?? "") ||
    !/^[0-9a-f]{12}$/.test(plan.id ?? "") ||
    !Number.isFinite(Date.parse(plan.startsAt)) ||
    Date.parse(plan.forwardStopsAt) !== Date.parse(plan.startsAt) + forwardMs ||
    Date.parse(plan.deadline) !== Date.parse(plan.startsAt) + cycleMs ||
    plan.maxRequests !== 800 ||
    plan.monitoredUSD !== 1 ||
    !equal(plan.props, {
      deploymentHash: digest(plan.id),
      identity: { purpose: "service-binding-contract", nonce: plan.id },
    }) ||
    !equal(Object.keys(plan.workers ?? {}).sort(), [...roles].sort()) ||
    roles.some(
      (role) =>
        plan.workers[role].name !== nameFor(plan.id, role) ||
        plan.workers[role].moduleSha256 !== digest(moduleBytes(role)),
    )
  )
    fail("invalid or changed frozen plan/source");
}

function tags(plan, role) {
  return [
    `binding-probe:${plan.id}`,
    `binding-probe-plan:${digest(canonical(plan))}`,
    `binding-probe-role:${role}`,
  ];
}

function metadata(plan, role, token) {
  return {
    main_module: moduleName(role),
    compatibility_date: date,
    bindings:
      role === "receiver"
        ? []
        : [
            { name: "PROBE_TOKEN", type: "secret_text", text: token },
            {
              name: "RECEIVER",
              type: "service",
              service: plan.workers.receiver.name,
              entrypoint: "BindingProbe",
              props: plan.props,
            },
          ],
    tags: tags(plan, role),
  };
}

function checkBindings(plan, role, bindings, receiverEnvironment) {
  if (!Array.isArray(bindings)) fail("missing binding inventory");
  if (role === "receiver") {
    if (bindings.length) fail("receiver acquired a resource binding");
    return;
  }
  const token = bindings.find((b) => b.name === "PROBE_TOKEN");
  const service = bindings.find((b) => b.name === "RECEIVER");
  if (
    bindings.length !== 2 ||
    !equal(token, { name: "PROBE_TOKEN", type: "secret_text" }) ||
    service?.type !== "service" ||
    service.service !== plan.workers.receiver.name ||
    service.entrypoint !== "BindingProbe" ||
    (service.environment !== undefined && service.environment !== receiverEnvironment) ||
    Object.keys(service).some(
      (key) => !["name", "type", "service", "entrypoint", "environment", "props"].includes(key),
    ) ||
    JSON.stringify(service.props ?? null).length > 4096
  )
    fail("caller acquired an unexpected capability");
  // Props are public experimental data ONLY for this unprivileged exact
  // receiver. No authority/controller/candidate verification uses this rule.
}

function references(value, target) {
  if (!value || typeof value !== "object") return false;
  return Object.entries(value).some(
    ([key, child]) =>
      (["service", "script", "script_name"].includes(key) && child === target) ||
      references(child, target),
  );
}

// save() must durably commit before resolving. A lost upload response never
// authorizes a retry or an absence-based settlement of an uncertain intent.
export async function bindingProbe(
  cf,
  plan,
  ledger,
  { mode = "run", token, save, signal, now = Date.now, invoke = fetch } = {},
) {
  validatePlan(plan);
  if (
    !["run", "recover"].includes(mode) ||
    cf.accountId !== plan.accountId ||
    typeof save !== "function"
  )
    fail("invalid execution context");
  const planHash = digest(canonical(plan));
  if (
    ledger.planHash !== planHash ||
    !equal(
      Object.keys(ledger.intents ?? {}).sort(),
      Object.keys(ledger.intents ?? {})
        .filter((key) => roles.includes(key))
        .sort(),
    )
  )
    fail("journal does not match plan");
  const started = now();
  // Recovery has its own bounded command window; it never changes the original
  // cycle deadline or admits forward work after expiry.
  const recoveryEnds =
    Math.max(Date.parse(plan.deadline), started) + (mode === "recover" ? 5 * 60_000 : 0);
  let cleaning = mode === "recover";
  let requests = 0;
  const checkClock = () => {
    const limit = cleaning ? recoveryEnds : Date.parse(plan.forwardStopsAt);
    if (now() < Date.parse(plan.startsAt) || now() >= limit || (!cleaning && signal?.aborted))
      fail("clock expired or cancelled");
    if (++requests > (cleaning ? plan.maxRequests : plan.maxRequests / 2))
      fail("request bound exceeded; preserve recovery journal");
    return Math.max(1, Math.min(15_000, limit - now()));
  };
  const transport = cf.request.bind(cf);
  const api = Object.create(cf);
  api.request = (route, init = {}, options = {}) => {
    const timeout = AbortSignal.timeout(checkClock());
    ledger.lastAction = { route, method: init.method ?? "GET" };
    ledger.requestBudgetUsed = requests;
    return transport(
      route,
      {
        ...init,
        redirect: "error",
        signal: cleaning || !signal ? timeout : AbortSignal.any([signal, timeout]),
      },
      options,
    );
  };
  const persist = async () => {
    await save(ledger);
  };
  const settingsRoute = (role) => `/workers/scripts/${plan.workers[role].name}/settings`;
  const scriptRoute = (role) => `/workers/scripts/${plan.workers[role].name}`;
  const list = async (route) => {
    const all = [];
    for (let page = 1; page <= 100; page++) {
      const response = await api.request(
        `${route}?page=${page}&per_page=100`,
        {},
        { envelope: true },
      );
      if (!Array.isArray(response.result)) fail("incomplete inventory");
      all.push(...response.result);
      const info = response.result_info;
      if (
        info?.total_pages !== undefined &&
        (!Number.isSafeInteger(info.total_pages) ||
          info.total_pages < page ||
          info.total_pages > 100)
      )
        fail("invalid inventory pagination");
      if (info?.total_pages !== undefined ? page >= info.total_pages : response.result.length < 100)
        return all;
    }
    fail("inventory pagination exceeded bound");
  };
  const inventory = async () => {
    const scripts = await list("/workers/scripts");
    if (
      !Array.isArray(scripts) ||
      scripts.some((s) => !/^[a-zA-Z0-9_-]+$/.test(s.id ?? s.name ?? ""))
    )
      fail("invalid Worker inventory");
    const namespaces = await list("/workers/durable_objects/namespaces");
    const domains = await list("/workers/domains");
    for (const resource of [...namespaces, ...domains]) {
      if (
        roles.some((role) => [resource.script, resource.service].includes(plan.workers[role].name))
      )
        fail("unexpected fixture namespace or domain");
    }
    const incoming = [];
    for (const item of scripts) {
      const name = item.id ?? item.name;
      const settings = await api.request(`/workers/scripts/${name}/settings`);
      for (const role of roles) {
        if (references(settings, plan.workers[role].name)) incoming.push({ from: name, to: role });
      }
    }
    return {
      scripts: scripts.map((s) => s.id ?? s.name).sort(),
      namespaceIds: namespaces.map((n) => n.id).sort(),
      incoming,
    };
  };
  const defaultEnvironment = async (role) => {
    const value = await api.request(`/workers/services/${plan.workers[role].name}`);
    const environment = value?.default_environment?.environment;
    if (!/^[a-zA-Z0-9_-]{1,32}$/.test(environment ?? "")) fail("default environment unverified");
    return environment;
  };
  const inspect = async (role, publicCaller = false) => {
    if (!ledger.intents[role]) fail("no durable upload intent");
    const settings = await api.request(settingsRoute(role), {}, { allow404: true });
    if (!settings) return undefined;
    if (
      settings.compatibility_date !== date ||
      !equal(settings.compatibility_flags ?? [], []) ||
      !equal([...(settings.tags ?? [])].sort(), tags(plan, role).sort()) ||
      !equal(settings.tail_consumers ?? [], [])
    )
      fail("fixture settings changed");
    const environment = await defaultEnvironment(role);
    const receiverEnvironment =
      role === "receiver" ? environment : ledger.intents.receiver?.environment;
    if (!receiverEnvironment) fail("receiver default environment unverified");
    checkBindings(plan, role, bindingShape(settings.bindings), receiverEnvironment);
    const versionId = await api.latestVersion(plan.workers[role].name);
    const version = await api.request(`${scriptRoute(role)}/versions/${versionId}`);
    const versions = await api.request(`${scriptRoute(role)}/versions`);
    if (
      !equal(
        (versions.items ?? []).map((v) => v.id),
        [versionId],
      ) ||
      version.id !== versionId ||
      version.number !== 1 ||
      version.metadata?.author_id !== plan.tokenId ||
      Date.parse(version.metadata?.created_on) < Date.parse(ledger.intents[role].at) ||
      Date.parse(version.metadata?.created_on) >= Date.parse(plan.forwardStopsAt) ||
      !Number.isFinite(Date.parse(version.metadata?.created_on)) ||
      (ledger.intents[role].versionId && ledger.intents[role].versionId !== versionId)
    )
      fail("fixture version/custody changed");
    checkBindings(plan, role, bindingShape(version.resources?.bindings), receiverEnvironment);
    const response = await api.request(
      `${scriptRoute(role)}/content/v2`,
      {},
      { probeContentIdentity: true },
    );
    // The shared transport returns JSON. Raw module verification is supplied by
    // its bounded content reader; it never exposes credentials in receipts.
    if (
      !response ||
      response.module !== moduleName(role) ||
      response.sha256 !== plan.workers[role].moduleSha256 ||
      response.count !== 1
    )
      fail("fixture module changed");
    const exposure = await api.request(`${scriptRoute(role)}/subdomain`);
    const routes = await api.request(
      `/workers/services/${plan.workers[role].name}/environments/${environment}/routes?show_zonename=true`,
    );
    const schedules = await api.request(`${scriptRoute(role)}/schedules`);
    const expectedPublic = role === "caller" && publicCaller;
    const pendingExposure =
      cleaning &&
      role === "caller" &&
      ledger.callerPublic === true &&
      typeof exposure?.enabled === "boolean";
    // Upload can succeed before its response/private-default normalization is
    // observed. These exact stateless modules remain harmless under either
    // provider default; recovery must first prove source/version/capabilities.
    const uploadExposure =
      ledger.intents[role].exposurePending === true &&
      typeof exposure?.enabled === "boolean" &&
      typeof exposure?.previews_enabled === "boolean";
    if (
      (!uploadExposure &&
        ((!pendingExposure && exposure?.enabled !== expectedPublic) ||
          exposure?.previews_enabled !== false)) ||
      !equal(routes, []) ||
      !equal(schedules?.schedules, [])
    )
      fail("fixture exposure changed");
    if ((await api.latestVersion(plan.workers[role].name)) !== versionId)
      fail("fixture version changed during readback");
    ledger.intents[role].versionId = versionId;
    ledger.intents[role].environment = environment;
    ledger.intents[role].observedOwned = true;
    await persist();
    return {
      settings: { bindings: bindingShape(settings.bindings) },
      versionId,
      versionBindings: bindingShape(version.resources.bindings),
      environment,
    };
  };
  const cleanup = async () => {
    cleaning = true;
    // A new recovery attempt must not inherit success if any fresh inspection
    // fails. Commit pending state before beginning those management reads.
    ledger.settled = false;
    for (const role of roles) if (ledger.intents[role]) ledger.intents[role].settled = false;
    await persist();
    const failures = [];
    for (const role of [...roles].reverse()) {
      if (!ledger.intents[role]) continue;
      try {
        const state = await inspect(role, role === "caller" && ledger.callerPublic === true);
        const snapshot = await inventory();
        if (snapshot.incoming.some((ref) => ref.to === role))
          fail("incoming reference blocks ordinary deletion");
        if (state) {
          ledger.intents[role].deleteIntent = true;
          await persist();
          try {
            await api.request(scriptRoute(role), { method: "DELETE" }, { allow404: true });
          } catch {
            ledger.intents[role].deleteResponseUnknown = true;
          }
        }
        if ((await api.request(settingsRoute(role), {}, { allow404: true })) !== undefined)
          fail("fixture deletion not observed");
        const after = await inventory();
        if (after.scripts.includes(plan.workers[role].name)) fail("fixture remains in inventory");
        if (ledger.intents[role].uncertain && !ledger.intents[role].observedOwned)
          fail("unresolved upload intent; absence is not settlement");
        ledger.intents[role].settled = true;
        ledger.lastInventory = after;
        await persist();
      } catch {
        failures.push(role);
        ledger.cleanupFailureAt ??= {};
        ledger.cleanupFailureAt[role] = ledger.lastAction;
      }
    }
    ledger.settled =
      failures.length === 0 &&
      roles.every((role) => !ledger.intents[role] || ledger.intents[role].settled === true);
    ledger.cleanupFailures = failures;
    ledger.completedAt = new Date(now()).toISOString();
    ledger.withinOriginalBound = now() <= Date.parse(plan.deadline);
    await persist();
  };
  const credential = await api.request("/tokens/verify");
  if (
    credential?.id !== plan.tokenId ||
    credential.status !== "active" ||
    Date.parse(credential.expires_on) < recoveryEnds + 5 * 60_000 ||
    !Number.isFinite(Date.parse(credential.expires_on))
  )
    fail("credential identity or recovery lifetime unverified");
  if (mode === "recover") {
    await cleanup();
    return ledger;
  }
  if (
    ledger.startedAt ||
    Object.keys(ledger.intents).length ||
    !/^[A-Za-z0-9]{64}$/.test(token ?? "")
  )
    fail("run cannot be retried or token is invalid");
  checkClock();
  const baseline = await inventory();
  if (
    roles.some((role) => baseline.scripts.includes(plan.workers[role].name)) ||
    baseline.incoming.length
  )
    fail("fixture name collision or incoming reference");
  for (const role of roles)
    if ((await api.request(settingsRoute(role), {}, { allow404: true })) !== undefined)
      fail("fixture name collision");
  ledger.startedAt = new Date(now()).toISOString();
  ledger.baseline = baseline;
  await persist();
  try {
    for (const role of roles) {
      checkClock();
      ledger.intents[role] = {
        at: new Date(now()).toISOString(),
        uncertain: true,
        exposurePending: true,
      };
      await persist();
      const meta = metadata(plan, role, token);
      ledger.intents[role].requestedMetadata = { ...meta, bindings: bindingShape(meta.bindings) };
      await persist();
      const form = new FormData();
      form.set("metadata", new Blob([JSON.stringify(meta)], { type: "application/json" }));
      form.set(
        meta.main_module,
        new Blob([moduleBytes(role)], { type: "application/javascript+module" }),
        meta.main_module,
      );
      await api.request(scriptRoute(role), { method: "PUT", body: form });
      ledger.intents[role].uncertain = false;
      await persist();
      if (!(await inspect(role))) fail("uploaded fixture missing");
      await api.configureSubdomain(plan.workers[role].name, false);
      ledger.intents[role].exposurePending = false;
      await persist();
      if (!(await inspect(role))) fail("uploaded fixture missing");
    }
    const receiver = await inspect("receiver");
    const caller = await inspect("caller");
    let environmentBindings;
    try {
      environmentBindings = bindingShape(
        await api.request(
          `/workers/services/${plan.workers.caller.name}/environments/${caller.environment}/bindings`,
        ),
      );
    } catch {
      environmentBindings = { unavailable: true };
    }
    ledger.observation = { requested: plan.props, receiver, caller, environmentBindings };
    // Arm the possible exposure change before dispatch, including lost replies.
    ledger.callerPublic = true;
    await persist();
    const url = await api.enableSubdomain(plan.workers.caller.name);
    if (!new RegExp(`^https://${plan.workers.caller.name}\\.[a-z0-9-]+\\.workers\\.dev$`).test(url))
      fail("unexpected invocation URL");
    await inspect("receiver");
    await inspect("caller", true);
    ledger.invocationIntent = true;
    await persist();
    const timeout = AbortSignal.timeout(checkClock());
    const response = await invoke(`${url}/observe`, {
      method: "POST",
      redirect: "error",
      headers: { authorization: `Bearer ${token}` },
      signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
    });
    const body = await readProbeResponse(response);
    if (!response.ok || body.length > 8192) fail("runtime observation failed or oversized");
    ledger.observation.runtime = JSON.parse(body);
    if (
      !equal(receiver, await inspect("receiver")) ||
      !equal(caller, await inspect("caller", true))
    )
      fail("fixture changed across runtime observation");
    ledger.observation.runtimePropsMatch = equal(ledger.observation.runtime, {
      protocol: "qualification-binding-probe-v1",
      props: plan.props,
    });
    ledger.observation.complete = !environmentBindings.unavailable;
    await persist();
  } catch {
    ledger.failed = true;
    ledger.failureAt = ledger.lastAction;
    // Do not persist provider/runtime error strings, tokens, or request bodies.
    await persist();
  } finally {
    await cleanup();
  }
  return ledger;
}

// Local journal custody is independent of the measured provider field. There
// is no automatic stale-lock reclamation: two reclaimers could otherwise race
// and remove a new live lock. A crash requires operator-confirmed local custody.
export async function withBindingProbeJournal(directory, plan, run, { create = false } = {}) {
  if (create) fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  if (
    fs.realpathSync(directory) !== path.resolve(directory) ||
    fs.statSync(directory).mode & 0o077 ||
    fs.statSync(directory).uid !== process.getuid()
  )
    fail("journal directory must be private and contain no symlinks");
  const lock = path.join(directory, "binding-probe.lock");
  const fd = fs.openSync(lock, "wx", 0o600);
  fs.writeFileSync(fd, JSON.stringify({ host: os.hostname(), pid: process.pid }));
  fs.fsyncSync(fd);
  fs.closeSync(fd);
  const file = path.join(directory, "binding-probe.json");
  try {
    let ledger;
    try {
      ledger = readRegular(file);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
      if (!create) fail("recovery requires its existing durable journal");
      ledger = { planHash: digest(canonical(plan)), intents: {} };
    }
    const save = async (value) => {
      const temporary = `${file}.${randomBytes(8).toString("hex")}.candidate`;
      const out = fs.openSync(temporary, "wx", 0o600);
      try {
        try {
          fs.writeFileSync(out, `${JSON.stringify(value, null, 2)}\n`);
          fs.fsyncSync(out);
        } finally {
          fs.closeSync(out);
        }
        fs.renameSync(temporary, file);
        const dir = fs.openSync(directory, "r");
        try {
          fs.fsyncSync(dir);
        } finally {
          fs.closeSync(dir);
        }
      } finally {
        if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
      }
    };
    return await run(ledger, save);
  } finally {
    fs.unlinkSync(lock);
  }
}

// These opt-in commands use the existing qualification Cloudflare transport and
// credential custody. No scheduled workflow dispatches them. Source admission,
// current credential instructions and a live cycle still require operator approval.
export async function bindingProbeCommand(command, args, Cloudflare, env = process.env) {
  const root = path.dirname(directory);
  const sha = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
  const dirty = execFileSync("git", ["status", "--porcelain", "--untracked-files=all"], {
    cwd: root,
    encoding: "utf8",
  });
  if (dirty || env.QUALIFICATION_BINDING_PROBE_SOURCE_SHA !== sha)
    fail("requires clean explicitly admitted source");
  if (command === "binding-probe-plan") {
    if (args.length !== 4) fail("plan requires output, token ID, nonce and UTC start time");
    const plan = prepareBindingProbe({
      accountId: env.CLOUDFLARE_ACCOUNT_ID,
      sourceSha: sha,
      tokenId: args[1],
      id: args[2],
      startsAt: args[3],
    });
    const fd = fs.openSync(args[0], "wx", 0o600);
    try {
      fs.writeFileSync(fd, `${JSON.stringify(plan, null, 2)}\n`);
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    return;
  }
  if (!["binding-probe-run", "binding-probe-recover"].includes(command) || args.length !== 2)
    fail("run/recover requires frozen plan and private journal directory");
  const plan = readRegular(args[0]);
  validatePlan(plan);
  if (
    plan.sourceSha !== sha ||
    plan.accountId !== env.CLOUDFLARE_ACCOUNT_ID ||
    !env.CLOUDFLARE_API_TOKEN
  )
    fail("source or credential coordinates changed");
  const cf = new Cloudflare(plan.accountId, env.CLOUDFLARE_API_TOKEN);
  const cancellation = new AbortController();
  const stop = () => cancellation.abort();
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
  try {
    const ledger = await withBindingProbeJournal(
      args[1],
      plan,
      (value, save) =>
        bindingProbe(cf, plan, value, {
          mode: command === "binding-probe-run" ? "run" : "recover",
          // An ephemeral invocation key is never persisted or printed. Recovery
          // uses management identity only; it cannot invoke or create the fixture.
          token: randomBytes(32).toString("hex"),
          save,
          signal: cancellation.signal,
        }),
      { create: command === "binding-probe-run" },
    );
    if (
      !ledger.settled ||
      (command === "binding-probe-run" && (ledger.failed || !ledger.observation?.complete))
    )
      fail("incomplete observation or settlement; inspect private journal");
  } finally {
    process.removeListener("SIGINT", stop);
    process.removeListener("SIGTERM", stop);
  }
}

export function readRegular(file) {
  const fd = fs.openSync(
    file,
    fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK,
  );
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || stat.size > 1024 * 1024) fail("invalid journal/plan file");
    return JSON.parse(fs.readFileSync(fd, "utf8"));
  } finally {
    fs.closeSync(fd);
  }
}
