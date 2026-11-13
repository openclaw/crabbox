import { afterEach, describe, expect, it, vi } from "vitest";

import {
  AWSQualificationController,
  AWSQualificationRegistry,
  AWSQualificationTransport,
} from "../src/aws-qualification-authority";
import type {
  AWSQualificationRunIdentity,
  AWSQualificationTransportProbeRequest,
} from "../src/aws-qualification-contract";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import { qualificationTransportProbeResponse } from "../src/qualification-transport-probe";
import type { Env } from "../src/types";

const version = "11111111-1111-1111-1111-111111111111";
const controller = { deploymentHash: "d".repeat(64) };
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
class Storage {
  values = new Map<string, unknown>();
  async get<T>(key: string): Promise<T | undefined> {
    return structuredClone(this.values.get(key)) as T | undefined;
  }
  async put(key: string, value: unknown): Promise<void> {
    this.values.set(key, structuredClone(value));
  }
}
function fixture() {
  const storage = new Storage();
  const identity: AWSQualificationRunIdentity = {
    runId: "image-qualification-42-1",
    owner: "maintainer",
    candidateSha: "a".repeat(40),
    candidateWorker: "crabbox-image-qualification-42-1",
    ...controller,
    expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
    firstWalk: true,
  };
  const request: AWSQualificationTransportProbeRequest = {
    identity,
    challenge: "b".repeat(64),
    authorityVersion: version,
    candidateVersion: version,
    expiresAt: new Date(Date.now() + 9 * 60_000).toISOString(),
  };
  const enroll = vi.fn<() => Promise<void>>(async () => {});
  const runNamespace = { idFromName: (id: string) => id, get: () => ({ enroll }) };
  const registry = new AWSQualificationRegistry(
    { storage } as never,
    { CF_VERSION_METADATA: { id: version }, AWS_QUALIFICATION_RUNS: runNamespace } as never,
  );
  const env = {
    CF_VERSION_METADATA: { id: version },
    AWS_QUALIFICATION_REGISTRY: { idFromName: (id: string) => id, get: () => registry },
    AWS_QUALIFICATION_RUNS: runNamespace,
  };
  const transport = new AWSQualificationTransport(env as never, identity);
  const trusted = new AWSQualificationController(env as never, controller);
  const reopen = () =>
    new AWSQualificationRegistry(
      { storage } as never,
      { CF_VERSION_METADATA: { id: version }, AWS_QUALIFICATION_RUNS: runNamespace } as never,
    );
  return { storage, identity, request, registry, transport, trusted, env, enroll, reopen };
}
describe("canonical pre-enrollment transport challenge", () => {
  it("observes actual bound props/version, settles once, then admits enrollment", async () => {
    const f = fixture();
    vi.stubGlobal(
      "fetch",
      vi.fn<typeof globalThis.fetch>(() => {
        throw Error("no network");
      }),
    );
    expect(await f.trusted.openTransportProbe(f.request)).toMatchObject({ state: "pending" });
    await f.transport.observeBinding(f.request.challenge);
    expect(await f.trusted.discoverTransportProbe()).toEqual({
      protocol: 1,
      ...f.request,
      state: "observed",
      observedAt: expect.any(String),
    });
    expect(f.enroll).not.toHaveBeenCalled();
    await f.trusted.settleTransportProbe(f.request.challenge, true);
    await f.trusted.claim(f.identity);
    expect(f.enroll).toHaveBeenCalledOnce();
    await expect(
      f.trusted.openTransportProbe({ ...f.request, challenge: "c".repeat(64) }),
    ).rejects.toThrow("active");
    expect(fetch).not.toHaveBeenCalled();
    expect([...f.storage.values.keys()].toSorted()).toEqual(["active", "transport-probe"]);
  });
  for (const state of ["pending", "observed", "expired"] as const)
    it(`blocks new and legacy claims and idle discovery while ${state}`, async () => {
      const f = fixture();
      await f.trusted.openTransportProbe(f.request);
      if (state === "observed") await f.transport.observeBinding(f.request.challenge);
      if (state === "expired") vi.spyOn(Date, "now").mockReturnValue(Date.now() + 10 * 60_000);
      await expect(f.trusted.claim(f.identity)).rejects.toThrow("unsettled");
      await expect(f.registry.claim(controller, f.identity)).rejects.toThrow("unsettled");
      await expect(f.trusted.discover()).rejects.toThrow("unsettled");
      expect(f.enroll).not.toHaveBeenCalled();
      await f.trusted.settleTransportProbe(f.request.challenge, false);
      await expect(f.trusted.claim(f.identity)).rejects.toThrow("cancelled");
    });
  for (const key of [
    "owner",
    "runId",
    "candidateSha",
    "candidateWorker",
    "deploymentHash",
    "expiresAt",
    "firstWalk",
    "extra",
  ] as const)
    it(`rejects wrong/extra actual props: ${key}`, async () => {
      const f = fixture();
      await f.trusted.openTransportProbe(f.request);
      const props = { ...f.identity, [key]: key === "firstWalk" ? undefined : "wrong" };
      const transport = new AWSQualificationTransport(f.env as never, props as never);
      await expect(transport.observeBinding(f.request.challenge)).rejects.toThrow(
        /transport|qualification/i,
      );
      expect((await f.trusted.discoverTransportProbe())?.state).toBe("pending");
      expect(f.enroll).not.toHaveBeenCalled();
    });
  it("rejects forged response without observation, wrong challenge/version, replay and extra arguments", async () => {
    const f = fixture();
    await f.trusted.openTransportProbe(f.request);
    await expect(f.trusted.settleTransportProbe(f.request.challenge, true)).rejects.toThrow(
      "not observed",
    );
    await expect(f.transport.observeBinding("c".repeat(64))).rejects.toThrow(
      /transport|qualification/i,
    );
    await expect(
      f.registry.observeTransport(
        f.identity,
        f.request.challenge,
        "22222222-2222-2222-2222-222222222222",
      ),
    ).rejects.toThrow(/transport|qualification/i);
    await expect(
      (f.transport.observeBinding as (...args: unknown[]) => Promise<void>)(
        f.request.challenge,
        f.identity,
      ),
    ).rejects.toThrow(/transport|qualification/i);
    await f.transport.observeBinding(f.request.challenge);
    await expect(f.transport.observeBinding(f.request.challenge)).rejects.toThrow(
      /transport|qualification/i,
    );
    await f.trusted.settleTransportProbe(f.request.challenge, true);
    await expect(f.transport.observeBinding(f.request.challenge)).rejects.toThrow(
      /transport|qualification/i,
    );
    await expect(f.trusted.openTransportProbe(f.request)).rejects.toThrow("settled");
    await expect(
      f.trusted.openTransportProbe({ ...f.request, challenge: "c".repeat(64) }),
    ).rejects.toThrow("tombstone");
  });
  it("rejects malformed request, extra fields, expired proof, wrong authority and oversized identity", async () => {
    const f = fixture();
    for (const request of [
      { ...f.request, extra: true },
      { ...f.request, expiresAt: new Date(Date.now() - 1).toISOString() },
      { ...f.request, expiresAt: new Date(Date.now() + 11 * 60_000).toISOString() },
      { ...f.request, authorityVersion: "22222222-2222-2222-2222-222222222222" },
      { ...f.request, identity: { ...f.identity, owner: "x".repeat(300) } },
    ])
      // eslint-disable-next-line no-await-in-loop -- verify no rejected attempt admits state
      await expect(f.trusted.openTransportProbe(request)).rejects.toThrow(
        /transport|qualification/i,
      );
    await f.trusted.openTransportProbe(f.request);
    await f.transport.observeBinding(f.request.challenge);
    vi.spyOn(Date, "now").mockReturnValue(Date.now() + 10 * 60_000);
    await expect(f.trusted.settleTransportProbe(f.request.challenge, true)).rejects.toThrow(
      /transport|qualification/i,
    );
    await f.trusted.settleTransportProbe(f.request.challenge, false);
    expect(f.storage.values.size).toBe(1);
  });
  it("recovers the same durable slot after every effect, with no new admission", async () => {
    const f = fixture();
    await f.trusted.openTransportProbe(f.request);
    let registry = f.reopen();
    await registry.openTransportProbe(controller, f.request); // uncertain open is idempotent
    await registry.observeTransport(f.identity, f.request.challenge, version);
    registry = f.reopen();
    expect((await registry.discoverTransportProbe({ deploymentHash: "e".repeat(64) }))?.state).toBe(
      "observed",
    );
    await expect(
      registry.settleTransportProbe({ deploymentHash: "e".repeat(64) }, f.request.challenge, false),
    ).rejects.toThrow(/transport|qualification/i);
    await registry.settleTransportProbe(controller, f.request.challenge, false);
    registry = f.reopen();
    expect(
      (await registry.settleTransportProbe(controller, f.request.challenge, false)).state,
    ).toBe("cancelled");
    await expect(
      registry.settleTransportProbe(controller, f.request.challenge, true),
    ).rejects.toThrow(/transport|qualification/i);
    expect(f.storage.values.size).toBe(1);
    expect(f.enroll).not.toHaveBeenCalled();
  });
  it("serializes probe admission against the entire enrollment hop", async () => {
    const f = fixture();
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    f.enroll.mockImplementation(async () => {
      entered.resolve();
      await release.promise;
    });
    const claim = f.trusted.claim(f.identity);
    await entered.promise;
    const probe = f.trusted.openTransportProbe(f.request);
    void probe.catch(() => {});
    release.resolve();
    await claim;
    await expect(probe).rejects.toThrow("active");
    expect(f.enroll).toHaveBeenCalledOnce();
  });
  it("bounds queued registry operations and releases capacity after settlement", async () => {
    const f = fixture();
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    f.enroll.mockImplementationOnce(async () => {
      entered.resolve();
      await release.promise;
    });
    const first = f.trusted.claim(f.identity);
    await entered.promise;
    const waiting = Array.from({ length: 7 }, () => f.registry.claim(controller, f.identity));
    await expect(f.trusted.openTransportProbe(f.request)).rejects.toThrow("queue is full");
    release.resolve();
    await first;
    await Promise.all(waiting);
    await expect(f.trusted.openTransportProbe(f.request)).rejects.toThrow("active");
  });

  it("retains the bounded settled tombstone until expiry, then reuses one slot", async () => {
    const f = fixture();
    await f.trusted.openTransportProbe(f.request);
    await f.trusted.settleTransportProbe(f.request.challenge, false);
    const later = Date.now() + 10 * 60_000;
    vi.spyOn(Date, "now").mockReturnValue(later);
    await expect(f.trusted.openTransportProbe(f.request)).rejects.toThrow("expiry");
    await f.trusted.openTransportProbe({
      ...f.request,
      challenge: "c".repeat(64),
      expiresAt: new Date(later + 9 * 60_000).toISOString(),
    });
    expect(f.storage.values.size).toBe(1);
  });
});
describe("narrow candidate probe route", () => {
  it("normal auth rejects an unauthenticated request before dispatch", async () => {
    const fleet = vi.fn<() => Promise<Response>>(async () => new Response());
    const response = await routeCoordinatorRequest(
      new Request("https://fixture.invalid/v1/qualification/transport-probe", {
        method: "POST",
        body: JSON.stringify({ challenge: "b".repeat(64) }),
      }),
      { CRABBOX_ADMIN_TOKEN: "a".repeat(64) } as Env,
      fleet,
    );
    expect(response.status).toBe(401);
    expect(fleet).not.toHaveBeenCalled();
  });
  it("accepts only an admin nonce, no Fleet or run operation", async () => {
    const observeBinding = vi.fn<() => Promise<void>>(async () => {});
    const env = { CRABBOX_AWS_QUALIFICATION_TRANSPORT: { observeBinding } } as unknown as Env;
    const request = (body: unknown, admin = true) =>
      new Request("https://fixture.invalid/v1/qualification/transport-probe", {
        method: "POST",
        headers: { "x-crabbox-admin": String(admin) },
        body: JSON.stringify(body),
      });
    expect(
      (await qualificationTransportProbeResponse(request({ challenge: "b".repeat(64) }), env))
        ?.status,
    ).toBe(200);
    expect(
      (
        await qualificationTransportProbeResponse(
          request({ challenge: "b".repeat(64), props: {} }),
          env,
        )
      )?.status,
    ).toBe(409);
    expect(
      (
        await qualificationTransportProbeResponse(
          request({ challenge: "b".repeat(64) }, false),
          env,
        )
      )?.status,
    ).toBe(404);
    expect(observeBinding).toHaveBeenCalledExactlyOnceWith("b".repeat(64));
  });
});
