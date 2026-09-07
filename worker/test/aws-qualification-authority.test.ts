import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";

import { afterEach, describe, expect, it, vi } from "vitest";

import controllerWorker from "../../scripts/image-qualification-controller-worker.mjs";
import { qualificationNetwork } from "../../scripts/image-qualification-network.mjs";
import { EC2SpotClient } from "../src/aws";
import {
  AWSQualificationController,
  AWSQualificationRegistry,
  AWSQualificationRun,
  AWSQualificationTransport,
} from "../src/aws-qualification-authority";
import type {
  AWSQualificationControllerProps,
  AWSQualificationFinalReceipt,
  AWSQualificationRequest,
  AWSQualificationRunIdentity,
} from "../src/aws-qualification-contract";
import { leaseConfig } from "../src/config";
import { AWSProvider } from "../src/fleet";
import type { CoordinatorCheckpointRecord, Env } from "../src/types";

const controller: AWSQualificationControllerProps = { deploymentHash: "d".repeat(64) };
const identity: AWSQualificationRunIdentity = {
  runId: "qualification-test-1",
  owner: "maintainer",
  candidateSha: "a".repeat(40),
  candidateWorker: "crabbox-qualification-candidate",
  deploymentHash: controller.deploymentHash,
  expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
};
const authorityConfig = readFileSync(
  new URL("../wrangler.aws-qualification-authority.jsonc", import.meta.url),
  "utf8",
);

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("AWS qualification authority deployment", () => {
  it("admits one immutable executor and consumes its capability before candidate execution", async () => {
    const fixture = authorityFixture();
    const token = "c".repeat(64);
    const tokenDigest = createHash("sha256").update(token).digest("hex");
    await fixture.run.enroll(controller, identity);
    await expect(fixture.run.execute(identity, request("GetCallerIdentity"))).rejects.toThrow(
      "not armed",
    );
    expect(fixture.signer.calls).toHaveLength(0);
    await fixture.run.prepareExecutor(controller, tokenDigest);
    await expect(fixture.run.prepareExecutor(controller, "d".repeat(64))).rejects.toThrow(
      "already bound",
    );
    await expect(
      fixture.run.executorReady(controller, "e".repeat(64), "203.0.113.7"),
    ).rejects.toThrow("capability");
    await expect(fixture.run.executorReady(controller, token, "203.0.113.7")).resolves.toEqual({
      armed: false,
    });
    await expect(fixture.run.executorReady(controller, token, "203.0.113.8")).rejects.toThrow(
      "immutable",
    );
    await expect(fixture.run.armExecution(controller)).rejects.toThrow("not confirmed");
    const network = await fixture.run.prepareNetwork(controller);
    expect(network.securityGroupId).toBe("sg-fixed");
    expect(network.attempt).toBe("1");
    expect(Date.parse(network.cleanupNotAfter) - Date.parse(identity.expiresAt)).toBe(30 * 60_000);
    const dispatch = await fixture.run.dispatchNetwork(controller, network.attemptId!);
    expect(Date.parse(dispatch.dispatchedUntil)).toBeGreaterThan(Date.now());
    await expect(fixture.run.dispatchNetwork(controller, network.attemptId!)).rejects.toThrow(
      "not prepared",
    );
    await fixture.run.confirmNetwork(controller, network.attemptId!, "sgr-1234");
    await expect(
      fixture.run.confirmNetwork(controller, network.attemptId!, "sgr-5678"),
    ).rejects.toThrow("changed");
    await fixture.run.armExecution(controller);
    await expect(fixture.run.executorReady(controller, token, "203.0.113.7")).resolves.toEqual({
      armed: true,
    });
    await expect(fixture.run.executorReady(controller, token, "203.0.113.7")).rejects.toThrow(
      "consumed",
    );
    await expect(
      fixture.run.execute(identity, request("GetCallerIdentity")),
    ).resolves.toMatchObject({ status: 200 });
    const attestation = JSON.stringify(await fixture.run.attest(controller));
    for (const privateValue of [token, tokenDigest, "203.0.113.7", "sg-fixed", "sgr-1234"]) {
      expect(attestation).not.toContain(privateValue);
    }
    expect(new AWSQualificationTransport({} as never, identity)).not.toHaveProperty("armExecution");
    expect(new AWSQualificationTransport({} as never, identity)).not.toHaveProperty(
      "confirmNetworkRevocation",
    );
  });

  it("keeps compute cleanup independent and retains recovery until the exact network intent is cleared", async () => {
    vi.useFakeTimers();
    const fixture = authorityFixture();
    const token = "c".repeat(64);
    await fixture.run.enroll(controller, identity);
    await fixture.run.prepareExecutor(controller, createHash("sha256").update(token).digest("hex"));
    await fixture.run.executorReady(controller, token, "203.0.113.7");
    const network = await fixture.run.prepareNetwork(controller);
    await fixture.run.dispatchNetwork(controller, network.attemptId!);
    await fixture.run.confirmNetwork(controller, network.attemptId!, "sgr-1234");
    await expect(fixture.run.clearNetwork(controller, network.attemptId!)).rejects.toThrow(
      "identity mismatch",
    );
    await Promise.all([
      expect(fixture.run.finalize(controller)).rejects.toThrow("network cleanup incomplete"),
      vi.runAllTimersAsync(),
    ]);
    expect((await fixture.run.attest(controller)).finalized).toBe(false);
    await expect(fixture.run.armExecution(controller)).rejects.toThrow("finalizing");
    await expect(fixture.run.clearNetwork(controller, "f".repeat(64))).rejects.toThrow(
      "identity mismatch",
    );
    await vi.advanceTimersByTimeAsync(60_001);
    await expect(fixture.run.clearNetwork(controller, network.attemptId!)).rejects.toThrow(
      "outcome is unresolved",
    );
    await expect(
      fixture.run.confirmNetworkRevocation(controller, "f".repeat(64), "sgr-1234"),
    ).rejects.toThrow("revocation receipt");
    await expect(
      fixture.run.confirmNetworkRevocation(controller, network.attemptId!, "sgr-9999"),
    ).rejects.toThrow("revocation receipt");
    await fixture.run.confirmNetworkRevocation(controller, network.attemptId!, "sgr-1234");
    const restarted = new AWSQualificationRun(
      { storage: fixture.storage } as never,
      fixture.env as never,
      fixture.signer,
    );
    expect((await restarted.networkStatus(controller)).revokedAt).toBeTruthy();
    await fixture.run.clearNetwork(controller, network.attemptId!);
    const before = fixture.signer.calls.length;
    await fixture.run.finalize(controller);
    expect(fixture.signer.calls).toHaveLength(before);
    expect((await fixture.run.attest(controller)).finalized).toBe(true);
  });

  it("clears a fenced prepared intent that was never dispatched", async () => {
    vi.useFakeTimers();
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    const token = "c".repeat(64);
    await fixture.run.prepareExecutor(controller, createHash("sha256").update(token).digest("hex"));
    await fixture.run.executorReady(controller, token, "203.0.113.7");
    const network = await fixture.run.prepareNetwork(controller);
    await fixture.run.beginFinalization(controller);
    await fixture.run.clearNetwork(controller, network.attemptId!);
    await Promise.all([fixture.run.finalize(controller), vi.runAllTimersAsync()]);
    expect((await fixture.run.attest(controller)).finalized).toBe(true);
  });

  it("cannot clear an unresolved dispatch regardless of age", async () => {
    vi.useFakeTimers();
    const fixture = authorityFixture();
    await fixture.run.enroll(controller, identity);
    const token = "c".repeat(64);
    await fixture.run.prepareExecutor(controller, createHash("sha256").update(token).digest("hex"));
    await fixture.run.executorReady(controller, token, "203.0.113.7");
    const network = await fixture.run.prepareNetwork(controller);
    await fixture.run.dispatchNetwork(controller, network.attemptId!);
    await fixture.run.beginFinalization(controller);
    await vi.advanceTimersByTimeAsync(24 * 60 * 60_000);
    await expect(fixture.run.clearNetwork(controller, network.attemptId!)).rejects.toThrow(
      "outcome is unresolved",
    );
    await expect(
      fixture.run.confirmNetworkRevocation(controller, network.attemptId!, "sgr-1234"),
    ).rejects.toThrow("revocation receipt");
  });

  it("binds the real controller, entrypoint, DO and network helper to final AWS effects", async () => {
    vi.useFakeTimers();
    const fixture = authorityFixture();
    fixture.env.CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID = "sg-12345678";
    const runIdentity = { ...identity, runId: "image-qualification-42-1" };
    const registry = new AWSQualificationRegistry(
      { storage: new MemoryStorage() } as never,
      {} as never,
    );
    const env = {
      AWS_QUALIFICATION_RUNS: {
        idFromName: (name: string) => name,
        get: (name: string) => (name === runIdentity.runId ? fixture.run : authorityFixture().run),
      },
      AWS_QUALIFICATION_REGISTRY: { idFromName: () => "registry", get: () => registry },
    } as never;
    const binding = new AWSQualificationController(env, controller);
    const wrongBinding = new AWSQualificationController(env, { deploymentHash: "e".repeat(64) });
    const controllerToken = "f".repeat(64);
    const rpc = async (
      route: string,
      input: Record<string, unknown>,
      authority = binding,
      token = controllerToken,
    ) => {
      const response = await controllerWorker.fetch(
        new Request(`https://controller.example.workers.dev/${route}`, {
          method: "POST",
          headers: { authorization: `Bearer ${token}` },
          body: JSON.stringify(input),
        }),
        { CONTROLLER_TOKEN: controllerToken, AUTHORITY: authority },
      );
      if (!response.ok) throw new Error(`controller ${response.status}`);
      return await response.json();
    };
    const preflightToken = "c".repeat(64);
    const preflight = async () => {
      const edgeRequest = new Request("https://controller.example.workers.dev/executor", {
        method: "POST",
        headers: { authorization: `Bearer ${preflightToken}`, "cf-connecting-ip": "203.0.113.7" },
        body: JSON.stringify({ runId: runIdentity.runId }),
      });
      Object.defineProperty(edgeRequest, "cf", { value: { colo: "TEST" } });
      return await controllerWorker.fetch(edgeRequest, { AUTHORITY: binding });
    };
    await binding.enroll(runIdentity);
    await rpc("prepare-executor", {
      runId: runIdentity.runId,
      tokenDigest: createHash("sha256").update(preflightToken).digest("hex"),
    });
    expect((await preflight()).status).toBe(200);
    const receipt = await rpc("prepare-network", { runId: runIdentity.runId });
    const effects: string[] = [];
    let revokeInput: Record<string, unknown> | undefined;
    let ruleAtRevoke: string | undefined;
    let rules: Array<Record<string, unknown>> = [];
    const call = async (_service: string, operation: string, input: Record<string, unknown>) => {
      effects.push(operation);
      if (operation === "get-caller-identity") return { Account: "123456789012" };
      if (operation === "describe-security-group-rules")
        return { SecurityGroupRules: structuredClone(rules) };
      if (operation === "authorize-security-group-ingress") {
        const specs = input["TagSpecifications"] as Array<{ Tags: unknown }>;
        rules = [
          {
            SecurityGroupRuleId: "sgr-1234",
            GroupId: input["GroupId"],
            IsEgress: false,
            IpProtocol: "tcp",
            FromPort: 22,
            ToPort: 22,
            CidrIpv4: "203.0.113.7/32",
            Tags: specs[0]!.Tags,
          },
        ];
        return { SecurityGroupRules: structuredClone(rules) };
      }
      if (operation === "revoke-security-group-ingress") {
        revokeInput = input;
        ruleAtRevoke = (await fixture.run.networkStatus(controller)).ruleId;
        rules = [];
        return { Return: true, RevokedSecurityGroupRules: [{ SecurityGroupRuleId: "sgr-1234" }] };
      }
      throw new Error("unexpected AWS call");
    };
    const dispatch = () =>
      rpc("dispatch-network", {
        runId: runIdentity.runId,
        attemptId: receipt.attemptId,
      });
    const confirm = (ruleId: string) =>
      rpc("confirm-network", {
        runId: runIdentity.runId,
        attemptId: receipt.attemptId,
        ruleId,
      });
    const cleanup = {
      remove: true,
      call,
      confirm,
      sleep: async () => {},
      confirmRevocation: (ruleId: string) =>
        rpc("confirm-network-revocation", {
          runId: runIdentity.runId,
          attemptId: receipt.attemptId,
          ruleId,
        }),
    };
    await expect(
      rpc("dispatch-network", { runId: runIdentity.runId }, binding, "wrong"),
    ).rejects.toThrow("403");
    await expect(
      rpc("dispatch-network", { runId: runIdentity.runId }, wrongBinding),
    ).rejects.toThrow("409");
    await expect(rpc("dispatch-network", { runId: "image-qualification-43-1" })).rejects.toThrow(
      "409",
    );
    expect(effects).toEqual([]);
    await expect(
      rpc("dispatch-network", { runId: runIdentity.runId, attemptId: "0".repeat(64) }),
    ).rejects.toThrow("409");
    expect(effects).toEqual([]);
    await qualificationNetwork(receipt, { call, dispatch, confirm, sleep: async () => {} });
    expect(effects.filter((action) => action === "authorize-security-group-ingress")).toHaveLength(
      1,
    );
    await rpc("arm-execution", { runId: runIdentity.runId });
    expect(await (await preflight()).json()).toEqual({ armed: true });
    expect((await preflight()).status).toBe(409);
    await rpc("begin-finalization", { runId: runIdentity.runId });
    await expect(
      qualificationNetwork(receipt, { call, dispatch, confirm, sleep: async () => {} }),
    ).rejects.toThrow("no recorded dispatch");
    expect(effects.filter((action) => action === "authorize-security-group-ingress")).toHaveLength(
      1,
    );
    await vi.advanceTimersByTimeAsync(60_001);
    const pending = await rpc("network", { runId: runIdentity.runId });
    await expect(
      qualificationNetwork(
        { ...pending, ruleId: undefined },
        {
          ...cleanup,
          confirm: (ruleId: string) =>
            rpc("confirm-network", {
              runId: runIdentity.runId,
              attemptId: "0".repeat(64),
              ruleId,
            }),
        },
      ),
    ).rejects.toThrow("409");
    expect(effects).not.toContain("revoke-security-group-ingress");
    await qualificationNetwork(pending, cleanup);
    expect(revokeInput).toEqual({ GroupId: "sg-12345678", SecurityGroupRuleIds: ["sgr-1234"] });
    expect(ruleAtRevoke).toBe("sgr-1234");
    await rpc("clear-network", { runId: runIdentity.runId, attemptId: pending.attemptId });
    expect((await fixture.run.networkStatus(controller)).clearedAt).toBeTruthy();
    expect(effects.filter((action) => action === "revoke-security-group-ingress")).toHaveLength(1);
  });

  it("has no public route, preview URL, workers.dev URL, or cron", () => {
    expect(authorityConfig).toContain('"workers_dev": false');
    expect(authorityConfig).toContain('"preview_urls": false');
    expect(authorityConfig).not.toContain('"routes"');
    expect(authorityConfig).not.toContain('"triggers"');
    expect(authorityConfig).not.toContain('"crons"');
    expect(authorityConfig).toContain('"name": "AWS_QUALIFICATION_REGISTRY"');
    expect(authorityConfig).toContain('"new_sqlite_classes": ["AWSQualificationRegistry"]');
  });

  it("separates the candidate transport from the protected controller entrypoint", async () => {
    const enroll =
      vi.fn<
        (
          controller: AWSQualificationControllerProps,
          identity: AWSQualificationRunIdentity,
        ) => Promise<void>
      >();
    const finalize = vi.fn<(controller: AWSQualificationControllerProps) => Promise<unknown>>();
    const beginFinalization =
      vi.fn<(controller: AWSQualificationControllerProps) => Promise<void>>();
    const attest = vi.fn<(controller: AWSQualificationControllerProps) => Promise<unknown>>();
    attest.mockResolvedValue({ finalized: true });
    const execute =
      vi.fn<
        (
          identity: AWSQualificationRunIdentity,
          request: AWSQualificationRequest,
        ) => Promise<{ status: number; body: string }>
      >();
    execute.mockResolvedValue({ status: 200, body: "ok" });
    const namespace = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<
        (id: string) => {
          enroll: typeof enroll;
          execute: typeof execute;
          beginFinalization: typeof beginFinalization;
          finalize: typeof finalize;
          attest: typeof attest;
        }
      >(() => ({ enroll, execute, beginFinalization, finalize, attest })),
    };
    const claim =
      vi.fn<
        (
          controller: AWSQualificationControllerProps,
          identity: AWSQualificationRunIdentity,
        ) => Promise<unknown>
      >();
    const discover = vi.fn<(controller: AWSQualificationControllerProps) => Promise<unknown>>();
    const markFinalizing =
      vi.fn<(controller: AWSQualificationControllerProps, runId: string) => Promise<unknown>>();
    const markFinalized =
      vi.fn<(controller: AWSQualificationControllerProps, runId: string) => Promise<unknown>>();
    const retire =
      vi.fn<(controller: AWSQualificationControllerProps, runId: string) => Promise<void>>();
    const assertActive = vi
      .fn<(identity: AWSQualificationRunIdentity) => Promise<void>>()
      .mockResolvedValue();
    const registry = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<
        () => {
          assertActive: typeof assertActive;
          claim: typeof claim;
          discover: typeof discover;
          markFinalizing: typeof markFinalizing;
          markFinalized: typeof markFinalized;
          retire: typeof retire;
        }
      >(() => ({ assertActive, claim, discover, markFinalizing, markFinalized, retire })),
    };
    const env = {
      AWS_QUALIFICATION_RUNS: namespace,
      AWS_QUALIFICATION_REGISTRY: registry,
    } as never;
    const candidate = new AWSQualificationTransport(env, identity);
    const protectedController = new AWSQualificationController(env, controller);

    expect("enroll" in candidate).toBe(false);
    expect("beginFinalization" in candidate).toBe(false);
    expect("finalize" in candidate).toBe(false);
    expect("attest" in candidate).toBe(false);
    expect("discover" in candidate).toBe(false);
    expect("claim" in candidate).toBe(false);
    expect("retire" in candidate).toBe(false);
    await candidate.execute(request("GetCallerIdentity"));
    await protectedController.enroll(identity);
    await protectedController.finalize(identity.runId);
    await protectedController.attest(identity.runId);
    await protectedController.discover();
    await protectedController.retire(identity.runId);
    expect(assertActive).toHaveBeenCalledWith(identity);
    expect(execute).toHaveBeenCalledWith(identity, expect.any(Object));
    expect(claim).toHaveBeenCalledWith(controller, identity);
    expect(enroll).toHaveBeenCalledWith(controller, identity);
    expect(enroll.mock.invocationCallOrder[0]).toBeLessThan(claim.mock.invocationCallOrder[0]!);
    expect(beginFinalization).toHaveBeenCalledWith(controller);
    expect(beginFinalization.mock.invocationCallOrder[0]).toBeLessThan(
      markFinalizing.mock.invocationCallOrder[0]!,
    );
    expect(finalize).toHaveBeenCalledWith(controller);
    expect(markFinalizing).toHaveBeenCalledWith(controller, identity.runId);
    expect(markFinalized).toHaveBeenCalledWith(controller, identity.runId);
    expect(attest).toHaveBeenCalledWith(controller);
    expect(discover).toHaveBeenCalledWith(controller);
    expect(retire).toHaveBeenCalledWith(controller, identity.runId);

    assertActive.mockRejectedValueOnce(new Error("AWS qualification registry run is not active"));
    await expect(candidate.execute(request("GetCallerIdentity"))).rejects.toThrow("not active");
    expect(execute).toHaveBeenCalledTimes(1);
  });

  it("fences an admitted candidate before signer dispatch when finalization wins the run hop", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const candidateEntered = Promise.withResolvers<void>();
    const releaseCandidate = Promise.withResolvers<void>();
    const cleanupEntered = Promise.withResolvers<void>();
    const releaseCleanup = Promise.withResolvers<void>();
    const execute = vi.fn<
      (
        runIdentity: AWSQualificationRunIdentity,
        candidateRequest: AWSQualificationRequest,
      ) => Promise<{ status: number; body: string }>
    >(async (runIdentity, candidateRequest) => {
      candidateEntered.resolve();
      await releaseCandidate.promise;
      return await fixture.run.execute(runIdentity, candidateRequest);
    });
    const beginFinalization = vi.fn<
      (controllerProps: AWSQualificationControllerProps) => Promise<void>
    >(async (controllerProps) => {
      await fixture.run.beginFinalization(controllerProps);
    });
    const finalize = vi.fn<(controllerProps: AWSQualificationControllerProps) => Promise<unknown>>(
      async () => {
        cleanupEntered.resolve();
        await releaseCleanup.promise;
        return {};
      },
    );
    const namespace = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<
        () => {
          execute: typeof execute;
          beginFinalization: typeof beginFinalization;
          finalize: typeof finalize;
        }
      >(() => ({ execute, beginFinalization, finalize })),
    };
    const assertActive = vi
      .fn<(runIdentity: AWSQualificationRunIdentity) => Promise<void>>()
      .mockResolvedValue();
    const markFinalizing = vi
      .fn<(controllerProps: AWSQualificationControllerProps, runId: string) => Promise<unknown>>()
      .mockResolvedValue({});
    const markFinalized = vi
      .fn<(controllerProps: AWSQualificationControllerProps, runId: string) => Promise<unknown>>()
      .mockResolvedValue({});
    const registry = {
      idFromName: vi.fn<(name: string) => string>((name) => name),
      get: vi.fn<
        () => {
          assertActive: typeof assertActive;
          markFinalizing: typeof markFinalizing;
          markFinalized: typeof markFinalized;
        }
      >(() => ({ assertActive, markFinalizing, markFinalized })),
    };
    const env = {
      AWS_QUALIFICATION_RUNS: namespace,
      AWS_QUALIFICATION_REGISTRY: registry,
    } as never;
    const candidate = new AWSQualificationTransport(env, identity);
    const protectedController = new AWSQualificationController(env, controller);
    const candidateResult = candidate.execute(
      request(
        "ImportKeyPair",
        {
          KeyName: "candidate-key",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );

    await candidateEntered.promise;
    await protectedController.beginFinalization(identity.runId);
    releaseCandidate.resolve();
    await expect(candidateResult).rejects.toThrow("finalizing");
    expect(fixture.signer.calls).toHaveLength(0);
    const finalization = protectedController.finalize(identity.runId);
    await cleanupEntered.promise;
    releaseCleanup.resolve();
    await finalization;
    expect(beginFinalization.mock.invocationCallOrder[0]).toBeLessThan(
      markFinalizing.mock.invocationCallOrder[0]!,
    );
  });

  it("keeps one idempotent active registry claim until finalized retirement", async () => {
    const storage = new MemoryStorage();
    const registry = new AWSQualificationRegistry({ storage } as never, {} as never);

    await expect(
      registry.claim(controller, {
        ...identity,
        expiresAt: new Date(Date.now() - 1).toISOString(),
      }),
    ).rejects.toThrow("next 120 minutes");
    const first = await registry.claim(controller, identity);
    const repeated = await registry.claim(controller, identity);
    expect(repeated).toEqual(first);
    await expect(registry.assertActive(identity)).resolves.toBeUndefined();
    await expect(
      registry.assertActive({ ...identity, runId: "qualification-test-2" }),
    ).rejects.toThrow("not active");
    await expect(
      registry.claim(controller, { ...identity, runId: "qualification-test-2" }),
    ).rejects.toThrow("already has an active run");
    await expect(registry.retire(controller, identity.runId)).rejects.toThrow("not finalized");
    expect((await registry.markFinalizing(controller, identity.runId)).cleanupState).toBe(
      "finalizing",
    );
    await expect(registry.assertActive(identity)).rejects.toThrow("not active");
    expect((await registry.markFinalized(controller, identity.runId)).cleanupState).toBe(
      "finalized",
    );
    await registry.retire(controller, identity.runId);
    expect(await registry.discover(controller)).toBeUndefined();
    await expect(registry.retire(controller, identity.runId)).resolves.toBeUndefined();
    await expect(registry.assertActive(identity)).rejects.toThrow("not active");
    await expect(registry.claim(controller, identity)).rejects.toThrow("retired");

    const nextIdentity = { ...identity, runId: "qualification-test-2" };
    await registry.claim(controller, nextIdentity);
    await expect(registry.retire(controller, identity.runId)).rejects.toThrow("not active");
    await expect(registry.assertActive(nextIdentity)).resolves.toBeUndefined();
  });
});

