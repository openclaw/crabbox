// Appended only by the credential-free cross-package regression harness to the
// actual private authority's tests, reusing their storage and provider fixtures.
describe("previous private package upgrade", () => {
  it.each(["cancelled", "lost-response"])(
    "settles %s without changing old identity or source custody",
    async (scenario) => {
      const previous = await import(/* @vite-ignore */ previousAuthorityURL);
      const oldRecovery = await import(/* @vite-ignore */ previousRecoveryURL);
      const currentRecovery = await import(/* @vite-ignore */ currentRecoveryURL);
      const oldSession = await import(/* @vite-ignore */ previousSessionURL);
      const currentSession = await import(/* @vite-ignore */ currentSessionURL);
      const f = firstWalkAuthorityFixture();
      const { taggingContract: _contract, ...originalIdentity } = f.identity;
      const originalSigner = vi.mocked(f.signer.execute).getMockImplementation()!;
      let producingOldState = true;
      vi.spyOn(f.signer, "execute").mockImplementation(
        async (service, action, region, parameters) => {
          if (producingOldState && parameters["DryRun"] === "true" && action === "CreateTags") {
            const tags = Object.fromEntries(
              Object.entries(parameters)
                .filter(([key]) => /^Tag\.\d+\.Key$/.test(key))
                .map(([key, value]) => [value, parameters[key.replace(/Key$/, "Value")]]),
            );
            const expected = {
              crabbox_qualification_run: originalIdentity.runId,
              crabbox_qualification_owner: originalIdentity.owner,
              crabbox_qualification_sha: originalIdentity.candidateSha,
              crabbox_qualification_expiry: originalIdentity.expiresAt,
            };
            return awsError(
              JSON.stringify(tags) === JSON.stringify(expected)
                ? "DryRunOperation"
                : "UnauthorizedOperation",
            );
          }
          return originalSigner(service, action, region, parameters);
        },
      );
      // This enrollment and every persisted byte below come from the actual old
      // baseline+overlay module. No new-format state is downgraded or patched.
      const oldRun = new previous.AWSQualificationRun({ storage: f.storage }, f.env, f.signer);
      const registryStorage = new MemoryStorage();
      const oldNamespace = { idFromName: (name: string) => name, get: () => oldRun };
      const oldRegistry = new previous.AWSQualificationRegistry(
        { storage: registryStorage },
        { AWS_QUALIFICATION_RUNS: oldNamespace },
      );
      const oldControl = new previous.AWSQualificationController(
        {
          AWS_QUALIFICATION_RUNS: oldNamespace,
          AWS_QUALIFICATION_REGISTRY: {
            idFromName: (name: string) => name,
            get: () => oldRegistry,
          },
        },
        controller,
      );
      await oldControl.enroll(originalIdentity);
      await importKey({ ...f, run: oldRun }, originalIdentity);
      await oldRun.execute(originalIdentity, request("RunInstances", runInstancesParams(), "ec2"));
      for (let i = 0; i < 4; i++) {
        try {
          await oldRun.prepareFirstWalk(originalIdentity, firstWalkRequest());
        } catch (error) {
          if (!/dispatched|readback/.test(String(error))) throw error;
        }
      }
      await oldControl.beginFinalization(originalIdentity.runId);
      const oldAttestation = await oldControl.attest(originalIdentity.runId);
      const oldBinding = oldRecovery.recoveryBinding(oldAttestation);
      const runBytes = JSON.stringify(Object.fromEntries(await f.storage.list({ prefix: "" })));
      const registryBytes = JSON.stringify(
        Object.fromEntries(await registryStorage.list({ prefix: "" })),
      );
      expect(oldBinding.taggingContract).toBeUndefined();
      expect(JSON.parse(runBytes)["first-walk"]).toBeDefined();
      expect(JSON.parse(runBytes)["ledger"].keyPairIds).toHaveLength(1);
      expect(JSON.parse(registryBytes)["active"].cleanupState).toBe("finalizing");

      const storage = new MemoryStorage();
      await storage.put(JSON.parse(runBytes));
      const nextRegistryStorage = new MemoryStorage();
      await nextRegistryStorage.put(JSON.parse(registryBytes));
      expect(JSON.stringify(Object.fromEntries(await storage.list({ prefix: "" })))).toBe(runBytes);
      expect(
        JSON.stringify(Object.fromEntries(await nextRegistryStorage.list({ prefix: "" }))),
      ).toBe(registryBytes);
      producingOldState = false;
      const env = {
        ...f.env,
        CRABBOX_AWS_QUALIFICATION_AUTHORITY_SHA: "c".repeat(40),
        CRABBOX_AWS_QUALIFICATION_AUTHORITY_VERSION: "cleanup-upgrade-v2",
        CRABBOX_AWS_QUALIFICATION_RECOVERY_JSON: JSON.stringify(oldBinding),
      };
      let run = new AWSQualificationRun({ storage } as never, env as never, f.signer);
      const namespace = { idFromName: (name: string) => name, get: () => run };
      const registry = new AWSQualificationRegistry(
        { storage: nextRegistryStorage } as never,
        { AWS_QUALIFICATION_RUNS: namespace } as never,
      );
      const control = new AWSQualificationController(
        {
          AWS_QUALIFICATION_RUNS: namespace,
          AWS_QUALIFICATION_REGISTRY: { idFromName: (name: string) => name, get: () => registry },
        } as never,
        controller,
      );
      expect(await control.discover()).toEqual(JSON.parse(registryBytes)["active"]);
      expect(currentRecovery.recoveryBinding(await control.attest(originalIdentity.runId))).toEqual(
        oldBinding,
      );
      const forward = new AWSQualificationRun({ storage } as never, f.env as never, f.signer);
      await expect(forward.execute(originalIdentity, request("GetCallerIdentity"))).rejects.toThrow(
        "tagging contract",
      );
      await expect(control.enroll(originalIdentity)).rejects.toThrow("tagging contract");
      await expect(run.execute(originalIdentity, request("GetCallerIdentity"))).rejects.toThrow(
        /cleanup-only|tagging contract/,
      );
      await expect(run.prepareFirstWalk(originalIdentity, firstWalkRequest())).rejects.toThrow(
        /cleanup-only|tagging contract/,
      );
      const sourceCustody = await storage.get("first-walk");
      if (scenario === "lost-response") {
        f.fault("DeleteVolume");
        await expect(control.finalize(originalIdentity.runId)).rejects.toThrow();
        expect((await run.attest(controller)).finalized).toBe(false);
        expect(await storage.get("first-walk")).toEqual(sourceCustody);
        expect(currentRecovery.recoveryBinding(await run.attest(controller))).toEqual(oldBinding);
        run = new AWSQualificationRun({ storage } as never, env as never, f.signer);
      }
      await control.finalize(originalIdentity.runId);
      const proof = await control.attest(originalIdentity.runId);
      expect(proof.finalized).toBe(true);
      expect(proof.finalReceipt?.failureCodes).toEqual([]);
      expect(proof.finalReceipt?.finalCounts).toEqual({
        images: 0,
        instances: 0,
        keyPairs: 0,
        snapshots: 0,
        volumes: 0,
      });
      expect(currentRecovery.recoveryBinding(proof)).toEqual(oldBinding);
      expect(
        (await storage.get<{ identity: AWSQualificationRunIdentity }>("run"))!.identity,
      ).toEqual(originalIdentity);
      expect(await storage.get("first-walk")).toEqual(sourceCustody);
      expect(proof.taggingContract).toBeUndefined();
      await control.retire(originalIdentity.runId);
      expect(await control.discover()).toBeUndefined();
      expect((await nextRegistryStorage.get<Array<{ runId: string }>>("retired"))![0]!.runId).toBe(
        originalIdentity.runId,
      );
      const scope = {
        profile: "first-walk-aws-session/v1",
        repository: "example/repo",
        workflowSha: "a".repeat(40),
        candidateSha: originalIdentity.candidateSha,
        runId: "image-qualification-42-1",
        candidateArtifactId: "9",
        candidateArtifactDigest: `sha256:${"c".repeat(64)}`,
        manifestSha256: "d".repeat(64),
        startsAt: "2030-01-01T00:00:00.000Z",
        workExpiresAt: "2030-01-01T02:10:00.000Z",
        expiresAt: "2030-01-01T02:30:00.000Z",
        recoveryExpiresAt: "2030-01-01T04:30:00.000Z",
      };
      expect(currentSession.cleanupSessionPolicy(scope)).toEqual(
        oldSession.cleanupSessionPolicy(scope),
      );
      expect(() => currentSession.runSessionPolicy(scope)).toThrow();
      expect(
        f.signer.calls.filter((call) => ["CreateTags", "DeleteTags"].includes(call.action)),
      ).toEqual([]);
      expect(
        f.signer.calls.filter(
          (call) =>
            call.action === "DeleteSnapshot" &&
            call.parameters["SnapshotId"] === originalIdentity.retainedImage!.snapshotId,
        ),
      ).toEqual([]);
      expect(f.signer.calls.filter((call) => call.action === "DeleteVolume")).toHaveLength(1);
      console.info(
        JSON.stringify({
          proof: "UPGRADE_SETTLED",
          scenario,
          originalIdentityPreserved: true,
          originalRecoveryBindingPreserved: true,
          originalAttachmentRecordPreserved: true,
          borrowedSourceNotDeleted: true,
          uncertainDeleteNotReplayed: true,
          finalCounts: proof.finalReceipt?.finalCounts,
          registryRetired: true,
        }),
      );
    },
  );
});
