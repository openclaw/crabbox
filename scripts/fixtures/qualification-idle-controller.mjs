// Offline lifecycle fixture. All identities, credentials and provider data fake.
import assert from "node:assert/strict";
import { Cloudflare } from "../image-qualification-control.mjs";
import { fixtureAdmission, version } from "./qualification-isolation.mjs";
import {
  authorityName,
  controllerName,
  bindingShape,
  canonical,
  digest,
} from "../image-qualification-isolation.mjs";
import {
  idleController,
  idleInitialState,
  idleSourceIdentity,
} from "../image-qualification-idle-controller.mjs";

export const start = Date.parse("2026-11-01T00:00:00.000Z");
export const callerVersion = "22222222-2222-4222-8222-222222222222";
export function idleFixture(options = {}) {
  const f = fixtureAdmission();
  f.bindings.push({ name: "CF_VERSION_METADATA", type: "version_metadata" });
  f.record.authority.bindingDigest = digest(canonical(bindingShape(f.bindings)));
  const authoritySettings = { bindings: f.bindings };
  const baseline = {
    scripts: [authorityName],
    namespaceIds: f.record.authority.namespaces.map((n) => n.id).sort(),
    incoming: [],
  };
  const plan = {
    profile: "idle-controller/v1",
    attemptId: "f".repeat(24),
    sourceSha: "b".repeat(40),
    tokenId: "a".repeat(32),
    authorId: "b".repeat(32),
    sources: idleSourceIdentity(),
    originalInventorySha256: digest(canonical(baseline)),
    authoritySettingsSha256: digest(canonical(authoritySettings)),
    authorityModule: { module: "authority.js", count: 1, sha256: "d".repeat(64) },
    custodyReceiptSha256: "e".repeat(64),
    defaultEnvironment: "production",
    props: { deploymentHash: "c".repeat(64) },
    owner: "qualification-owner",
    fallback: "qualification-operator",
    uploadExposure: [{ enabled: true, previews_enabled: true }],
    maxRequests: 800,
    startsAt: new Date(start).toISOString(),
    forwardStopsAt: new Date(start + 600000).toISOString(),
    deadline: new Date(start + 1200000).toISOString(),
    isolation: f.record,
  };
  const state = idleInitialState(plan);
  const saved = [];
  const model = { worker: null, calls: [], runtime: [], time: start + 1000 };
  class FixtureCloudflare extends Cloudflare {
    constructor() {
      super(f.record.accountId, "fake-token");
    }
    async request(route, init = {}, opts = {}) {
      const url = new URL(route, "https://fixture.invalid");
      const pathname = url.pathname;
      const method = init.method ?? "GET";
      const call = { route, pathname, method };
      model.calls.push(call);
      await options.before?.(call, model, init);
      assert.equal(saved.at(-1)?.requests, state.requests, "request budget persisted");
      if (init.signal?.aborted) throw new Error("aborted");
      const list = (result) =>
        opts.envelope ? { result, result_info: { total_pages: 1 } } : result;
      if (pathname === "/tokens/verify")
        return {
          id: plan.tokenId,
          status: "active",
          expires_on: new Date(start + 86400000).toISOString(),
          ...options.credential,
        };
      if (pathname === "/workers/scripts")
        return list([{ id: authorityName }, ...(model.worker ? [{ id: controllerName }] : [])]);
      if (pathname === "/workers/durable_objects/namespaces")
        return list(f.record.authority.namespaces);
      if (pathname === "/workers/domains") return list(options.domains ?? []);
      if (pathname === "/workers/subdomain") return { subdomain: "fixture" };
      if (
        pathname === `/workers/services/${authorityName}` ||
        pathname === `/workers/services/${controllerName}`
      )
        return { default_environment: { environment: options.environment ?? "production" } };
      if (pathname.startsWith(`/workers/scripts/${authorityName}`)) {
        if (pathname.endsWith("settings")) return authoritySettings;
        if (pathname.endsWith("deployments"))
          return {
            deployments: [
              { versions: [{ version_id: options.authorityVersion ?? version, percentage: 100 }] },
            ],
          };
        if (pathname.endsWith("content/v2")) return plan.authorityModule;
        if (pathname.endsWith("subdomain")) return { enabled: false, previews_enabled: false };
        if (pathname.endsWith("schedules")) return { schedules: [] };
      }
      if (pathname.endsWith("/routes")) return [];
      assert.ok(pathname.startsWith(`/workers/scripts/${controllerName}`), route);
      if (method === "PUT") {
        assert.equal(saved.at(-1).phase, "create-intent");
        if (options.lostUploadAbsent) throw new Error("uncertain create");
        const metadata = JSON.parse(await init.body.get("metadata").text());
        const bytes = Buffer.from(await init.body.get("controller.mjs").arrayBuffer());
        model.worker = {
          metadata,
          sha256: digest(bytes),
          exposure: { enabled: true, previews_enabled: true },
        };
        if (options.lostUpload) throw new Error("lost create reply");
        return {};
      }
      if (!model.worker) {
        if (opts.allow404) return undefined;
        throw new Error("absent");
      }
      const w = model.worker;
      const bindings = () =>
        w.metadata.bindings.map((binding) => {
          if (binding.type === "secret_text") return { name: binding.name, type: binding.type };
          if (binding.type === "service") {
            const { props, ...visible } = binding;
            return {
              ...visible,
              ...(options.keepProps ? { props } : {}),
              environment: "production",
              ...options.binding,
            };
          }
          return binding;
        });
      if (method === "DELETE") {
        assert.equal(url.search, "");
        assert.equal(saved.at(-1).phase, "delete-intent");
        if (options.refuseDelete) throw new Error("delete denied");
        model.worker = null;
        if (options.lostDelete) throw new Error("lost delete reply");
        return {};
      }
      if (pathname.endsWith("settings")) return { ...w.metadata, bindings: bindings() };
      if (pathname.endsWith("deployments"))
        return {
          deployments: [
            { versions: [{ version_id: options.callerVersion ?? callerVersion, percentage: 100 }] },
          ],
        };
      if (pathname.endsWith(`/versions/${options.callerVersion ?? callerVersion}`))
        return {
          id: options.callerVersion ?? callerVersion,
          number: options.number ?? 1,
          metadata: {
            author_id: options.authorId ?? plan.authorId,
            created_on: new Date(start + 1000).toISOString(),
          },
          resources: { bindings: bindings() },
        };
      if (pathname.endsWith("/versions"))
        return list([{ id: options.callerVersion ?? callerVersion }]);
      if (pathname.endsWith("content/v2"))
        return { module: "controller.mjs", count: 1, sha256: options.module ?? w.sha256 };
      if (pathname.endsWith("subdomain")) {
        if (method === "POST") {
          w.exposure = JSON.parse(init.body);
          if (options.lostExposure) throw new Error("lost exposure reply");
        }
        return w.exposure;
      }
      if (pathname.endsWith("schedules")) return { schedules: [] };
      throw new Error(`unexpected fixture route ${route}`);
    }
  }
  const cf = new FixtureCloudflare();
  const save = async (next) => {
    await options.save?.(next);
    saved.push(structuredClone(next));
  };
  const invoke = async (url, init) => {
    model.runtime.push(url);
    await options.invoke?.(url, init, model);
    if (url.endsWith("/discover")) return Response.json(options.discover ?? { run: null });
    if (typeof options.reply === "function") return Response.json(await options.reply(init));
    return Response.json(
      options.reply ?? {
        controllerVersion: callerVersion,
        identity: {
          protocol: 1,
          role: "controller",
          challenge: JSON.parse(init.body).challenge,
          props: options.props ?? plan.props,
          authorityVersion: version,
        },
      },
    );
  };
  const context = {
    save,
    token: "f".repeat(64),
    now: () => model.time,
    signal: options.signal,
    invoke,
  };
  const run = () => idleController(cf, plan, state, context);
  const recover = () => idleController(cf, plan, state, { ...context, mode: "recover" });
  return { plan, state, model, saved, cf, save, context, run, recover, options };
}