describe("AWS qualification authority", () => {
  it("stamps the protected attempt on every resource type and rejects candidate tag overrides", async () => {
    const fixture = authorityFixture();
    await expect(
      fixture.run.enroll(controller, { ...identity, runId: "qualification-without-attempt" }),
    ).rejects.toThrow("must end with its attempt");
    await enrollForCandidate(fixture, identity);
    const spoof = {
      "TagSpecification.1.Tag.1.Key": "crabbox_qualification_attempt",
      "TagSpecification.1.Tag.1.Value": "999",
    };
    await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
          "TagSpecification.1.ResourceType": "key-pair",
          ...spoof,
        },
        "ec2",
      ),
    );
    await fixture.run.execute(
      identity,
      request(
        "RunInstances",
        {
          ...runInstancesParams(),
          "TagSpecification.1.ResourceType": "instance",
          ...spoof,
        },
        "ec2",
      ),
    );
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        {
          InstanceId: "i-owned",
          Name: "qualification-image",
          NoReboot: "true",
          "TagSpecification.1.ResourceType": "image",
          ...spoof,
        },
        "ec2",
      ),
    );
    await fixture.run.execute(
      identity,
      request(
        "CreateTags",
        {
          "ResourceId.1": "i-owned",
          "Tag.1.Key": "crabbox_qualification_attempt",
          "Tag.1.Value": "999",
        },
        "ec2",
      ),
    );
    const types: string[] = [];
    for (const call of fixture.signer.calls) {
      for (const [key, type] of Object.entries(call.parameters)) {
        if (!/^TagSpecification\.[12]\.ResourceType$/.test(key)) continue;
        types.push(String(type));
        const prefix = key.replace("ResourceType", "Tag.");
        const attemptKey = Object.keys(call.parameters).find(
          (name) =>
            name.startsWith(prefix) &&
            name.endsWith(".Key") &&
            call.parameters[name] === "crabbox_qualification_attempt",
        );
        expect(attemptKey).toBeDefined();
        expect(call.parameters[attemptKey!.replace(/Key$/, "Value")]).toBe("1");
      }
    }
    expect(types.toSorted()).toEqual(["image", "instance", "key-pair", "snapshot", "volume"]);
    const tagged = fixture.signer.calls.find((call) => call.action === "CreateTags")!;
    const attemptKey = Object.keys(tagged.parameters).find(
      (name) => tagged.parameters[name] === "crabbox_qualification_attempt",
    )!;
    expect(tagged.parameters[attemptKey.replace(/Key$/, "Value")]).toBe("1");
    const capture = process.env.QUALIFICATION_POLICY_FIXTURE_DIR;
    if (capture) {
      mkdirSync(capture, { recursive: true });
      writeFileSync(
        path.join(capture, "authority-requests.json"),
        JSON.stringify(
          {
            identity,
            requests: fixture.signer.calls,
          },
          null,
          2,
        ),
      );
    }
  });

  it("rejects cross-run identity, policy drift, FSR, and foreign resources", async () => {
    const fixture = authorityFixture();
    await expect(fixture.run.enroll({ deploymentHash: "e".repeat(64) }, identity)).rejects.toThrow(
      "not bound",
    );
    await enrollForCandidate(fixture, identity);

    await expect(
      fixture.run.execute(
        { ...identity, candidateSha: "b".repeat(40) },
        request("GetCallerIdentity"),
      ),
    ).rejects.toThrow("not enrolled");
    await expect(
      fixture.run.execute(identity, request("EnableFastSnapshotRestores", {}, "ec2")),
    ).rejects.toThrow("fast snapshot restore is disabled");
    await expect(
      fixture.run.execute(
        identity,
        request("TerminateInstances", { "InstanceId.1": "i-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
    fixture.env.CRABBOX_AWS_QUALIFICATION_ROOT_GB = "19";
    await expect(fixture.run.execute(identity, request("GetCallerIdentity"))).rejects.toThrow(
      "policy changed",
    );
    await expect(
      authorityFixture().run.enroll(controller, { ...identity, candidateWorker: "bad/worker" }),
    ).rejects.toThrow("candidate Worker is malformed");
    const authorityDrift = authorityFixture();
    await enrollForCandidate(authorityDrift, identity);
    authorityDrift.env.CRABBOX_AWS_QUALIFICATION_AUTHORITY_SHA = "c".repeat(40);
    await expect(
      authorityDrift.run.execute(identity, request("GetCallerIdentity")),
    ).rejects.toThrow("authority deployment changed");
  });

  it("attests persisted operation and finalization evidence without sensitive values", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const denied = request("TerminateInstances", { "InstanceId.1": "i-sensitive-foreign" }, "ec2");
    await expect(fixture.run.execute(identity, denied)).rejects.toThrow("outside the run ledger");
    const smuggledAction = request("https://private.example.invalid", {}, "ec2");
    await expect(fixture.run.execute(identity, smuggledAction)).rejects.toThrow(
      "action is not allowed",
    );
    const imported = request(
      "ImportKeyPair",
      {
        KeyName: "crabbox-cbx-abcdef123456",
        PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification secret-comment"),
      },
      "ec2",
    );
    await fixture.run.execute(identity, imported);
    await fixture.run.finalize(controller);

    const attestation = await fixture.run.attest(controller);
    expect(attestation).toMatchObject({
      version: 1,
      runId: identity.runId,
      candidateSha: identity.candidateSha,
      candidateWorker: identity.candidateWorker,
      deploymentHash: identity.deploymentHash,
      authoritySha: "b".repeat(40),
      authorityVersion: "qualification-v1",
      policyHash: expect.stringMatching(/^[0-9a-f]{64}$/),
      enrolledAt: expect.any(String),
      expiresAt: identity.expiresAt,
      finalized: true,
    });
    expect(attestation.operations).toHaveLength(3);
    expect(
      attestation.operations.find((entry) => entry.action === "TerminateInstances"),
    ).toMatchObject({ denialReason: "resource-not-owned" });
    expect(attestation.operations.find((entry) => entry.action === "ImportKeyPair")).toMatchObject({
      signerDispatches: [
        {
          beforeSequence: expect.any(Number),
          beforeAt: expect.any(String),
          afterSequence: expect.any(Number),
          afterAt: expect.any(String),
          outcome: "accepted",
          statusClass: 2,
        },
      ],
    });
    expect(attestation.operations.find((entry) => entry.action === "DeniedAction")).toMatchObject({
      denialReason: "policy-denied",
    });
    expect(attestation.finalReceipt).toMatchObject({
      resourcesAtStart: { keyPairs: 1 },
      cleanupAttempts: [
        {
          action: "DeleteKeyPair",
          targetDigest: expect.stringMatching(/^[0-9a-f]{64}$/),
          outcome: "accepted",
        },
      ],
      finalCounts: { images: 0, instances: 0, keyPairs: 0, snapshots: 0, volumes: 0 },
      failureCodes: [],
    });
    expect(attestation.finalReceipt?.inventory.length).toBeGreaterThan(0);
    expect(attestation.finalReceipt?.verification).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ action: "GetCallerIdentity", outcome: "accepted" }),
        expect.objectContaining({ action: "DescribeSecurityGroups", outcome: "accepted" }),
      ]),
    );
    const encoded = JSON.stringify(attestation);
    for (const sensitive of [
      denied.opId,
      imported.opId,
      smuggledAction.opId,
      "i-sensitive-foreign",
      "key-owned",
      "123456789012",
      "AAAAqualification",
      "203.0.113.10",
      "https://",
    ]) {
      expect(encoded).not.toContain(sensitive);
    }
  });

  it("rings saturated cleanup evidence without blocking teardown", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    const counts = { images: 0, instances: 0, keyPairs: 1, snapshots: 0, volumes: 0 };
    const startedAt = new Date().toISOString();
    await fixture.storage.put("final-receipt", {
      version: 1,
      startedAt,
      finalizeAttempts: 1,
      resourcesAtStart: counts,
      pendingAtStart: [],
      cleanupAttemptsTotal: 1024,
      cleanupAttemptsTruncated: 0,
      cleanupAttempts: Array.from({ length: 1024 }, (_, index) => ({
        sequence: index + 1,
        action: "DeleteKeyPair",
        targetDigest: "d".repeat(64),
        startedAt,
        completedAt: startedAt,
        outcome: "accepted" as const,
        statusClass: 2,
      })),
      inventoryTotal: 2048,
      inventoryTruncated: 0,
      inventory: Array.from({ length: 2048 }, (_, index) => ({
        sequence: index + 1,
        phase: "pre-cleanup" as const,
        at: startedAt,
        outcome: "accepted" as const,
        counts,
        failureCodes: [],
      })),
      verificationTotal: 1024,
      verificationTruncated: 0,
      verification: Array.from({ length: 1024 }, (_, index) => ({
        sequence: index + 1,
        action: "DescribeKeyPairs",
        at: startedAt,
        outcome: "absent" as const,
      })),
      failureCodes: [],
    } satisfies AWSQualificationFinalReceipt);

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    const receipt = (await fixture.run.attest(controller)).finalReceipt!;
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect(receipt.cleanupAttempts).toHaveLength(1024);
    expect(receipt.cleanupAttemptsTotal).toBeGreaterThan(1024);
    expect(receipt.cleanupAttemptsTruncated).toBeGreaterThan(0);
    expect(receipt.inventory).toHaveLength(2048);
    expect(receipt.inventoryTotal).toBeGreaterThan(2048);
    expect(receipt.inventoryTruncated).toBeGreaterThan(0);
    expect(receipt.verification).toHaveLength(1024);
    expect(receipt.verificationTotal).toBeGreaterThan(1024);
    expect(receipt.verificationTruncated).toBeGreaterThan(0);
    expect(receipt.finalCounts).toEqual({
      images: 0,
      instances: 0,
      keyPairs: 0,
      snapshots: 0,
      volumes: 0,
    });
  });

  it("keeps candidate dispatch fenced after a failed finalization", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    fixture.signer.accountId = "999999999999";

    await expect(fixture.run.finalize(controller)).rejects.toThrow("wrong account");
    const failed = await fixture.run.attest(controller);
    expect(failed).toMatchObject({
      finalizingAt: expect.any(String),
      finalized: false,
    });

    fixture.signer.accountId = "123456789012";
    const callsBeforeRetry = fixture.signer.calls.length;
    await expect(fixture.run.execute(identity, request("GetCallerIdentity"))).rejects.toThrow(
      "finalizing",
    );
    expect(fixture.signer.calls).toHaveLength(callsBeforeRetry);
    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    await expect(fixture.run.enroll(controller, identity)).rejects.toThrow("cannot be re-enrolled");
  });

  it("bounds persisted operation evidence and keeps retries idempotent", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const read = request("GetCallerIdentity");
    await fixture.run.execute(identity, read);
    await fixture.run.execute(identity, read);
    expect((await fixture.run.attest(controller)).operations).toHaveLength(1);

    await Promise.all(
      Array.from({ length: 63 }, (_, index) =>
        fixture.storage.put(`evidence:seed-${index}`, {
          version: 1,
          opDigest: `${index}`.padStart(64, "0"),
          requestDigest: "c".repeat(64),
          action: "seed",
          requestedAt: new Date().toISOString(),
          signerDispatches: [],
        }),
      ),
    );
    await expect(fixture.run.execute(identity, request("GetCallerIdentity"))).rejects.toThrow(
      "evidence limit",
    );
  });

  it("accepts the real EC2SpotClient key contract and owns only the verified physical key", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const client = new EC2SpotClient(
      {
        CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
          execute: (value) => fixture.run.execute(identity, value),
        },
      } as Env,
      "us-east-1",
    ) as unknown as {
      ensureSSHKey(name: string, publicKey: string, leaseID: string): Promise<void>;
    };

    await client.ensureSSHKey(
      "crabbox-cbx-abcdef123456",
      "ssh-ed25519 AAAAqualification reviewer",
      "cbx_abcdef123456",
    );

    const imported = fixture.signer.calls.find((call) => call.action === "ImportKeyPair");
    expect(imported?.parameters["PublicKeyMaterial"]).toBe(
      btoa("ssh-ed25519 AAAAqualification reviewer"),
    );
    expect(imported?.parameters["KeyName"]).toMatch(/^cbxq-[0-9a-f]{32}$/);
    expect(imported?.parameters["KeyName"]).not.toBe("crabbox-cbx-abcdef123456");
    expect(fixture.signer.calls.filter((call) => call.action === "DescribeKeyPairs")).toHaveLength(
      2,
    );
  });

  it("serializes concurrent launches and enforces three confirmed sequential launches", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);

    const outcomes = await Promise.allSettled([
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ]);
    expect(outcomes.filter((result) => result.status === "fulfilled")).toHaveLength(1);
    expect(outcomes.filter((result) => result.status === "rejected")).toHaveLength(1);
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(1);

    await terminateOwned(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await terminateOwned(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await terminateOwned(fixture);
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("launch budget");
  });

  it("runs the real public stop path through source, candidate, and promoted image order", async () => {
    vi.stubGlobal("setTimeout", ((callback: () => void) => {
      queueMicrotask(callback);
      return 0;
    }) as typeof setTimeout);
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const client = new EC2SpotClient(env, "us-east-1") as unknown as {
      createServer(
        config: ReturnType<typeof leaseConfig>,
        leaseID: string,
        slug: string,
        owner: string,
        imageID: string,
        securityGroupID: string,
      ): Promise<{ cloudID: string }>;
      ensureSSHKey(name: string, publicKey: string, leaseID: string): Promise<void>;
    };
    const provider = new AWSProvider(env, "us-east-1", {} as never);
    const config = {
      ...leaseConfig({
        provider: "aws",
        target: "linux",
        serverType: "t3.small",
        serverTypeExplicit: true,
        capacity: { market: "on-demand" },
        providerKey: "crabbox-cbx-abcdef123456",
        sshPublicKey: "ssh-ed25519 AAAAqualification",
      }),
      awsProfile: "",
      awsRootGB: 20,
      awsSubnetID: "subnet-fixed",
    };
    await client.ensureSSHKey(config.providerKey, config.sshPublicKey, "cbx_abcdef123456");

    const source = await client.createServer(
      config,
      "cbx_source000001",
      "qualification-source",
      "maintainer",
      "ami-base",
      "sg-fixed",
    );
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: source.cloudID, Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await provider.deleteServer(source.cloudID);
    const candidate = await client.createServer(
      config,
      "cbx_candidate001",
      "qualification-candidate",
      "maintainer",
      "ami-created",
      "sg-fixed",
    );
    await provider.deleteServer(candidate.cloudID);
    const promoted = await client.createServer(
      config,
      "cbx_promoted0001",
      "qualification-promoted",
      "maintainer",
      "ami-created",
      "sg-fixed",
    );
    await provider.deleteServer(promoted.cloudID);

    expect(
      fixture.signer.calls
        .filter((call) => call.action === "RunInstances" || call.action === "CreateImage")
        .map((call) =>
          call.action === "RunInstances"
            ? `RunInstances:${call.parameters["ImageId"]}`
            : call.action,
        ),
    ).toEqual([
      "RunInstances:ami-base",
      "CreateImage",
      "RunInstances:ami-created",
      "RunInstances:ami-created",
    ]);
    expect(
      fixture.signer.calls.filter((call) => call.action === "TerminateInstances"),
    ).toHaveLength(3);
  });

  it("reconciles an ambiguous launch with a run-and-op scoped client token", async () => {
    const fixture = authorityFixture({ failOnce: "RunInstances" });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    const launch = request("RunInstances", runInstancesParams(), "ec2");

    await expect(fixture.run.execute(identity, launch)).rejects.toThrow("lost response");
    await expect(fixture.run.execute(identity, launch)).resolves.toMatchObject({ status: 200 });
    const calls = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(calls).toHaveLength(2);
    expect(calls[0]!.parameters["ClientToken"]).toBe(calls[1]!.parameters["ClientToken"]);
    expect(String(calls[0]!.parameters["ClientToken"])).toMatch(/^cbxq-[0-9a-f]{59}$/);
    await fixture.run.execute(identity, launch);
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(2);
  });

  it("never redispatches a no-effect pending launch after expiry", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ failOnce: "RunInstances" });
    await enrollForCandidate(fixture, expiring);
    await fixture.run.execute(
      expiring,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    await expect(
      fixture.run.execute(expiring, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("lost response");
    const launchCalls = fixture.signer.calls.filter((call) => call.action === "RunInstances");
    expect(launchCalls).toHaveLength(1);
    const callsBeforeExpiry = fixture.signer.calls.length;

    vi.setSystemTime(new Date(now.getTime() + 2_000));
    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(1);
    expect(
      fixture.signer.calls
        .slice(callsBeforeExpiry)
        .filter((call) => call.action === "RunInstances"),
    ).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("drops an undispatched generic mutation when account verification crosses expiry", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, expiring);
    await importKey(fixture, expiring);
    fixture.signer.advanceNextIdentityByMs = 2_000;

    await expect(
      fixture.run.execute(
        expiring,
        request(
          "CreateTags",
          {
            "ResourceId.1": "key-owned",
            "Tag.1.Key": "Name",
            "Tag.1.Value": "expired-candidate-mutation",
          },
          "ec2",
        ),
      ),
    ).rejects.toThrow("expired before mutation dispatch");
    expect(fixture.signer.calls.filter((call) => call.action === "CreateTags")).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("does not recover a prepared mutation that never reached the signer", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    const prepared = request(
      "CreateTags",
      {
        "ResourceId.1": "key-owned",
        "Tag.1.Key": "Name",
        "Tag.1.Value": "never-dispatched",
      },
      "ec2",
    );
    await fixture.storage.put(`intent:${prepared.opId}`, {
      phase: "prepared",
      requestHash: "prepared-request",
      request: prepared,
      startedAt: new Date().toISOString(),
    });

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.filter((call) => call.action === "CreateTags")).toHaveLength(0);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("does not redispatch an expired dispatched generic mutation during finalization", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, expiring);
    await importKey(fixture, expiring);
    const dispatched = request(
      "CreateTags",
      {
        "ResourceId.1": "key-owned",
        "Tag.1.Key": "Name",
        "Tag.1.Value": "crash-before-signer",
      },
      "ec2",
    );
    await fixture.storage.put(`intent:${dispatched.opId}`, {
      phase: "dispatched",
      requestHash: "dispatched-request",
      request: dispatched,
      startedAt: now.toISOString(),
    });
    vi.setSystemTime(new Date(now.getTime() + 2_000));

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.filter((call) => call.action === "CreateTags")).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("discovers and cleans a lost-response launch after expiry without redispatch", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-04T00:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ loseAfterEffect: "RunInstances" });
    await enrollForCandidate(fixture, expiring);
    await fixture.run.execute(
      expiring,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    await expect(
      fixture.run.execute(expiring, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("lost response");
    const callsBeforeExpiry = fixture.signer.calls.length;

    vi.setSystemTime(new Date(now.getTime() + 2_000));
    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(
      fixture.signer.calls
        .slice(callsBeforeExpiry)
        .filter((call) => call.action === "RunInstances"),
    ).toHaveLength(0);
    expect(fixture.signer.calls.some((call) => call.action === "TerminateInstances")).toBe(true);
  });

  it("captures the CreateImage child snapshot and permits only one active checkpoint set", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );

    await fixture.run.execute(identity, create);
    await expect(
      fixture.run.execute(identity, { ...create, opId: crypto.randomUUID() }),
    ).rejects.toThrow("one active checkpoint");
    const ledger = await fixture.storage.get<{
      imageIds: string[];
      snapshotIds: string[];
    }>("ledger");
    expect(ledger).toMatchObject({ imageIds: ["ami-created"], snapshotIds: ["snap-child"] });
  });

  it("keeps bounded deletion tombstones for provider verification and retry", async () => {
    const fixture = authorityFixture({ deleteNotFound: "DeregisterImage" });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const provider = new AWSProvider(env, "us-east-1", fixture.storage as never);
    const checkpoint = {
      leaseID: "cbx_source000001",
      name: "qualification-image",
      scope: { accountID: "123456789012", region: "us-east-1" },
      image: {
        id: "ami-created",
        resourceID: "ami-created",
        kind: "aws-ami",
        immutableID: "ami-created",
        snapshotIDs: ["snap-child"],
        state: "available",
      },
    } as CoordinatorCheckpointRecord;

    await expect(provider.deleteCheckpointImage(checkpoint)).resolves.toBeUndefined();
    const ledger = await fixture.storage.get<{
      imageIds: string[];
      retiredImageIds: string[];
      retiredSnapshotIds: string[];
      snapshotIds: string[];
    }>("ledger");
    expect(ledger).toMatchObject({
      imageIds: [],
      retiredImageIds: ["ami-created"],
      retiredSnapshotIds: ["snap-child"],
      snapshotIds: [],
    });
    await expect(
      fixture.run.execute(identity, request("DeregisterImage", { ImageId: "ami-created" }, "ec2")),
    ).resolves.toMatchObject({ status: 400 });
    expect(fixture.signer.calls.filter((call) => call.action === "DeregisterImage")).toHaveLength(
      2,
    );
    await expect(
      fixture.run.execute(identity, request("DeleteKeyPair", { KeyPairId: "key-owned" }, "ec2")),
    ).resolves.toMatchObject({ status: 200 });
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeKeyPairs", { "KeyPairId.1": "key-owned" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 400 });
    await expect(
      fixture.run.execute(identity, request("DeleteKeyPair", { KeyPairId: "key-owned" }, "ec2")),
    ).resolves.toMatchObject({ status: 200 });
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeImages", { "ImageId.1": "ami-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
  });

  it("allows only the fixed base AMI or an active run-derived AMI", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await terminateOwned(fixture);

    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-foreign" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-created" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 200 });
    await terminateOwned(fixture);
    await fixture.run.execute(
      identity,
      request("DeregisterImage", { ImageId: "ami-created" }, "ec2"),
    );
    await fixture.run.execute(
      identity,
      request("DeleteSnapshot", { SnapshotId: "snap-child" }, "ec2"),
    );
    await expect(
      fixture.run.execute(
        identity,
        request("RunInstances", { ...runInstancesParams(), ImageId: "ami-created" }, "ec2"),
      ),
    ).rejects.toThrow("outside the run ledger");
  });

  it("reconciles lost and delayed key creation without redispatching ImportKeyPair", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedKeyVisibility: 2,
      loseAfterEffect: "ImportKeyPair",
    });
    await enrollForCandidate(fixture, identity);
    const keyRequest = request(
      "ImportKeyPair",
      {
        KeyName: "crabbox-cbx-abcdef123456",
        PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
      },
      "ec2",
    );
    const imported = fixture.run.execute(identity, keyRequest);
    await expect(imported).rejects.toThrow("lost response");

    const replay = fixture.run.execute(identity, keyRequest);
    await expect(replay).resolves.toMatchObject({ status: 200 });
    expect(fixture.signer.calls.filter((call) => call.action === "ImportKeyPair")).toHaveLength(1);
  });

  it("reconciles a 5xx and delayed image without redispatching CreateImage", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedImageVisibility: 3,
      http500AfterEffect: "CreateImage",
    });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );
    await expect(fixture.run.execute(identity, create)).rejects.toThrow("response is ambiguous");

    const replay = fixture.run.execute(identity, create);
    await expect(replay).resolves.toMatchObject({ status: 200 });
    expect(fixture.signer.calls.filter((call) => call.action === "CreateImage")).toHaveLength(1);
  });

  it("retires a no-effect ImportKeyPair intent after authoritative absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ failOnce: "ImportKeyPair" });
    await enrollForCandidate(fixture, identity);
    await expect(importKey(fixture)).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(false);
  });

  it("retires a no-effect CreateImage intent after authoritative cleanup", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ failOnce: "CreateImage" });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await expect(
      fixture.run.execute(
        identity,
        request(
          "CreateImage",
          { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.some((call) => call.action === "DeregisterImage")).toBe(false);
  });

  it("cleans up an image that becomes visible only during final inventory", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedImageVisibility: 8,
      loseAfterEffect: "CreateImage",
    });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await expect(
      fixture.run.execute(
        identity,
        request(
          "CreateImage",
          { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.some((call) => call.action === "DeregisterImage")).toBe(true);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteSnapshot")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("cleans up a key pair that becomes visible only during final inventory", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      delayedKeyVisibility: 8,
      loseAfterEffect: "ImportKeyPair",
    });
    await enrollForCandidate(fixture, identity);
    await expect(importKey(fixture)).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(true);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("retires a pending snapshot deletion after recovery confirms absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      deleteNotFound: "DeleteSnapshot",
      loseAfterEffect: "DeleteSnapshot",
    });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request(
        "CreateImage",
        { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
        "ec2",
      ),
    );
    await fixture.run.execute(
      identity,
      request("DeregisterImage", { ImageId: "ami-created" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("DeleteSnapshot", { SnapshotId: "snap-child" }, "ec2")),
    ).rejects.toThrow("lost response");

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
    expect(fixture.signer.calls.filter((call) => call.action === "DeleteSnapshot").length).toBe(2);
  });

  it("revalidates the expected STS account before every mutation", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    fixture.signer.accountId = "999999999999";

    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("wrong account");
    expect(fixture.signer.calls.filter((call) => call.action === "RunInstances")).toHaveLength(0);
  });

  it("revalidates the expected STS account before finalization and reconciliation", async () => {
    useImmediateTimeouts();
    const finalizeFixture = authorityFixture();
    await enrollForCandidate(finalizeFixture, identity);
    await importKey(finalizeFixture);
    finalizeFixture.signer.accountId = "999999999999";
    await expect(finalizeFixture.run.finalize(controller)).rejects.toThrow("wrong account");
    expect(
      finalizeFixture.signer.calls.filter((call) => call.action === "DescribeInstances"),
    ).toHaveLength(0);

    const reconcileFixture = authorityFixture({ loseAfterEffect: "CreateImage" });
    await enrollForCandidate(reconcileFixture, identity);
    await importKey(reconcileFixture);
    await reconcileFixture.run.execute(
      identity,
      request("RunInstances", runInstancesParams(), "ec2"),
    );
    const create = request(
      "CreateImage",
      { InstanceId: "i-owned", Name: "qualification-image", NoReboot: "true" },
      "ec2",
    );
    await expect(reconcileFixture.run.execute(identity, create)).rejects.toThrow("lost response");
    reconcileFixture.signer.accountId = "999999999999";
    await expect(reconcileFixture.run.execute(identity, create)).rejects.toThrow("wrong account");
    expect(
      reconcileFixture.signer.calls.filter((call) => call.action === "DescribeImages"),
    ).toHaveLength(0);
  });

  it("retains active instance ownership until Describe confirms termination", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );

    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
    await fixture.run.execute(
      identity,
      request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
    await fixture.run.execute(
      identity,
      request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).resolves.toMatchObject({ status: 200 });
  });

  it("retires an acknowledged missing instance before the next public launch", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    fixture.signer.instanceDescribeNotFound = true;
    await expect(
      fixture.run.execute(
        identity,
        request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
      ),
    ).resolves.toMatchObject({ status: 400 });
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");

    const env = {
      CRABBOX_AWS_QUALIFICATION_TRANSPORT: {
        execute: (value: AWSQualificationRequest) => fixture.run.execute(identity, value),
      },
    } as Env;
    const provider = new AWSProvider(env, "us-east-1", {} as never);
    await provider.deleteServer("i-owned");
    expect(
      await fixture.storage.get<{
        instanceIds: string[];
        retiredInstanceIds: string[];
        terminatingInstanceIds: string[];
      }>("ledger"),
    ).toMatchObject({
      instanceIds: [],
      retiredInstanceIds: ["i-owned"],
      terminatingInstanceIds: [],
    });
    fixture.signer.instanceDescribeNotFound = false;
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).resolves.toMatchObject({ status: 200 });
  });

  it("confirms mixed DescribeInstances absence one instance at a time", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));
    await fixture.run.execute(
      identity,
      request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
    );
    const ledger = await fixture.storage.get<{
      instanceIds: string[];
      retiredInstanceIds: string[];
      terminatingInstanceIds: string[];
    }>("ledger");
    expect(ledger).toBeDefined();
    ledger!.retiredInstanceIds = ["i-retired"];
    await fixture.storage.put("ledger", ledger);

    await expect(
      fixture.run.execute(
        identity,
        request(
          "DescribeInstances",
          { "InstanceId.1": "i-retired", "InstanceId.2": "i-owned" },
          "ec2",
        ),
      ),
    ).resolves.toMatchObject({ status: 400 });
    expect(
      await fixture.storage.get<{
        instanceIds: string[];
        retiredInstanceIds: string[];
        terminatingInstanceIds: string[];
      }>("ledger"),
    ).toMatchObject({
      instanceIds: ["i-owned"],
      retiredInstanceIds: ["i-retired"],
      terminatingInstanceIds: ["i-owned"],
    });
    await expect(
      fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2")),
    ).rejects.toThrow("one active instance");
  });

  it("recovers a lost termination only after repeated per-instance absence", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({
      loseAfterEffect: "TerminateInstances",
      terminationDescribeNotFound: true,
    });
    await enrollForCandidate(fixture, identity);
    await importKey(fixture);
    await fixture.run.execute(identity, request("RunInstances", runInstancesParams(), "ec2"));

    await expect(
      fixture.run.execute(
        identity,
        request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
      ),
    ).rejects.toThrow("lost response");
    expect(
      await fixture.storage.get<{ instanceIds: string[]; terminatingInstanceIds: string[] }>(
        "ledger",
      ),
    ).toMatchObject({ instanceIds: ["i-owned"], terminatingInstanceIds: [] });

    await expect(fixture.run.finalize(controller)).resolves.toBeDefined();
    const terminationCalls = fixture.signer.calls
      .map((call, index) => ({ ...call, index }))
      .filter((call) => call.action === "TerminateInstances");
    const individualConfirmations = fixture.signer.calls
      .map((call, index) => ({ ...call, index }))
      .filter(
        (call) =>
          call.action === "DescribeInstances" && call.parameters["InstanceId.1"] === "i-owned",
      );
    expect(terminationCalls).toHaveLength(2);
    expect(individualConfirmations.length).toBeGreaterThanOrEqual(2);
    expect(individualConfirmations[1]!.index).toBeLessThan(terminationCalls[1]!.index);
    expect((await fixture.storage.list({ prefix: "intent:" })).size).toBe(0);
  });

  it("never adopts or deletes a duplicate foreign physical key", async () => {
    useImmediateTimeouts();
    const fixture = authorityFixture({ duplicateForeignKey: true });
    await enrollForCandidate(fixture, identity);
    const result = await fixture.run.execute(
      identity,
      request(
        "ImportKeyPair",
        {
          KeyName: "crabbox-cbx-abcdef123456",
          PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
        },
        "ec2",
      ),
    );
    expect(result.status).toBe(400);

    await fixture.run.finalize(controller);
    expect(fixture.signer.calls.some((call) => call.action === "DeleteKeyPair")).toBe(false);
  });

  it("expires into cleanup and a final tag inventory catches unknown resources", async () => {
    vi.useFakeTimers();
    useImmediateTimeouts();
    const now = new Date("2026-09-03T12:00:00Z");
    vi.setSystemTime(now);
    const expiring = { ...identity, expiresAt: new Date(now.getTime() + 1_000).toISOString() };
    const fixture = authorityFixture({ unknownTaggedInstance: true, unknownVisibilityDelay: 2 });
    await enrollForCandidate(fixture, expiring);
    vi.setSystemTime(new Date(now.getTime() + 2_000));

    await expect(fixture.run.execute(expiring, request("GetCallerIdentity"))).rejects.toThrow(
      "expired",
    );
    expect(
      fixture.signer.calls.some(
        (call) =>
          call.action === "TerminateInstances" && call.parameters["InstanceId.1"] === "i-unknown",
      ),
    ).toBe(true);
  });

  it("requires one parsed security group from a successful response", async () => {
    useImmediateTimeouts();
    const errorFixture = authorityFixture({ securityGroupErrorWithId: true });
    await enrollForCandidate(errorFixture, identity);

    await expect(errorFixture.run.finalize(controller)).rejects.toThrow(
      "DescribeSecurityGroups verification http 403",
    );

    const duplicateFixture = authorityFixture({ duplicateSecurityGroup: true });
    await enrollForCandidate(duplicateFixture, identity);
    await expect(duplicateFixture.run.finalize(controller)).rejects.toThrow(
      "preprovisioned security group is missing",
    );
  });

  it("rejects oversized and privileged candidate payloads before AWS", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    await expect(
      fixture.run.execute(identity, request("GetCallerIdentity", { value: "x".repeat(70 * 1024) })),
    ).rejects.toThrow("64 KiB");
    await importKey(fixture);
    await expect(
      fixture.run.execute(
        identity,
        request(
          "RunInstances",
          { ...runInstancesParams(), "IamInstanceProfile.Name": "admin" },
          "ec2",
        ),
      ),
    ).rejects.toThrow("forbidden");
  });

  it("bounds the complete envelope and rejects unknown, cyclic, and nested input", async () => {
    const fixture = authorityFixture();
    await enrollForCandidate(fixture, identity);
    const cyclic: Record<string, unknown> = {};
    cyclic["self"] = cyclic;
    await expect(
      fixture.run.execute(identity, request("GetCallerIdentity", cyclic)),
    ).rejects.toThrow("contains a cycle");
    await expect(
      fixture.run.execute(
        identity,
        request("GetCallerIdentity", { nested: { deeper: { value: "nope" } } }),
      ),
    ).rejects.toThrow("nesting exceeds policy");
    await expect(
      fixture.run.execute(identity, {
        ...request("GetCallerIdentity"),
        unexpected: "nope",
      } as AWSQualificationRequest),
    ).rejects.toThrow("unknown field");
    await expect(
      fixture.run.execute(identity, {
        ...request("GetCallerIdentity"),
        unexpected: "x".repeat(70 * 1024),
      } as AWSQualificationRequest),
    ).rejects.toThrow("64 KiB");
    expect(fixture.signer.calls).toHaveLength(0);
  });
});

