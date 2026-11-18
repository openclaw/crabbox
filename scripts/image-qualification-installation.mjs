// Protected qualification installation. Never load this record from a candidate,
// PR, artifact or executor. Run ownership remains in the existing authority.
import fs from "node:fs";
import path from "node:path";
import {
  canonical,
  digest,
  controllerName,
  bindingShape,
  verifyOwned,
  ownedNamespaces,
} from "./image-qualification-isolation.mjs";
import { withQualificationJournal } from "./image-qualification-journal.mjs";
import { managementEndpoint, managementFailure } from "./image-qualification-binding-probe.mjs";

const sha = /^[0-9a-f]{64}$/;
const equal = (a, b) => canonical(a) === canonical(b);
const fail = (message) => {
  throw Error(`qualification installation: ${message}`);
};
const failure = (error) =>
  managementFailure(error) ?? {
    reason: /cancel|abort/i.test(error?.message ?? "")
      ? "cancelled"
      : /deadline|expired|bound exhausted/i.test(error?.message ?? "")
        ? "deadline-or-bound"
        : /unowned|ownership/i.test(error?.message ?? "")
          ? "ownership-mismatch"
          : /version/i.test(error?.message ?? "")
            ? "version-mismatch"
            : /binding/i.test(error?.message ?? "")
              ? "binding-mismatch"
              : /exposure|subdomain/i.test(error?.message ?? "")
                ? "exposure-mismatch"
                : /namespace/i.test(error?.message ?? "")
                  ? "namespace-mismatch"
                  : /module|executable|source/i.test(error?.message ?? "")
                    ? "source-mismatch"
                    : /registry|custody|active|probe/i.test(error?.message ?? "")
                      ? "custody-unresolved"
                      : "identity-or-lifecycle-mismatch",
  };

const keys = (v, expected) => equal(Object.keys(v ?? {}).sort(), expected.sort());

export function readInstallation(env = process.env) {
  const raw = env.QUALIFICATION_INSTALLATION_JSON;
  if (!raw && !env.QUALIFICATION_INSTALLATION_SHA256) return undefined;
  if (env.QUALIFICATION_TRANSPORT_PROFILE !== "candidate-transport-v2")
    fail("retained installation requires candidate-transport-v2");
  if (!raw || raw.length > 16_384) fail("protected descriptor missing or oversized");
  const record = JSON.parse(raw);
  if (
    !sha.test(env.QUALIFICATION_INSTALLATION_SHA256 ?? "") ||
    digest(canonical(record)) !== env.QUALIFICATION_INSTALLATION_SHA256
  )
    fail("protected descriptor digest mismatch");
  validateInstallation(record);
  if (record.accountId !== env.CLOUDFLARE_ACCOUNT_ID) fail("account mismatch");
  return record;
}

// Namespace/version coordinates are read back at installation. Stable ownership
// deliberately excludes the authority's credential-dependent physical version.
export function installationId(record) {
  return digest(
    canonical({
      profile: record.profile,
      accountId: record.accountId,
      candidateSha: record.candidateSha,
      candidate: { name: record.candidate?.name, module: record.candidate?.module },
      relay: record.relay,
      controller: record.controller,
    }),
  );
}
export function validateInstallation(record) {
  if (
    !keys(record, [
      "profile",
      "id",
      "accountId",
      "candidateSha",
      "candidate",
      "controller",
      "relay",
    ]) ||
    record.profile !== "qualification-installation/v1" ||
    !/^[0-9a-f]{32}$/.test(record.accountId ?? "") ||
    !/^[0-9a-f]{40}$/.test(record.candidateSha ?? "") ||
    !keys(record.candidate, ["name", "module", "namespaceId"]) ||
    !/^crabbox-image-qualification-[1-9][0-9]*-1$/.test(record.candidate.name ?? "") ||
    !/^[0-9a-f]{32}$/.test(record.candidate.namespaceId ?? "") ||
    !keys(record.controller, ["name", "module"]) ||
    record.controller.name !== controllerName ||
    !keys(record.relay, ["name", "module"]) ||
    record.relay.name !== record.candidate.name.replace("qualification-", "qualification-relay-") ||
    [record.candidate, record.controller, record.relay].some(
      ({ module }) =>
        !keys(module, ["module", "count", "sha256"]) ||
        module.count !== 1 ||
        !/^[A-Za-z0-9_.-]+$/.test(module.module ?? "") ||
        !sha.test(module.sha256 ?? ""),
    ) ||
    !sha.test(record.id ?? "") ||
    record.id !== installationId(record)
  )
    fail("invalid pinned installation");
  return record;
}

export async function installedURL(cf, name) {
  const exposure = await cf.request(`/workers/scripts/${name}/subdomain`);
  if (exposure?.enabled !== true || exposure?.previews_enabled !== false)
    fail("authenticated ingress exposure differs");
  const subdomain = (await cf.request("/workers/subdomain"))?.subdomain;
  if (!/^[a-z0-9-]+$/.test(subdomain ?? "")) fail("account subdomain missing");
  return `https://${name}.${subdomain}.workers.dev`;
}

