/* oxlint-disable eslint/no-await-in-loop -- Prefix checks inspect durable storage sequentially. */
import { afterEach, describe, expect, it, vi } from "vitest";

import type { CoordinatorStorageView } from "../src/coordinator-runtime";
import { FleetCoordinator, KoyebProvider } from "../src/fleet";
import {
  provisioningOperationKey,
  provisioningPlanKey,
  provisioningAttemptKey,
  type LeaseProvisioningOperation,
} from "../src/lease-provisioning";
import { orgKeyForLabel } from "../src/org-identity";
import type {
  FrozenProvisioningPlan,
  ProviderProvisioningCandidate,
  ProviderResumableProvisioning,
} from "../src/provider-provisioning";
import {
  openProvisioningMaterial,
  provisioningMaterialKey,
  type SealedProvisioningMaterial,
} from "../src/provisioning-material";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const leaseID = "cbx_abcdef123456";
const org = orgKeyForLabel("example-org");
const env = {
  FLEET: {},
  KOYEB_API_TOKEN: "synthetic-provider-token",
  CRABBOX_KOYEB_ORGANIZATION_ID: "33333333-3333-4333-8333-333333333333",
  CRABBOX_KOYEB_APP_ID: "44444444-4444-4444-8444-444444444444",
  CRABBOX_KOYEB_REGION: "was",
  CRABBOX_KOYEB_INSTANCE_TYPE: "large",
  CRABBOX_KOYEB_IMAGE: `ghcr.io/example/runner@sha256:${"a".repeat(64)}`,
  KOYEB_APP_ID: "44444444-4444-4444-8444-444444444444",
  KOYEB_APP_NAME: "my-app",
  KOYEB_ORGANIZATION_ID: "33333333-3333-4333-8333-333333333333",
  KOYEB_REGION: "was",
  CRABBOX_SESSION_SECRET: "synthetic-session-secret-with-32-characters",
  CRABBOX_DURABLE_PROVISIONING_ADMISSION: "true",
  CRABBOX_COST_RATES_JSON: JSON.stringify({ "koyeb:large": 1 }),
} as Env;

function candidate(index: number): ProviderProvisioningCandidate {
  const scope = `koyeb:context:v1:app-${index}`;
  return {
    lease: { providerScope: scope, providerProject: `app-${index}`, region: "was" },
    plan: {
      version: 1,
      provider: "koyeb",
      scope,
      resources: [{ cloudID: `worker-${index}`, region: "was", scope }],
      data: { selectedApp: `app-${index}` },
    },
    material: {
      adminPassword: `synthetic-password-${index}`,
      bootstrap: `bootstrap-${index}`,
      providerSecret: String(index).repeat(32),
    },
    step: { phase: "prepared", attempt: 0, state: { app: index }, nextWake: Date.now() },
  };
}

function request(): Request {
  return new Request("https://coordinator.test/v1/leases", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      prefer: "respond-async",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
      "x-crabbox-admin": "true",
    },
    body: JSON.stringify({
      leaseID,
      slug: "blue-lobster",
      provider: "koyeb",
      target: "linux",
      architecture: "amd64",
      tailscale: false,
      sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@example.com",
      ttlSeconds: 3600,
      idleTimeoutSeconds: 600,
    }),
  });
}

function fixture(
  options: {
    overrides?: Partial<Env>;
    candidates?: ProviderProvisioningCandidate[];
    retry?: boolean;
    beforePrepare?: (storage: ProvisioningTestStorage, lease: LeaseRecord) => Promise<void>;
  } = {},
) {
  const currentEnv = { ...env, ...options.overrides };
  const storage = new ProvisioningTestStorage();
  const runtime = new ProvisioningTestRuntime(storage);
  const candidates = options.candidates ?? [candidate(1), candidate(2)];
  const prepare = vi.fn<ProviderResumableProvisioning["prepare"]>(async (_config, lease) => {
    await options.beforePrepare?.(storage, lease);
    return { ...candidates[0]!, candidates };
  });
  const selectAdmission = vi.fn<NonNullable<ProviderResumableProvisioning["selectAdmission"]>>(
    async () => 1,
  );
  const advance = vi.fn<ProviderResumableProvisioning["advance"]>(async () => {
    throw new Error("provider dispatch is outside this admission fixture");
  });
  const provider = new KoyebProvider(currentEnv, async () => {
    throw new Error("external API call is outside this admission fixture");
  });
  provider.resumableProvisioning = () => ({
    version: 1,
    supports: () => true,
    prepare,
    selectAdmission,
    advance,
  });
  let rolledBack: LeaseProvisioningOperation | undefined;
  if (options.retry) {
    selectAdmission.mockResolvedValueOnce(0);
    const commitAndWake = runtime.commitAndWake.bind(runtime);
    const retrySignal = new Error("injected serialization retry");
    vi.spyOn(runtime, "commitAndWake").mockImplementation(
      async <T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> => {
        try {
          return await commitAndWake(async (transaction) => {
            const value = await callback(transaction);
            const operation = await transaction.get<LeaseProvisioningOperation>(
              provisioningOperationKey(leaseID),
            );
            if (!rolledBack && operation) {
              rolledBack = operation;
              throw retrySignal;
            }
            return value;
          });
        } catch (error) {
          if (error !== retrySignal) throw error;
          return commitAndWake(callback);
        }
      },
    );
  }
  const coordinator = new FleetCoordinator(runtime, currentEnv, { koyeb: provider });
  return {
    currentEnv,
    storage,
    coordinator,
    prepare,
    selectAdmission,
    advance,
    candidates,
    rolledBack: () => rolledBack,
  };
}