describe("AWS qualification candidate transport", () => {
  it("fails closed without falling back to stray raw credentials", async () => {
    const execute = vi.fn<(request: AWSQualificationRequest) => Promise<never>>();
    execute.mockRejectedValue(new Error("binding unavailable"));
    const client = new EC2SpotClient(
      {
        CRABBOX_AWS_QUALIFICATION_TRANSPORT: { execute },
        AWS_ACCESS_KEY_ID: "must-not-be-used",
        AWS_SECRET_ACCESS_KEY: "must-not-be-used",
      } as Env,
      "us-east-1",
    );

    await expect(client.identity()).rejects.toThrow("binding unavailable");
    expect(execute).toHaveBeenCalledTimes(2);
  });
});

function authorityFixture(
  options: {
    delayedImageVisibility?: number;
    delayedKeyVisibility?: number;
    deleteNotFound?: string;
    duplicateForeignKey?: boolean;
    duplicateSecurityGroup?: boolean;
    failOnce?: string;
    http500AfterEffect?: string;
    loseAfterEffect?: string;
    securityGroupErrorWithId?: boolean;
    terminationDescribeNotFound?: boolean;
    unknownTaggedInstance?: boolean;
    unknownVisibilityDelay?: number;
  } = {},
) {
  const storage = new MemoryStorage();
  const signer = new FakeSigner(options);
  const env = {
    CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID: "123456789012",
    CRABBOX_AWS_QUALIFICATION_AUTHORITY_SHA: "b".repeat(40),
    CRABBOX_AWS_QUALIFICATION_AUTHORITY_VERSION: "qualification-v1",
    CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID: "ami-base",
    CRABBOX_AWS_QUALIFICATION_REGION: "us-east-1",
    CRABBOX_AWS_QUALIFICATION_ROOT_GB: "20",
    CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID: "sg-fixed",
    CRABBOX_AWS_QUALIFICATION_SUBNET_ID: "subnet-fixed",
  };
  const run = new AWSQualificationRun({ storage } as never, env as never, signer);
  return { env, run, signer, storage };
}