export async function verifyInstallation(cf, { candidateSha } = {}) {
  const record = validateInstallation(cf.installation);
  if (
    cf.accountId !== record.accountId ||
    (candidateSha && candidateSha !== record.candidateSha) ||
    cf.admission.production.namespaceIds.includes(record.candidate.namespaceId) ||
    [record.candidate, record.relay, record.controller].some((v) =>
      cf.admission.production.workers.includes(v.name),
    )
  )
    fail("installation/source isolation differs");
  const versions = {};
  for (const [role, worker] of Object.entries({
    candidate: record.candidate,
    relay: record.relay,
    controller: record.controller,
  })) {
    const version = await cf.latestVersion(worker.name);
    const settings = await verifyOwned(cf, worker.name);
    if (
      settings.compatibility_date !== "2026-09-03" ||
      !equal(settings.compatibility_flags ?? [], role === "candidate" ? ["nodejs_compat"] : [])
    )
      fail(`${role} compatibility differs`);
    const own = await ownedNamespaces(cf, worker.name, settings.bindings);
    if (
      !equal(
        own.map((n) => n.id).sort(),
        role === "candidate" ? [record.candidate.namespaceId] : [],
      )
    )
      fail("namespace differs");
    const module = await cf.request(
      `/workers/scripts/${worker.name}/content/v2`,
      {},
      { contentIdentityBytes: 4 * 1024 * 1024 },
    );
    if (!equal(module, worker.module)) fail(`${role} executable differs`);
    const [exposure, routes, schedules, domains] = await Promise.all([
      cf.request(`/workers/scripts/${worker.name}/subdomain`),
      cf.request(
        `/workers/services/${worker.name}/environments/production/routes?show_zonename=true`,
      ),
      cf.request(`/workers/scripts/${worker.name}/schedules`),
      cf.list(`/workers/domains?service=${worker.name}`),
    ]);
    if (
      exposure?.enabled !== (role !== "candidate") ||
      exposure?.previews_enabled !== false ||
      !equal(routes, []) ||
      !equal(schedules?.schedules, []) ||
      !equal(domains, []) ||
      !equal(settings.tail_consumers ?? [], []) ||
      (await cf.latestVersion(worker.name)) !== version
    )
      fail(`${role} exposure, capability or version differs`);
    versions[role] = { version, bindingsSha256: digest(canonical(settings.bindings)) };
  }
  return versions;
}