afterEach(() => vi.restoreAllMocks());

describe("Fleet durable provisioning candidate admission", () => {
  it("retries admission with coherent selected identity and material sealed only once per candidate", async () => {
    const f = fixture({ retry: true });
    const encrypt = vi.spyOn(crypto.subtle, "encrypt");
    const response = await f.coordinator.fetch(request());
    expect(response.status).toBe(202);
    expect(f.prepare).toHaveBeenCalledTimes(1);
    expect(f.selectAdmission).toHaveBeenCalledTimes(2);
    expect(encrypt).toHaveBeenCalledTimes(2);
    const operation = (await f.storage.get<LeaseProvisioningOperation>(
      provisioningOperationKey(leaseID),
    ))!;
    const record = (await f.storage.get<LeaseRecord>(`lease:${leaseID}`))!;
    const plan = (await f.storage.get<FrozenProvisioningPlan>(
      provisioningPlanKey(operation.operationID),
    ))!;
    const sealed = await f.storage.get<SealedProvisioningMaterial>(
      provisioningMaterialKey(operation.operationID),
    );
    expect(f.rolledBack()?.scope).toBe(f.candidates[0]!.plan.scope);
    expect(operation.scope).toBe(f.candidates[1]!.plan.scope);
    expect(operation.operationID).toBe(f.rolledBack()?.operationID);
    expect(operation.generation).toBe(f.rolledBack()?.generation);
    expect(record).toMatchObject({
      id: leaseID,
      owner: "alice@example.com",
      org,
      state: "provisioning",
      createAttemptGeneration: operation.generation,
      ...f.candidates[1]!.lease,
    });
    expect(plan).toEqual(f.candidates[1]!.plan);
    expect(operation.step).toEqual({
      ...f.candidates[1]!.step,
      state: { journal: provisioningAttemptKey(operation) },
    });
    expect(await f.storage.get(provisioningAttemptKey(operation))).toMatchObject({
      generation: operation.generation,
      state: f.candidates[1]!.step.state,
    });
    await expect(openProvisioningMaterial(f.currentEnv, operation, sealed)).resolves.toEqual(
      f.candidates[1]!.material,
    );
    await expect(
      openProvisioningMaterial(
        f.currentEnv,
        { ...operation, scope: f.candidates[0]!.plan.scope },
        sealed,
      ),
    ).rejects.toThrow("protected provisioning material is unavailable");
    for (const prefix of [
      "lease:",
      "lease-provisioning:",
      "provisioning-plan:",
      "provisioning-material:",
      "provisioning-attempt:",
      "provisioning-due:",
    ]) {
      expect((await f.storage.list({ prefix })).size).toBe(1);
    }
    const publicText = await response.text();
    expect(publicText).not.toContain(f.candidates[1]!.material.adminPassword);
    expect(publicText).not.toContain(f.candidates[1]!.material.providerSecret);
    expect(f.advance).not.toHaveBeenCalled();
  });

  it.each([
    ["aggregate worker", { CRABBOX_MAX_ACTIVE_LEASES: "1" }],
    ["aggregate spend", { CRABBOX_MAX_MONTHLY_USD: "1.5" }],
  ] as const)(
    "leaves no admission residue after post-selection %s rejection",
    async (_label, overrides) => {
      const f = fixture({
        overrides,
        beforePrepare: async (storage, record) => {
          await storage.put("lease:cbx_111111111111", {
            ...record,
            id: "cbx_111111111111",
            slug: "existing-worker",
            state: "active",
            ...candidate(1).lease,
          });
        },
      });
      const response = await f.coordinator.fetch(request());
      expect(response.status).toBe(429);
      expect(await response.json()).toMatchObject({ error: "cost_limit_exceeded" });
      expect(f.selectAdmission).toHaveBeenCalledTimes(1);
      expect(await f.storage.get(`lease:${leaseID}`)).toBeUndefined();
      expect((await f.storage.list({ prefix: "lease:" })).size).toBe(1);
      for (const prefix of [
        "lease-provisioning:",
        "provisioning-plan:",
        "provisioning-material:",
        "provisioning-attempt:",
        "provisioning-due:",
      ]) {
        expect((await f.storage.list({ prefix })).size).toBe(0);
      }
      expect(await f.storage.getAlarm()).toBeNull();
      expect(f.advance).not.toHaveBeenCalled();
    },
  );

  it.each(["id", "owner", "org", "createAttemptGeneration", "state", "maxEstimatedUSD"])(
    "rejects runtime candidate attempts to overwrite %s before admission",
    async (field) => {
      const tampered = candidate(1);
      Object.assign(tampered.lease, { [field]: field === "maxEstimatedUSD" ? 0 : "tampered" });
      const f = fixture({ candidates: [tampered] });
      const response = await f.coordinator.fetch(request());
      expect(response.status).toBe(500);
      expect(await response.json()).toEqual({
        error: "durable provisioning candidate lease target is invalid",
      });
      expect(f.selectAdmission).not.toHaveBeenCalled();
      for (const prefix of [
        "lease:",
        "lease-provisioning:",
        "provisioning-plan:",
        "provisioning-material:",
        "provisioning-attempt:",
        "provisioning-due:",
      ]) {
        expect((await f.storage.list({ prefix })).size).toBe(0);
      }
      expect(f.advance).not.toHaveBeenCalled();
    },
  );
});