async function enrollForCandidate(
  fixture: ReturnType<typeof authorityFixture>,
  runIdentity: AWSQualificationRunIdentity,
): Promise<void> {
  await fixture.run.enroll(controller, runIdentity);
  // Signer lifecycle tests start after protected admission. The real admission/cleanup
  // state machine is exercised above; this fixture deliberately owns no network rule.
  const state = await fixture.storage.get<Record<string, unknown>>("run");
  await fixture.storage.put("run", {
    ...state,
    executor: { tokenDigest: "c".repeat(64), armedAt: new Date().toISOString() },
  });
}

async function importKey(
  fixture: ReturnType<typeof authorityFixture>,
  runIdentity: AWSQualificationRunIdentity = identity,
): Promise<void> {
  await fixture.run.execute(
    runIdentity,
    request(
      "ImportKeyPair",
      {
        KeyName: "crabbox-cbx-abcdef123456",
        PublicKeyMaterial: btoa("ssh-ed25519 AAAAqualification"),
      },
      "ec2",
    ),
  );
}

async function terminateOwned(fixture: ReturnType<typeof authorityFixture>): Promise<void> {
  await fixture.run.execute(
    identity,
    request("TerminateInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
  await fixture.run.execute(
    identity,
    request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
  await fixture.run.execute(
    identity,
    request("DescribeInstances", { "InstanceId.1": "i-owned" }, "ec2"),
  );
}

function useImmediateTimeouts(): void {
  vi.stubGlobal("setTimeout", ((callback: () => void) => {
    queueMicrotask(callback);
    return 0;
  }) as typeof setTimeout);
}

function request(
  action: string,
  parameters: Record<string, unknown> = {},
  service: AWSQualificationRequest["service"] = "sts",
): AWSQualificationRequest {
  return {
    opId: crypto.randomUUID(),
    region: "us-east-1",
    service,
    action,
    parameters,
  };
}

function runInstancesParams(): Record<string, unknown> {
  return {
    ImageId: "ami-base",
    InstanceType: "t3.small",
    KeyName: "crabbox-cbx-abcdef123456",
    MaxCount: "1",
    MinCount: "1",
    UserData: "IyEvYmluL3NoCg==",
    "BlockDeviceMapping.1.DeviceName": "/dev/sda1",
    "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
    "BlockDeviceMapping.1.Ebs.Encrypted": "true",
    "BlockDeviceMapping.1.Ebs.VolumeSize": "20",
    "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
    "NetworkInterface.1.AssociatePublicIpAddress": "true",
    "NetworkInterface.1.DeleteOnTermination": "true",
    "NetworkInterface.1.DeviceIndex": "0",
    "NetworkInterface.1.SecurityGroupId.1": "sg-fixed",
    "NetworkInterface.1.SubnetId": "subnet-fixed",
  };
}

class FakeSigner {
  readonly calls: Array<{
    service: string;
    action: string;
    region: string;
    parameters: Record<string, unknown>;
  }> = [];
  accountId = "123456789012";
  advanceNextIdentityByMs = 0;
  instanceDescribeNotFound = false;
  private failed = false;
  private imageDescribeCalls = 0;
  private key?: { id: string; name: string; publicKey: string; runId?: string; sha?: string };
  private keyDescribeCalls = 0;
  private instanceOp = "";
  private instanceState: "absent" | "running" | "shutting-down" | "terminated" = "absent";
  private terminationDescribeCalls = 0;
  private terminationMissingCalls = 0;
  private imageActive = false;
  private snapshotActive = false;
  private unknownActive: boolean;
  private unknownDescribeCalls = 0;

  constructor(
    private readonly options: {
      delayedImageVisibility?: number;
      delayedKeyVisibility?: number;
      deleteNotFound?: string;
      duplicateForeignKey?: boolean;
      duplicateSecurityGroup?: boolean;
      failOnce?: string;
      http500AfterEffect?: string;
      loseAfterEffect?: string;
      securityGroupErrorWithId?: boolean;
      terminationDescribeNotFound?: boolean;
      unknownTaggedInstance?: boolean;
      unknownVisibilityDelay?: number;
    },
  ) {
    this.unknownActive = Boolean(options.unknownTaggedInstance);
  }

  async execute(
    service: string,
    action: string,
    region: string,
    parameters: Record<string, unknown>,
  ): Promise<Response> {
    this.calls.push({ service, action, region, parameters: structuredClone(parameters) });
    if (action === this.options.failOnce && !this.failed) {
      this.failed = true;
      throw new Error("lost response");
    }
    if (action === "GetCallerIdentity") {
      if (this.advanceNextIdentityByMs > 0) {
        const advanceBy = this.advanceNextIdentityByMs;
        this.advanceNextIdentityByMs = 0;
        vi.setSystemTime(new Date(Date.now() + advanceBy));
      }
      return xml(
        `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>${this.accountId}</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`,
      );
    }
    if (action === "DescribeKeyPairs") {
      this.keyDescribeCalls += 1;
      const visibilityDelayed = this.keyDescribeCalls <= (this.options.delayedKeyVisibility ?? 0);
      if (parameters["Filter.1.Name"]) {
        const tagged = !visibilityDelayed && this.key?.runId ? keyXML(this.key) : "";
        return xml(
          `<DescribeKeyPairsResponse><keySet>${tagged}</keySet></DescribeKeyPairsResponse>`,
        );
      }
      if (!this.key) return awsError("InvalidKeyPair.NotFound");
      if (visibilityDelayed) {
        return awsError("InvalidKeyPair.NotFound");
      }
      return xml(
        `<DescribeKeyPairsResponse><keySet>${keyXML(this.key)}</keySet></DescribeKeyPairsResponse>`,
      );
    }
    if (action === "ImportKeyPair") {
      if (this.options.duplicateForeignKey) {
        this.key = {
          id: "key-foreign",
          name: String(parameters["KeyName"]),
          publicKey: "ssh-ed25519 AAAAforeign",
        };
        return awsError("InvalidKeyPair.Duplicate");
      }
      this.key = {
        id: "key-owned",
        name: String(parameters["KeyName"]),
        publicKey: atob(String(parameters["PublicKeyMaterial"])),
        runId: tagValue(parameters, "crabbox_qualification_run"),
        sha: tagValue(parameters, "crabbox_qualification_sha"),
      };
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.http500AfterEffect === action && !this.failed) {
        this.failed = true;
        return awsServerError();
      }
      return xml(
        `<ImportKeyPairResponse><keyPairId>${this.key.id}</keyPairId><keyName>${this.key.name}</keyName></ImportKeyPairResponse>`,
      );
    }
    if (action === "RunInstances") {
      this.instanceState = "running";
      this.instanceOp = tagValue(parameters, "crabbox_qualification_op");
      this.terminationDescribeCalls = 0;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      return xml(
        "<RunInstancesResponse><instancesSet><item><instanceId>i-owned</instanceId><instanceType>t3.small</instanceType><ipAddress>203.0.113.10</ipAddress><instanceState><name>running</name></instanceState><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item></instancesSet></RunInstancesResponse>",
      );
    }
    if (action === "TerminateInstances") {
      if (parameters["InstanceId.1"] === "i-unknown") this.unknownActive = false;
      else this.instanceState = "shutting-down";
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      return xml(
        `<TerminateInstancesResponse><instancesSet><item><instanceId>${parameters["InstanceId.1"]}</instanceId><currentState><name>shutting-down</name></currentState></item></instancesSet></TerminateInstancesResponse>`,
      );
    }
    if (action === "CreateImage") {
      this.imageActive = true;
      this.snapshotActive = true;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.http500AfterEffect === action && !this.failed) {
        this.failed = true;
        return awsServerError();
      }
      return xml("<CreateImageResponse><imageId>ami-created</imageId></CreateImageResponse>");
    }
    if (action === "DescribeImages") {
      this.imageDescribeCalls += 1;
      const visible =
        this.imageActive && this.imageDescribeCalls > (this.options.delayedImageVisibility ?? 0);
      return xml(
        `<DescribeImagesResponse><imagesSet>${visible ? `<item><imageId>ami-created</imageId><tagSet><item><key>crabbox_qualification_run</key><value>${identity.runId}</value></item><item><key>crabbox_qualification_op</key><value>${imageOp(this.calls)}</value></item></tagSet><blockDeviceMapping><item><ebs><snapshotId>snap-child</snapshotId></ebs></item></blockDeviceMapping></item>` : ""}</imagesSet></DescribeImagesResponse>`,
      );
    }
    if (action === "DeregisterImage") {
      this.imageActive = false;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidAMIID.NotFound");
      return xml("<DeregisterImageResponse />");
    }
    if (action === "DeleteSnapshot") {
      this.snapshotActive = false;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidSnapshot.NotFound");
      return xml("<DeleteSnapshotResponse />");
    }
    if (action === "DeleteKeyPair") {
      if (this.key?.id === parameters["KeyPairId"]) this.key = undefined;
      if (this.options.loseAfterEffect === action && !this.failed) {
        this.failed = true;
        throw new Error("lost response");
      }
      if (this.options.deleteNotFound === action) return awsError("InvalidKeyPair.NotFound");
      return xml("<DeleteKeyPairResponse />");
    }
    if (action === "DescribeInstances") {
      const requested = Object.entries(parameters)
        .filter(([key]) => /^InstanceId\.\d+$/.test(key))
        .map(([, value]) => String(value));
      if (requested.includes("i-retired")) {
        return awsError("InvalidInstanceID.NotFound");
      }
      if (
        this.options.terminationDescribeNotFound &&
        requested.length === 1 &&
        requested[0] === "i-owned" &&
        this.instanceState === "shutting-down"
      ) {
        this.terminationMissingCalls += 1;
        if (this.terminationMissingCalls >= 2) this.instanceState = "absent";
        return awsError("InvalidInstanceID.NotFound");
      }
      if (parameters["InstanceId.1"] === "i-owned" && this.instanceDescribeNotFound) {
        if (this.instanceState === "shutting-down") this.instanceState = "absent";
        return awsError("InvalidInstanceID.NotFound");
      }
      if (this.instanceState === "shutting-down") {
        this.terminationDescribeCalls += 1;
        if (this.terminationDescribeCalls >= 2) this.instanceState = "terminated";
      }
      this.unknownDescribeCalls += 1;
      const unknownVisible =
        this.unknownActive &&
        this.unknownDescribeCalls > (this.options.unknownVisibilityDelay ?? 0);
      const ownedVisible = this.instanceState !== "absent";
      const items = [
        ...(unknownVisible
          ? [
              "<item><instanceId>i-unknown</instanceId><instanceState><name>running</name></instanceState></item>",
            ]
          : []),
        ...(ownedVisible
          ? [
              `<item><instanceId>i-owned</instanceId><instanceType>t3.small</instanceType><ipAddress>203.0.113.10</ipAddress><instanceState><name>${this.instanceState}</name></instanceState><tagSet><item><key>crabbox_qualification_run</key><value>${identity.runId}</value></item><item><key>crabbox_qualification_op</key><value>${this.instanceOp}</value></item></tagSet><blockDeviceMapping><item><ebs><volumeId>vol-owned</volumeId></ebs></item></blockDeviceMapping></item>`,
            ]
          : []),
      ].join("");
      return xml(
        `<DescribeInstancesResponse><reservationSet>${items ? `<item><instancesSet>${items}</instancesSet></item>` : ""}</reservationSet></DescribeInstancesResponse>`,
      );
    }
    if (action === "DescribeSnapshots") {
      return xml(
        `<DescribeSnapshotsResponse><snapshotSet>${this.snapshotActive ? "<item><snapshotId>snap-child</snapshotId></item>" : ""}</snapshotSet></DescribeSnapshotsResponse>`,
      );
    }
    if (action === "DescribeVolumes") {
      return xml(
        `<DescribeVolumesResponse><volumeSet>${this.instanceState === "running" || this.instanceState === "shutting-down" ? "<item><volumeId>vol-owned</volumeId></item>" : ""}</volumeSet></DescribeVolumesResponse>`,
      );
    }
    if (action === "DescribeSecurityGroups") {
      if (this.options.securityGroupErrorWithId) {
        return new Response(
          "<Response><Errors><Error><Message>sg-fixed is unavailable</Message></Error></Errors></Response>",
          { status: 403, headers: { "content-type": "text/xml" } },
        );
      }
      const group =
        "<item><groupId>sg-fixed</groupId></item>" +
        (this.options.duplicateSecurityGroup ? "<item><groupId>sg-fixed</groupId></item>" : "");
      return xml(
        `<DescribeSecurityGroupsResponse><securityGroupInfo>${group}</securityGroupInfo></DescribeSecurityGroupsResponse>`,
      );
    }
    return xml(`<${action}Response/>`);
  }
}

function imageOp(calls: Array<{ action: string; parameters: Record<string, unknown> }>): string {
  const create = calls.findLast((call) => call.action === "CreateImage");
  for (let index = 1; index <= 64; index += 1) {
    if (create?.parameters[`TagSpecification.1.Tag.${index}.Key`] === "crabbox_qualification_op") {
      return String(create.parameters[`TagSpecification.1.Tag.${index}.Value`]);
    }
  }
  return "";
}

function tagValue(parameters: Record<string, unknown>, key: string): string {
  for (let index = 1; index <= 64; index += 1) {
    if (parameters[`TagSpecification.1.Tag.${index}.Key`] === key) {
      return String(parameters[`TagSpecification.1.Tag.${index}.Value`] ?? "");
    }
  }
  return "";
}

function keyXML(key: {
  id: string;
  name: string;
  publicKey: string;
  runId?: string;
  sha?: string;
}): string {
  return `<item>
    <keyPairId>${key.id}</keyPairId><keyName>${key.name}</keyName>
    <publicKey>${key.publicKey}</publicKey>
    <tagSet>${key.runId ? `<item><key>crabbox_qualification_run</key><value>${key.runId}</value></item><item><key>crabbox_qualification_sha</key><value>${key.sha}</value></item>` : ""}</tagSet>
  </item>`;
}

class MemoryStorage {
  private readonly values = new Map<string, unknown>();
  alarm?: number;

  async get<T>(key: string): Promise<T | undefined> {
    return structuredClone(this.values.get(key)) as T | undefined;
  }

  async put(key: string | Record<string, unknown>, value?: unknown): Promise<void> {
    if (typeof key === "string") {
      this.values.set(key, structuredClone(value));
      return;
    }
    for (const [name, entry] of Object.entries(key)) {
      this.values.set(name, structuredClone(entry));
    }
  }

  async delete(key: string): Promise<boolean> {
    return this.values.delete(key);
  }

  async list<T>(options: { prefix: string }): Promise<Map<string, T>> {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(options.prefix))
        .map(([key, value]) => [key, structuredClone(value) as T]),
    );
  }

  async setAlarm(value: number): Promise<void> {
    this.alarm = value;
  }

  async deleteAlarm(): Promise<void> {
    delete this.alarm;
  }
}

function awsError(code: string): Response {
  return new Response(
    `<Response><Errors><Error><Code>${code}</Code><Message>rejected</Message></Error></Errors></Response>`,
    { status: 400, headers: { "content-type": "text/xml" } },
  );
}

function awsServerError(): Response {
  return new Response(
    "<Response><Errors><Error><Code>InternalError</Code><Message>uncertain</Message></Error></Errors></Response>",
    { status: 500, headers: { "content-type": "text/xml" } },
  );
}

function xml(body: string): Response {
  return new Response(body, { status: 200, headers: { "content-type": "text/xml" } });
}