// Persist intent before every request, including the first mutation and failures
// before deployment.json. Never copy headers, bodies, tokens, URLs or raw errors.
export async function withInstallationJournal(
  cf,
  directory,
  operation,
  work,
  { signal, deadline = Date.now() + 15 * 60_000, recover = false } = {},
) {
  if (!Number.isFinite(deadline) || deadline <= Date.now()) fail("deadline expired");
  return withQualificationJournal(
    directory,
    {
      stateName: "installation-operation.json",
      lockName: "installation-operation.lock",
      create: !recover,
      initial: () => ({
        profile: "qualification-installation-operation/v1",
        installationId: cf.installation.id,
        descriptorSha256: digest(canonical(cf.installation)),
        candidateSha: cf.installation.candidateSha,
        modules: Object.fromEntries(
          ["candidate", "controller", "relay"].map((role) => [role, cf.installation[role].module]),
        ),
        namespaceSha256: digest(cf.installation.candidate.namespaceId),
        authorityVersion: cf.admission.authority.version,
        authoritySourceSha: cf.admission.authority.sourceSha,
        operation,
        deadline,
        recoveryDeadline: deadline + 15 * 60_000,
        events: [],
        outcome: "pending",
      }),
    },
    async (state, save) => {
      if (recover) {
        if (
          operation !== "install" ||
          state.operation !== "install" ||
          state.profile !== "qualification-installation-operation/v1" ||
          state.installationId !== cf.installation.id ||
          state.candidateSha !== cf.installation.candidateSha ||
          !equal(
            state.modules,
            Object.fromEntries(
              ["candidate", "controller", "relay"].map((role) => [
                role,
                cf.installation[role].module,
              ]),
            ),
          ) ||
          state.authorityVersion !== cf.admission.authority.version ||
          state.authoritySourceSha !== cf.admission.authority.sourceSha ||
          !Array.isArray(state.events) ||
          !Number.isFinite(state.deadline) ||
          state.recoveryDeadline !== state.deadline + 15 * 60_000
        )
          fail("original installation custody differs");
        deadline = Math.min(deadline, state.recoveryDeadline);
        if (Date.now() >= deadline) fail("original installation recovery deadline expired");
      } else if (state.events.length || state.outcome !== "pending")
        fail("original operation exists; reconcile, never replay");
      const original = cf.request;
      const api = Object.create(cf);
      api.recoverySignal = () => signal;
      api.controllerDeadline = deadline;
      api.request = async (route, init = {}, options = {}) => {
        if (
          (init.method ?? "GET") !== "GET" &&
          api.preparationStopsAt !== undefined &&
          Date.now() >= api.preparationStopsAt
        )
          fail("preparation activation deadline reached");
        if (signal?.aborted || Date.now() >= deadline || state.events.length >= 1600)
          fail("cancelled or bounded operation exhausted");
        const event = {
          method: init.method ?? "GET",
          endpoint: managementEndpoint(route),
          outcome: "intent",
          at: new Date().toISOString(),
        };
        const role = Object.entries({
          candidate: cf.installation.candidate,
          relay: cf.installation.relay,
          controller: cf.installation.controller,
        }).find(
          ([, worker]) =>
            route === `/workers/scripts/${worker.name}` ||
            route.startsWith(`/workers/scripts/${worker.name}/`),
        )?.[0];
        if (role) event.role = role;
        if (init.method === "PUT" && init.body instanceof FormData) {
          const metadata = JSON.parse(await init.body.get("metadata").text());
          event.requestedBindingsSha256 = digest(canonical(bindingShape(metadata.bindings)));
          const module = init.body.get(metadata.main_module);
          event.requestedModuleSha256 = digest(Buffer.from(await module.arrayBuffer()));
          event.requestedModule = metadata.main_module;
          event.requestedTagsSha256 = digest(canonical([...metadata.tags].sort()));
        }
        state.events.push(event);
        await save(state);
        try {
          const content = options.contentIdentityBytes || options.probeContentIdentity;
          const result = await original.call(
            api,
            route,
            {
              ...init,
              redirect: "error",
              signal: AbortSignal.any([
                AbortSignal.timeout(Math.max(1, Math.min(15_000, deadline - Date.now()))),
                ...(signal ? [signal] : []),
                ...(init.signal ? [init.signal] : []),
              ]),
            },
            {
              ...options,
              boundedBytes: route === "/workers/scripts" ? 1024 * 1024 : 64 * 1024,
              ...(!content ? { responseMetadata: true } : {}),
            },
          );
          event.outcome = "acknowledged";
          if (content) event.contentSha256 = result.sha256;
          else {
            event.status = result.response.status;
            if (/^[a-zA-Z0-9-]{1,128}$/.test(result.response.ray ?? ""))
              event.ray = result.response.ray;
            const value = options.envelope ? result.result?.result : result.result;
            if (
              typeof value?.deployment_id === "string" &&
              /^[a-f0-9-]{36}$/.test(value.deployment_id)
            )
              event.version = value.deployment_id;
            if (event.endpoint === "deployments") {
              const deployments = Array.isArray(value)
                ? value
                : (value?.deployments ?? value?.items);
              const versions = deployments?.[0]?.versions;
              event.deploymentShape = !Array.isArray(deployments)
                ? "malformed"
                : deployments.length
                  ? "nonempty"
                  : "empty";
              if (Array.isArray(versions))
                event.versions = versions.slice(0, 2).map((v) => ({
                  id: /^[a-f0-9-]{36}$/.test(v.version_id ?? "") ? v.version_id : null,
                  percentage: Number.isFinite(v.percentage) ? v.percentage : null,
                }));
            }
            if (typeof value?.enabled === "boolean") event.enabled = value.enabled;
            if (typeof value?.previews_enabled === "boolean")
              event.previewsEnabled = value.previews_enabled;
            if (Array.isArray(value?.bindings))
              event.bindingsSha256 = digest(canonical(bindingShape(value.bindings)));
          }
          await save(state);
          if (signal?.aborted || Date.now() >= deadline) fail("cancelled or deadline expired");
          return content || options.responseMetadata ? result : result.result;
        } catch (error) {
          event.outcome = event.method === "GET" ? "unverified" : "uncertain";
          event.failure = failure(error);
          await save(state);
          throw error;
        }
      };
      try {
        const value = await work(api, state);
        state.outcome = value?.result === "failed" ? "failed" : "verified";
        await save(state);
        return value;
      } catch (error) {
        state.outcome = "unresolved";
        state.classification = failure(error);
        await save(state);
        throw error;
      }
    },
  );
}

export function installationModulesMatch(
  record,
  artifactDir,
  manifest,
  controllerSource,
  relaySource,
) {
  const modules = manifest.files.filter(
    (v) => v.path.startsWith("worker/") && /\.(?:m?js|wasm)$/.test(v.path),
  );
  if (
    modules.length !== 1 ||
    manifest.candidateSha !== record.candidateSha ||
    record.candidate.module.module !== manifest.mainModule.replace(/^worker\//, "") ||
    record.candidate.module.sha256 !==
      digest(fs.readFileSync(path.join(artifactDir, manifest.mainModule))) ||
    record.controller.module.module !== "controller.mjs" ||
    record.relay.module.module !== "relay.mjs" ||
    record.controller.module.sha256 !== digest(fs.readFileSync(controllerSource)) ||
    record.relay.module.sha256 !== digest(fs.readFileSync(relaySource))
  )
    fail("artifact or protected module differs from pinned installation");
}
