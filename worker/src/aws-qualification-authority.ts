import { AwsClient } from "aws4fetch";
import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import { XMLParser } from "fast-xml-parser";

import { sha256Hex } from "./auth";
import {
  awsQualificationInstanceTypes,
  awsQualificationMaxRunMs,
  type AWSQualificationControllerProps,
  type AWSQualificationRequest,
  type AWSQualificationResponse,
  type AWSQualificationRunIdentity,
  type AWSQualificationService,
} from "./aws-qualification-contract";
import { requireAWSRegion } from "./aws-region";
import type { AWSCredentials } from "./types";

const ec2Version = "2016-11-15";
const stsVersion = "2011-06-15";
const onDemandQuotaCode = "L-1216C47A";
const stateKey = "run";
const ledgerKey = "ledger";
const receiptPrefix = "receipt:";
const intentPrefix = "intent:";
const cleanupRetryMs = 60_000;
const maxRootGB = 20;
const maxUserDataBytes = 24 * 1024;
const maxRequestBytes = 64 * 1024;
const maxResponseBytes = 64 * 1024;
const maxRequestNodes = 512;
const maxCandidateOperations = 64;
const maxPendingIntents = 8;
const maxLaunches = 3;
const maxActiveInstances = 1;
const maxActiveImages = 1;
const maxActiveSnapshots = 1;
const reconciliationBackoffMs = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000] as const;
const inventoryBackoffMs = reconciliationBackoffMs;
const parser = new XMLParser({ ignoreAttributes: false });
const requestKeys = new Set(["action", "opId", "parameters", "region", "service"]);

const allowedEC2Actions = new Set([
  "CreateImage",
  "CreateTags",
  "DeleteKeyPair",
  "DeleteSnapshot",
  "DeregisterImage",
  "DescribeImages",
  "DescribeInstances",
  "DescribeKeyPairs",
  "DescribeSecurityGroups",
  "DescribeSnapshots",
  "DescribeVolumes",
  "ImportKeyPair",
  "RunInstances",
  "TerminateInstances",
]);

const mutatingEC2Actions = new Set([
  "CreateImage",
  "CreateTags",
  "DeleteKeyPair",
  "DeleteSnapshot",
  "DeregisterImage",
  "ImportKeyPair",
  "RunInstances",
  "TerminateInstances",
]);

const preservedTagKeys = new Set([
  "Name",
  "architecture",
  "created_by",
  "crabbox",
  "crabbox_checkpoint_id",
  "crabbox_checkpoint_lease",
  "crabbox_checkpoint_token",
  "lease",
  "market",
  "org",
  "owner",
  "provider",
  "server_type",
  "target",
]);

interface AWSQualificationAuthorityEnv {
  AWS_ACCESS_KEY_ID?: string;
  AWS_SECRET_ACCESS_KEY?: string;
  AWS_SESSION_TOKEN?: string;
  CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID?: string;
  CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID?: string;
  CRABBOX_AWS_QUALIFICATION_REGION?: string;
  CRABBOX_AWS_QUALIFICATION_ROOT_GB?: string;
  CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID?: string;
  CRABBOX_AWS_QUALIFICATION_SUBNET_ID?: string;
  AWS_QUALIFICATION_RUNS: DurableObjectNamespace<AWSQualificationRun>;
}

interface AWSQualificationPolicy {
  accountId: string;
  baseAmiId: string;
  region: string;
  rootGB: number;
  securityGroupId: string;
  subnetId: string;
}

interface AWSQualificationRunState {
  identity: AWSQualificationRunIdentity;
  enrolledAt: string;
  accountVerified: boolean;
  operationCount: number;
  policy: AWSQualificationPolicy;
  policyHash: string;
  finalizedAt?: string;
}

interface AWSQualificationLedger {
  imageIds: string[];
  instanceIds: string[];
  keyPairIds: string[];
  keyPairNames: string[];
  snapshotIds: string[];
  volumeIds: string[];
  launchCount: number;
}

interface AWSQualificationReceipt {
  requestHash: string;
  response: AWSQualificationResponse;
}

interface AWSQualificationIntent {
  requestHash: string;
  request: AWSQualificationRequest;
  startedAt: string;
}

interface AuthoritySigner {
  execute(
    service: AWSQualificationService,
    action: string,
    region: string,
    parameters: Record<string, unknown>,
  ): Promise<Response>;
}

export class AWSQualificationTransport extends WorkerEntrypoint<
  AWSQualificationAuthorityEnv,
  AWSQualificationRunIdentity
> {
  async execute(request: AWSQualificationRequest): Promise<AWSQualificationResponse> {
    return await qualificationRun(this.env, this.ctx.props.runId).execute(this.ctx.props, request);
  }
}

export default AWSQualificationTransport;

export class AWSQualificationController extends WorkerEntrypoint<
  AWSQualificationAuthorityEnv,
  AWSQualificationControllerProps
> {
  async enroll(identity: AWSQualificationRunIdentity): Promise<void> {
    await qualificationRun(this.env, identity.runId).enroll(this.ctx.props, identity);
  }

  async finalize(runId: string): Promise<AWSQualificationLedger> {
    return await qualificationRun(this.env, runId).finalize(this.ctx.props);
  }
}

export class AWSQualificationRun extends DurableObject<AWSQualificationAuthorityEnv> {
  private serial = Promise.resolve();
  private readonly signer: AuthoritySigner;

  constructor(
    ctx: DurableObjectState,
    env: AWSQualificationAuthorityEnv,
    signer: AuthoritySigner = new DirectAuthoritySigner(env),
  ) {
    super(ctx, env);
    this.signer = signer;
  }

  async enroll(
    controller: AWSQualificationControllerProps,
    identity: AWSQualificationRunIdentity,
  ): Promise<void> {
    return await this.serialized(async () => {
      const policy = authorityPolicy(this.env);
      validateController(controller, identity.deploymentHash);
      validateRunIdentity(identity);
      const expiresAt = Date.parse(identity.expiresAt);
      const now = Date.now();
      if (expiresAt <= now || expiresAt > now + awsQualificationMaxRunMs) {
        throw new Error("AWS qualification expiry must be in the next 120 minutes");
      }
      if (policy.region !== requireAWSRegion(policy.region)) {
        throw new Error("AWS qualification region is invalid");
      }
      const existing = await this.ctx.storage.get<AWSQualificationRunState>(stateKey);
      if (existing) {
        if (
          canonicalJSON(existing.identity) !== canonicalJSON(identity) ||
          existing.policyHash !== (await qualificationPolicyHash(policy))
        ) {
          throw new Error("AWS qualification run identity is already enrolled");
        }
        return;
      }
      const policyHash = await qualificationPolicyHash(policy);
      await this.ctx.storage.put({
        [stateKey]: {
          identity: structuredClone(identity),
          enrolledAt: new Date(now).toISOString(),
          accountVerified: false,
          operationCount: 0,
          policy: structuredClone(policy),
          policyHash,
        } satisfies AWSQualificationRunState,
        [ledgerKey]: emptyLedger(),
      });
      await this.ctx.storage.setAlarm(expiresAt);
    });
  }

  async execute(
    identity: AWSQualificationRunIdentity,
    request: AWSQualificationRequest,
  ): Promise<AWSQualificationResponse> {
    return await this.serialized(() => this.executeSerialized(identity, request));
  }

  async finalize(controller?: AWSQualificationControllerProps): Promise<AWSQualificationLedger> {
    return await this.serialized(() => this.finalizeSerialized(controller));
  }

  override async alarm(): Promise<void> {
    await this.finalize();
  }

  private async executeSerialized(
    identity: AWSQualificationRunIdentity,
    request: AWSQualificationRequest,
  ): Promise<AWSQualificationResponse> {
    const run = await this.requireActiveRun(identity);
    const policy = run.policy;
    const normalizedRequest = validateRequestShape(request, policy.region);
    const requestHash = await sha256Hex(
      canonicalJSON({
        action: normalizedRequest.action,
        parameters: normalizedRequest.parameters,
        region: normalizedRequest.region,
        service: normalizedRequest.service,
      }),
    );
    const receipt = await this.ctx.storage.get<AWSQualificationReceipt>(
      `${receiptPrefix}${normalizedRequest.opId}`,
    );
    if (receipt) {
      if (receipt.requestHash !== requestHash) {
        throw new Error("AWS qualification opId was replayed with a different request");
      }
      return receipt.response;
    }
    const priorIntent = await this.ctx.storage.get<AWSQualificationIntent>(
      `${intentPrefix}${normalizedRequest.opId}`,
    );
    if (priorIntent) {
      if (priorIntent.requestHash !== requestHash) {
        throw new Error("AWS qualification pending opId has a different request");
      }
      const reconciled =
        priorIntent.request.action === "CreateImage" ||
        priorIntent.request.action === "ImportKeyPair"
          ? await this.reconcileIntentWithBackoff(run, priorIntent, policy)
          : await this.reconcileIntent(run, priorIntent, policy);
      if (reconciled) return reconciled;
      if (
        priorIntent.request.action === "CreateImage" ||
        priorIntent.request.action === "ImportKeyPair"
      ) {
        throw new Error(
          `AWS qualification ${priorIntent.request.action} outcome remains unresolved`,
        );
      }
    }

    const ledger = await this.ledger();
    const pending = await this.ctx.storage.list<AWSQualificationIntent>({ prefix: intentPrefix });
    if (!priorIntent) {
      if (run.operationCount >= maxCandidateOperations) {
        throw new Error("AWS qualification candidate operation limit reached");
      }
      if (pending.size >= maxPendingIntents) {
        throw new Error("AWS qualification pending intent limit reached");
      }
      assertLifecycleCapacity(normalizedRequest, ledger, pending);
    }
    const authorized = await authorizeRequest(
      normalizedRequest,
      identity,
      policy,
      ledger,
      await qualificationPhysicalKeyName(identity.runId),
      await qualificationClientToken(identity.runId, normalizedRequest.opId),
    );
    if (normalizedRequest.service !== "sts") {
      await this.ensureAccount(run, policy, authorized.mutating);
    }
    if (!priorIntent) {
      run.operationCount += 1;
      if (normalizedRequest.action === "RunInstances") ledger.launchCount += 1;
      await this.ctx.storage.put({
        [stateKey]: run,
        [ledgerKey]: ledger,
        ...(authorized.mutating
          ? {
              [`${intentPrefix}${normalizedRequest.opId}`]: {
                requestHash,
                request: normalizedRequest,
                startedAt: new Date().toISOString(),
              } satisfies AWSQualificationIntent,
            }
          : {}),
      });
    }

    const response = await this.signer.execute(
      normalizedRequest.service,
      normalizedRequest.action,
      policy.region,
      authorized.parameters,
    );
    const result = await boundedResponse(response);
    if (authorized.mutating && result.status >= 500) {
      throw new Error(
        `AWS qualification ${normalizedRequest.action} response is ambiguous: http ${result.status}`,
      );
    }
    if (result.status >= 200 && result.status < 300) {
      if (normalizedRequest.action === "ImportKeyPair") {
        await this.recordImportedKey(run, normalizedRequest, authorized.parameters, result, ledger);
      } else if (normalizedRequest.action === "CreateImage") {
        await this.recordCreatedImage(run, normalizedRequest, result, ledger);
      } else {
        updateLedgerFromResponse(
          ledger,
          normalizedRequest.action,
          result.body,
          authorized.parameters,
        );
      }
      await this.ctx.storage.put(ledgerKey, ledger);
    }
    await this.ctx.storage.put(`${receiptPrefix}${normalizedRequest.opId}`, {
      requestHash,
      response: result,
    } satisfies AWSQualificationReceipt);
    await this.ctx.storage.delete(`${intentPrefix}${normalizedRequest.opId}`);
    return result;
  }

  private async recordImportedKey(
    run: AWSQualificationRunState,
    request: AWSQualificationRequest,
    parameters: Record<string, unknown>,
    result: AWSQualificationResponse,
    ledger: AWSQualificationLedger,
  ): Promise<void> {
    const root = awsXMLRoot(result.body, "ImportKeyPair");
    const keyPairId = asString(root["keyPairId"]);
    const keyName = asString(root["keyName"]) || String(parameters["KeyName"] ?? "");
    if (!keyPairId || keyName !== (await qualificationPhysicalKeyName(run.identity.runId))) {
      throw new Error("AWS qualification key import returned an invalid identity");
    }
    const verified = await this.describeOwnedKey(run, request, keyName);
    if (!verified || verified.id !== keyPairId) {
      throw new Error("AWS qualification key import could not be verified");
    }
    ledger.keyPairIds = [keyPairId];
    ledger.keyPairNames = [keyName];
  }

  private async describeOwnedKey(
    run: AWSQualificationRunState,
    request: AWSQualificationRequest,
    keyName: string,
  ): Promise<{ id: string; name: string } | undefined> {
    const response = await this.signer.execute("ec2", "DescribeKeyPairs", run.policy.region, {
      IncludePublicKey: "true",
      "KeyName.1": keyName,
    });
    const result = await boundedResponse(response);
    if (result.status >= 300) {
      if (result.body.includes("InvalidKeyPair.NotFound")) return undefined;
      throw new Error(`AWS qualification key verification failed: http ${result.status}`);
    }
    const keys = items(record(awsXMLRoot(result.body, "DescribeKeyPairs")["keySet"])["item"]).map(
      record,
    );
    if (keys.length === 0) return undefined;
    if (keys.length !== 1) throw new Error("AWS qualification key verification is ambiguous");
    const key = keys[0]!;
    const tags = awsTagMap(key["tagSet"]);
    const material = decodePublicKeyMaterial(String(request.parameters["PublicKeyMaterial"] ?? ""));
    if (
      asString(key["keyName"]) !== keyName ||
      !asString(key["keyPairId"]) ||
      publicKeyIdentity(asString(key["publicKey"])) !== publicKeyIdentity(material) ||
      tags.get("crabbox_qualification_run") !== run.identity.runId ||
      tags.get("crabbox_qualification_sha") !== run.identity.candidateSha
    ) {
      throw new Error("AWS qualification physical key name is occupied by an unowned key");
    }
    return { id: asString(key["keyPairId"]), name: keyName };
  }

  private async recordCreatedImage(
    run: AWSQualificationRunState,
    request: AWSQualificationRequest,
    result: AWSQualificationResponse,
    ledger: AWSQualificationLedger,
  ): Promise<void> {
    const imageId = asString(awsXMLRoot(result.body, "CreateImage")["imageId"]);
    if (!imageId) throw new Error("AWS qualification CreateImage returned no image id");
    await this.captureImage(run, request.opId, imageId, ledger);
  }

  private async captureImage(
    run: AWSQualificationRunState,
    opId: string,
    imageId: string,
    ledger: AWSQualificationLedger,
  ): Promise<void> {
    const response = await this.signer.execute("ec2", "DescribeImages", run.policy.region, {
      "ImageId.1": imageId,
      "Owner.1": "self",
    });
    const result = await boundedResponse(response);
    if (result.status >= 300) {
      throw new Error(`AWS qualification image verification failed: http ${result.status}`);
    }
    const images = items(
      record(awsXMLRoot(result.body, "DescribeImages")["imagesSet"])["item"],
    ).map(record);
    if (images.length !== 1) throw new Error("AWS qualification image is not uniquely visible");
    const image = images[0]!;
    const tags = awsTagMap(image["tagSet"]);
    if (
      asString(image["imageId"]) !== imageId ||
      tags.get("crabbox_qualification_run") !== run.identity.runId ||
      tags.get("crabbox_qualification_op") !== opId
    ) {
      throw new Error("AWS qualification image ownership verification failed");
    }
    const snapshots = imageSnapshotIDs(image);
    if (snapshots.length !== maxActiveSnapshots) {
      throw new Error("AWS qualification image snapshot is not uniquely visible");
    }
    ledger.imageIds = [imageId];
    ledger.snapshotIds = snapshots;
  }

  private async reconcileIntent(
    run: AWSQualificationRunState,
    intent: AWSQualificationIntent,
    policy: AWSQualificationPolicy,
  ): Promise<AWSQualificationResponse | undefined> {
    await this.ensureAccount(run, policy);
    if (intent.request.action === "RunInstances") {
      // RunInstances is retried through the normal path with an authority-derived ClientToken.
      // AWS therefore returns the original allocation rather than creating a second instance.
      return undefined;
    }
    if (intent.request.action === "ImportKeyPair") {
      const keyName = await qualificationPhysicalKeyName(run.identity.runId);
      const owned = await this.describeOwnedKey(run, intent.request, keyName);
      if (!owned) return undefined;
      const ledger = await this.ledger();
      ledger.keyPairIds = [owned.id];
      ledger.keyPairNames = [owned.name];
      await this.ctx.storage.put(ledgerKey, ledger);
      const response = {
        status: 200,
        body: `<ImportKeyPairResponse><keyPairId>${xmlEscape(owned.id)}</keyPairId><keyName>${xmlEscape(owned.name)}</keyName></ImportKeyPairResponse>`,
      };
      await this.ctx.storage.put(`${receiptPrefix}${intent.request.opId}`, {
        requestHash: intent.requestHash,
        response,
      } satisfies AWSQualificationReceipt);
      await this.ctx.storage.delete(`${intentPrefix}${intent.request.opId}`);
      return response;
    }
    if (intent.request.action !== "CreateImage") {
      return undefined;
    }
    const response = await this.signer.execute("ec2", "DescribeImages", policy.region, {
      "Filter.1.Name": "tag:crabbox_qualification_run",
      "Filter.1.Value.1": run.identity.runId,
      "Filter.2.Name": "tag:crabbox_qualification_op",
      "Filter.2.Value.1": intent.request.opId,
      "Owner.1": "self",
    });
    const result = await boundedResponse(response);
    if (result.status < 200 || result.status >= 300) return undefined;
    const root = awsXMLRoot(result.body, "DescribeImages");
    const matches = items(record(root["imagesSet"])["item"]).map(record);
    if (matches.length === 0) return undefined;
    if (matches.length !== 1) {
      throw new Error("AWS qualification image reconciliation is ambiguous");
    }
    const id = asString(matches[0]!["imageId"]);
    if (!id) throw new Error("AWS qualification image reconciliation returned no id");
    const ledger = await this.ledger();
    await this.captureImage(run, intent.request.opId, id, ledger);
    await this.ctx.storage.put(ledgerKey, ledger);
    const synthetic = `<CreateImageResponse><imageId>${xmlEscape(id)}</imageId></CreateImageResponse>`;
    const receipt = {
      requestHash: intent.requestHash,
      response: { status: 200, body: synthetic },
    } satisfies AWSQualificationReceipt;
    await this.ctx.storage.put(`${receiptPrefix}${intent.request.opId}`, receipt);
    await this.ctx.storage.delete(`${intentPrefix}${intent.request.opId}`);
    return receipt.response;
  }

  private async reconcileIntentWithBackoff(
    run: AWSQualificationRunState,
    intent: AWSQualificationIntent,
    policy: AWSQualificationPolicy,
    attempt = 0,
  ): Promise<AWSQualificationResponse | undefined> {
    try {
      const reconciled = await this.reconcileIntent(run, intent, policy);
      if (reconciled) return reconciled;
    } catch (error) {
      if (!isRetryableReconciliationError(error)) throw error;
    }
    const delay = reconciliationBackoffMs[attempt];
    if (delay === undefined) return undefined;
    await sleep(delay);
    return await this.reconcileIntentWithBackoff(run, intent, policy, attempt + 1);
  }

  private async ensureAccount(
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
    force = false,
  ): Promise<void> {
    if (run.accountVerified && !force) return;
    const response = await this.signer.execute("sts", "GetCallerIdentity", policy.region, {});
    const result = await boundedResponse(response);
    if (result.status < 200 || result.status >= 300) {
      throw new Error(`AWS qualification account verification failed: http ${result.status}`);
    }
    const identity = record(
      awsXMLRoot(result.body, "GetCallerIdentity")["GetCallerIdentityResult"],
    );
    if (asString(identity["Account"]) !== policy.accountId) {
      throw new Error("AWS qualification authority is authenticated to the wrong account");
    }
    run.accountVerified = true;
    await this.ctx.storage.put(stateKey, run);
  }

  private async requireActiveRun(
    identity: AWSQualificationRunIdentity,
  ): Promise<AWSQualificationRunState> {
    const run = await this.ctx.storage.get<AWSQualificationRunState>(stateKey);
    if (!run || canonicalJSON(run.identity) !== canonicalJSON(identity)) {
      throw new Error("AWS qualification service binding is not enrolled for this run");
    }
    if (run.finalizedAt) throw new Error("AWS qualification run is finalized");
    if (Date.now() >= Date.parse(run.identity.expiresAt)) {
      await this.finalizeSerialized();
      throw new Error("AWS qualification run expired");
    }
    if ((await qualificationPolicyHash(authorityPolicy(this.env))) !== run.policyHash) {
      throw new Error("AWS qualification authority policy changed after enrollment");
    }
    return run;
  }

  private async finalizeSerialized(
    controller?: AWSQualificationControllerProps,
  ): Promise<AWSQualificationLedger> {
    const run = await this.ctx.storage.get<AWSQualificationRunState>(stateKey);
    const ledger = await this.ledger();
    if (!run || run.finalizedAt) return ledger;
    if (controller) validateController(controller, run.identity.deploymentHash);
    const policy = run.policy;
    await this.ensureAccount(run, policy);
    const failures: string[] = [];
    await this.recoverPendingIntents(run, policy, failures);
    const recoveredLedger = await this.inventoryRunResourcesEventually(
      run,
      await this.ledger(),
      failures,
      true,
    );
    await this.ctx.storage.put(ledgerKey, recoveredLedger);
    for (const imageId of recoveredLedger.imageIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- teardown order protects dependent snapshots.
      await this.cleanup(run, policy, "DeregisterImage", { ImageId: imageId }, failures);
    }
    for (const snapshotId of recoveredLedger.snapshotIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each exact resource has independent evidence.
      await this.cleanup(run, policy, "DeleteSnapshot", { SnapshotId: snapshotId }, failures);
    }
    for (const instanceId of recoveredLedger.instanceIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- terminate only ledger-owned instances.
      await this.cleanup(
        run,
        policy,
        "TerminateInstances",
        { "InstanceId.1": instanceId },
        failures,
      );
    }
    for (const keyPairId of recoveredLedger.keyPairIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- delete only ledger-owned key pairs.
      await this.cleanup(run, policy, "DeleteKeyPair", { KeyPairId: keyPairId }, failures);
    }
    const residue = await this.inventoryRunResourcesEventually(
      run,
      emptyLedger(recoveredLedger.launchCount),
      failures,
      false,
    );
    await this.verifyZeroResidue(recoveredLedger, policy, failures);
    if (hasOwnedResources(residue)) failures.push("run-tag inventory still contains resources");
    if (failures.length === 0) {
      await this.retireUnresolvedCreationIntents();
    }
    if (failures.length > 0) {
      await this.ctx.storage.setAlarm(Date.now() + cleanupRetryMs);
      throw new Error(`AWS qualification cleanup incomplete: ${failures.join("; ")}`);
    }
    await this.ctx.storage.put(ledgerKey, emptyLedger(recoveredLedger.launchCount));
    run.finalizedAt = new Date().toISOString();
    await this.ctx.storage.put(stateKey, run);
    await this.ctx.storage.deleteAlarm();
    return recoveredLedger;
  }

  private async recoverPendingIntents(
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    const pending = [
      ...(await this.ctx.storage.list<AWSQualificationIntent>({ prefix: intentPrefix })),
    ];
    await this.recoverPendingIntentEntries(pending, run, policy, failures);
  }

  private async recoverPendingIntentEntries(
    pending: Array<[string, AWSQualificationIntent]>,
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    const entry = pending[0];
    if (!entry) return;
    const [key, intent] = entry;
    try {
      await this.recoverPendingIntent(key, intent, run, policy, failures);
    } catch (error) {
      if (intent.request.action !== "CreateImage" && intent.request.action !== "ImportKeyPair") {
        failures.push(
          `${intent.request.action} reconciliation: ${
            error instanceof Error ? error.message : String(error)
          }`,
        );
      }
    }
    // The queue is capped at maxPendingIntents. Serial recursion preserves each ledger commit
    // before the next intent observes state without an unbounded call stack.
    await this.recoverPendingIntentEntries(pending.slice(1), run, policy, failures);
  }

  private async recoverPendingIntent(
    key: string,
    intent: AWSQualificationIntent,
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    if (intent.request.action === "CreateImage" || intent.request.action === "ImportKeyPair") {
      await this.reconcileIntentWithBackoff(run, intent, policy);
      return;
    }

    const ledger = await this.ledger();
    const authorized = await authorizeRequest(
      intent.request,
      run.identity,
      policy,
      ledger,
      await qualificationPhysicalKeyName(run.identity.runId),
      await qualificationClientToken(run.identity.runId, intent.request.opId),
    );
    if (authorized.mutating) {
      await this.ensureAccount(run, policy, true);
    }
    const response = await this.signer.execute(
      intent.request.service,
      intent.request.action,
      policy.region,
      authorized.parameters,
    );
    const result = await boundedResponse(response);
    if (result.status < 200 || result.status >= 300) {
      failures.push(`${intent.request.action} reconciliation http ${result.status}`);
      return;
    }
    updateLedgerFromResponse(ledger, intent.request.action, result.body, authorized.parameters);
    await this.ctx.storage.put(ledgerKey, ledger);
    if (intent.request.action === "RunInstances") {
      await this.ctx.storage.put(key.replace(intentPrefix, receiptPrefix), {
        requestHash: intent.requestHash,
        response: result,
      } satisfies AWSQualificationReceipt);
    }
    await this.ctx.storage.delete(key);
  }

  private async cleanup(
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
    action: string,
    parameters: Record<string, unknown>,
    failures: string[],
  ): Promise<void> {
    try {
      await this.ensureAccount(run, policy, true);
      const response = await this.signer.execute("ec2", action, policy.region, parameters);
      const result = await boundedResponse(response);
      if (
        result.status >= 300 &&
        !result.body.includes("NotFound") &&
        !result.body.includes("InvalidAMIID.NotFound") &&
        !result.body.includes("InvalidSnapshot.NotFound")
      ) {
        failures.push(`${action} http ${result.status}`);
      }
    } catch (error) {
      failures.push(`${action}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }

  private async inventoryRunResources(
    run: AWSQualificationRunState,
    ledger: AWSQualificationLedger,
    failures: string[],
  ): Promise<AWSQualificationLedger> {
    const filter = {
      "Filter.1.Name": "tag:crabbox_qualification_run",
      "Filter.1.Value.1": run.identity.runId,
    };
    try {
      const [instancesResponse, imagesResponse, snapshotsResponse, volumesResponse, keysResponse] =
        await Promise.all([
          this.signer.execute("ec2", "DescribeInstances", run.policy.region, filter),
          this.signer.execute("ec2", "DescribeImages", run.policy.region, {
            ...filter,
            "Owner.1": "self",
          }),
          this.signer.execute("ec2", "DescribeSnapshots", run.policy.region, {
            ...filter,
            "Owner.1": "self",
          }),
          this.signer.execute("ec2", "DescribeVolumes", run.policy.region, filter),
          this.signer.execute("ec2", "DescribeKeyPairs", run.policy.region, filter),
        ]);
      const [instances, images, snapshots, volumes, keys] = await Promise.all([
        boundedResponse(instancesResponse),
        boundedResponse(imagesResponse),
        boundedResponse(snapshotsResponse),
        boundedResponse(volumesResponse),
        boundedResponse(keysResponse),
      ]);
      for (const result of [instances, images, snapshots, volumes, keys]) {
        if (result.status >= 300) throw new Error(`inventory http ${result.status}`);
      }
      ledger.instanceIds = reservationsFromXML(instances.body)
        .flatMap((reservation) => items(record(reservation["instancesSet"])["item"]).map(record))
        .filter((instance) => asString(record(instance["instanceState"])["name"]) !== "terminated")
        .map((instance) => asString(instance["instanceId"]))
        .filter(Boolean);
      const imageRecords = items(
        record(awsXMLRoot(images.body, "DescribeImages")["imagesSet"])["item"],
      ).map(record);
      ledger.imageIds = imageRecords.map((image) => asString(image["imageId"])).filter(Boolean);
      ledger.snapshotIds = [
        ...new Set([
          ...imageRecords.flatMap(imageSnapshotIDs),
          ...items(record(awsXMLRoot(snapshots.body, "DescribeSnapshots")["snapshotSet"])["item"])
            .map(record)
            .map((snapshot) => asString(snapshot["snapshotId"]))
            .filter(Boolean),
        ]),
      ];
      ledger.volumeIds = items(
        record(awsXMLRoot(volumes.body, "DescribeVolumes")["volumeSet"])["item"],
      )
        .map(record)
        .map((volume) => asString(volume["volumeId"]))
        .filter(Boolean);
      const keyRecords = items(
        record(awsXMLRoot(keys.body, "DescribeKeyPairs")["keySet"])["item"],
      ).map(record);
      ledger.keyPairIds = keyRecords.map((key) => asString(key["keyPairId"])).filter(Boolean);
      ledger.keyPairNames = keyRecords.map((key) => asString(key["keyName"])).filter(Boolean);
      if (
        ledger.instanceIds.length > maxActiveInstances ||
        ledger.imageIds.length > maxActiveImages ||
        ledger.snapshotIds.length > maxActiveSnapshots ||
        ledger.keyPairIds.length > 1
      ) {
        failures.push("run-tag inventory exceeds qualification resource bounds");
      }
    } catch (error) {
      failures.push(`run-tag inventory: ${error instanceof Error ? error.message : String(error)}`);
    }
    return ledger;
  }

  private async inventoryRunResourcesEventually(
    run: AWSQualificationRunState,
    ledger: AWSQualificationLedger,
    failures: string[],
    accumulate: boolean,
    attempt = 0,
  ): Promise<AWSQualificationLedger> {
    const attemptFailures: string[] = [];
    const observed = await this.inventoryRunResources(
      run,
      emptyLedger(ledger.launchCount),
      attemptFailures,
    );
    const next = accumulate ? mergeLedgers(ledger, observed) : observed;
    const delay = inventoryBackoffMs[attempt];
    if (delay === undefined) {
      failures.push(...attemptFailures);
      return next;
    }
    await sleep(delay);
    return await this.inventoryRunResourcesEventually(run, next, failures, accumulate, attempt + 1);
  }

  private async retireUnresolvedCreationIntents(): Promise<void> {
    const pending = await this.ctx.storage.list<AWSQualificationIntent>({ prefix: intentPrefix });
    await Promise.all(
      [...pending]
        .filter(
          ([, intent]) =>
            intent.request.action === "CreateImage" || intent.request.action === "ImportKeyPair",
        )
        .map(([key]) => this.ctx.storage.delete(key)),
    );
  }

  private async verifyZeroResidue(
    ledger: AWSQualificationLedger,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    await this.verifyAbsent(
      policy,
      "DescribeInstances",
      "InstanceId",
      ledger.instanceIds,
      "reservationSet",
      failures,
    );
    await this.verifyAbsent(
      policy,
      "DescribeImages",
      "ImageId",
      ledger.imageIds,
      "imagesSet",
      failures,
    );
    await this.verifyAbsent(
      policy,
      "DescribeSnapshots",
      "SnapshotId",
      ledger.snapshotIds,
      "snapshotSet",
      failures,
    );
    await this.verifyAbsent(
      policy,
      "DescribeVolumes",
      "VolumeId",
      ledger.volumeIds,
      "volumeSet",
      failures,
    );
    await this.verifyKeyPairsAbsent(ledger, policy, failures);
    const group = await this.signer.execute("ec2", "DescribeSecurityGroups", policy.region, {
      "GroupId.1": policy.securityGroupId,
    });
    if (!(await boundedResponse(group)).body.includes(policy.securityGroupId)) {
      failures.push("preprovisioned security group is missing");
    }
  }

  private async verifyKeyPairsAbsent(
    ledger: AWSQualificationLedger,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    const parameters =
      ledger.keyPairIds.length > 0
        ? Object.fromEntries(ledger.keyPairIds.map((id, index) => [`KeyPairId.${index + 1}`, id]))
        : Object.fromEntries(
            ledger.keyPairNames.map((name, index) => [`KeyName.${index + 1}`, name]),
          );
    if (Object.keys(parameters).length === 0) return;
    try {
      const response = await this.signer.execute(
        "ec2",
        "DescribeKeyPairs",
        policy.region,
        parameters,
      );
      const result = await boundedResponse(response);
      if (result.status >= 300) {
        if (result.body.includes("NotFound")) return;
        failures.push(`DescribeKeyPairs verification http ${result.status}`);
        return;
      }
      const root = awsXMLRoot(result.body, "DescribeKeyPairs");
      if (items(record(root["keySet"])["item"]).length > 0) {
        failures.push("DescribeKeyPairs still returns run-owned resources");
      }
    } catch (error) {
      failures.push(
        `DescribeKeyPairs verification: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  private async verifyAbsent(
    policy: AWSQualificationPolicy,
    action: string,
    idField: string,
    ids: string[],
    resultField: string,
    failures: string[],
  ): Promise<void> {
    if (ids.length === 0) return;
    const parameters = Object.fromEntries(ids.map((id, index) => [`${idField}.${index + 1}`, id]));
    try {
      const response = await this.signer.execute("ec2", action, policy.region, parameters);
      const result = await boundedResponse(response);
      if (result.status >= 300) {
        if (result.body.includes("NotFound")) return;
        failures.push(`${action} verification http ${result.status}`);
        return;
      }
      const root = awsXMLRoot(result.body, action);
      const entries = items(record(root[resultField])["item"]).map(record);
      const remaining =
        action === "DescribeInstances"
          ? entries
              .flatMap((reservation) =>
                items(record(reservation["instancesSet"])["item"]).map(record),
              )
              .filter(
                (instance) => asString(record(instance["instanceState"])["name"]) !== "terminated",
              )
          : entries;
      if (remaining.length > 0) {
        failures.push(`${action} still returns run-owned resources`);
      }
    } catch (error) {
      failures.push(
        `${action} verification: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  private async ledger(): Promise<AWSQualificationLedger> {
    return (await this.ctx.storage.get<AWSQualificationLedger>(ledgerKey)) ?? emptyLedger();
  }

  private async serialized<T>(operation: () => Promise<T>): Promise<T> {
    const next = this.serial.then(operation, operation);
    this.serial = next.then(
      () => undefined,
      () => undefined,
    );
    return await next;
  }
}

class DirectAuthoritySigner implements AuthoritySigner {
  constructor(private readonly env: AWSQualificationAuthorityEnv) {}

  async execute(
    service: AWSQualificationService,
    action: string,
    region: string,
    parameters: Record<string, unknown>,
  ): Promise<Response> {
    const credentials = authorityCredentials(this.env);
    const client = new AwsClient({ ...credentials, service, region, retries: 0 });
    if (service === "ec2" || service === "sts") {
      const version = service === "ec2" ? ec2Version : stsVersion;
      const body = new URLSearchParams({ Action: action, Version: version });
      for (const [key, value] of Object.entries(parameters)) {
        if (typeof value !== "string")
          throw new Error(`AWS query parameter ${key} is not a string`);
        body.set(key, value);
      }
      return await client.fetch(`https://${service}.${region}.amazonaws.com/`, {
        method: "POST",
        headers: { "content-type": "application/x-www-form-urlencoded; charset=utf-8" },
        body: body.toString(),
      });
    }
    return await client.fetch(`https://${service}.${region}.amazonaws.com/`, {
      method: "POST",
      headers: {
        "content-type": "application/x-amz-json-1.1",
        "x-amz-target": `ServiceQuotasV20190624.${action}`,
      },
      body: JSON.stringify(parameters),
    });
  }
}

async function authorizeRequest(
  request: AWSQualificationRequest,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
  physicalKeyName: string,
  clientToken: string,
): Promise<{ mutating: boolean; parameters: Record<string, unknown> }> {
  if (request.service === "sts") {
    if (request.action !== "GetCallerIdentity" || Object.keys(request.parameters).length > 0) {
      throw new Error("AWS qualification STS action is not allowed");
    }
    return { mutating: false, parameters: {} };
  }
  if (request.service === "servicequotas") {
    if (
      request.action !== "GetServiceQuota" ||
      request.parameters["ServiceCode"] !== "ec2" ||
      request.parameters["QuotaCode"] !== onDemandQuotaCode
    ) {
      throw new Error("AWS qualification Service Quotas action is not allowed");
    }
    return { mutating: false, parameters: structuredClone(request.parameters) };
  }
  if (request.service !== "ec2" || !allowedEC2Actions.has(request.action)) {
    if (request.action.includes("FastSnapshotRestore")) {
      throw new Error("AWS qualification fast snapshot restore is disabled");
    }
    throw new Error("AWS qualification action is not allowed");
  }
  return {
    mutating: mutatingEC2Actions.has(request.action),
    parameters: authorizeEC2(request, identity, policy, ledger, physicalKeyName, clientToken),
  };
}

function authorizeEC2(
  request: AWSQualificationRequest,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
  physicalKeyName: string,
  clientToken: string,
): Record<string, unknown> {
  const input = stringParameters(request.parameters);
  switch (request.action) {
    case "DescribeSecurityGroups":
      requireExact(input["GroupId.1"], policy.securityGroupId, "security group");
      return { "GroupId.1": policy.securityGroupId };
    case "ImportKeyPair": {
      const material = input["PublicKeyMaterial"] ?? "";
      if (!/^crabbox-[A-Za-z0-9._-]{1,180}$/.test(input["KeyName"] ?? "")) {
        throw new Error("AWS qualification key pair name is outside policy");
      }
      decodePublicKeyMaterial(material);
      return {
        KeyName: physicalKeyName,
        PublicKeyMaterial: material,
        ...qualificationTags(input, "key-pair", identity, request.opId),
      };
    }
    case "DescribeKeyPairs": {
      if (input["IncludePublicKey"] !== undefined && input["IncludePublicKey"] !== "true") {
        throw new Error("AWS qualification key read must include public key material");
      }
      const ids = indexedValues(input, "KeyPairId");
      if (ids.length > 0) return authorizedIDs(input, "KeyPairId", ledger.keyPairIds);
      if (!input["KeyName.1"] || indexedValues(input, "KeyName").length !== 1) {
        throw new Error("AWS qualification key read is outside policy");
      }
      return { IncludePublicKey: "true", "KeyName.1": physicalKeyName };
    }
    case "DeleteKeyPair":
      return authorizedSingleID(input, "KeyPairId", ledger.keyPairIds);
    case "RunInstances":
      return authorizedRunInstances(
        input,
        clientToken,
        request.opId,
        identity,
        policy,
        ledger,
        physicalKeyName,
      );
    case "DescribeInstances":
      return authorizedDescribe(input, "InstanceId", ledger.instanceIds, identity.runId);
    case "DescribeVolumes":
      return authorizedDescribe(input, "VolumeId", ledger.volumeIds, identity.runId);
    case "DescribeImages":
      return authorizedImageRead(input, policy.baseAmiId, ledger.imageIds, identity.runId);
    case "DescribeSnapshots":
      return authorizedDescribe(input, "SnapshotId", ledger.snapshotIds, identity.runId, true);
    case "TerminateInstances":
      return authorizedIDs(input, "InstanceId", ledger.instanceIds);
    case "DeregisterImage":
      return authorizedSingleID(input, "ImageId", ledger.imageIds);
    case "DeleteSnapshot":
      return authorizedSingleID(input, "SnapshotId", ledger.snapshotIds);
    case "CreateImage": {
      requireOwned(input["InstanceId"], ledger.instanceIds, "instance");
      const name = boundedName(input["Name"]);
      return {
        InstanceId: input["InstanceId"],
        Name: name,
        NoReboot: input["NoReboot"] === "true" ? "true" : "false",
        ...qualificationTags(input, "image", identity, request.opId),
        ...qualificationTags(input, "snapshot", identity, request.opId, 2),
      };
    }
    case "CreateTags": {
      const resourceIds = indexedValues(input, "ResourceId");
      if (resourceIds.length === 0 || resourceIds.some((id) => !allLedgerIDs(ledger).has(id))) {
        throw new Error("AWS qualification tags target a resource outside the run ledger");
      }
      return {
        ...Object.fromEntries(resourceIds.map((id, index) => [`ResourceId.${index + 1}`, id])),
        ...plainTags(input, identity, request.opId),
      };
    }
    default:
      throw new Error("AWS qualification EC2 action is not implemented");
  }
}

function authorizedRunInstances(
  input: Record<string, string>,
  clientToken: string,
  opId: string,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
  physicalKeyName: string,
): Record<string, unknown> {
  for (const key of Object.keys(input)) {
    if (
      key.startsWith("IamInstanceProfile") ||
      key.startsWith("InstanceMarketOptions") ||
      key.startsWith("Placement.Host") ||
      key.includes("FastSnapshotRestore")
    ) {
      throw new Error(`AWS qualification RunInstances parameter is forbidden: ${key}`);
    }
  }
  const imageId = input["ImageId"] ?? "";
  if (imageId !== policy.baseAmiId && !ledger.imageIds.includes(imageId)) {
    throw new Error("AWS qualification image is outside the run ledger");
  }
  const instanceType = input["InstanceType"] ?? "";
  if (
    !awsQualificationInstanceTypes.includes(
      instanceType as (typeof awsQualificationInstanceTypes)[number],
    )
  ) {
    throw new Error("AWS qualification instance type is outside policy");
  }
  if (!/^crabbox-[A-Za-z0-9._-]{1,180}$/.test(input["KeyName"] ?? "")) {
    throw new Error("AWS qualification key pair name is outside policy");
  }
  requireOwned(physicalKeyName, ledger.keyPairNames, "key pair");
  requireExact(input["NetworkInterface.1.SubnetId"], policy.subnetId, "subnet");
  requireExact(
    input["NetworkInterface.1.SecurityGroupId.1"],
    policy.securityGroupId,
    "security group",
  );
  const rootGB = Number(input["BlockDeviceMapping.1.Ebs.VolumeSize"]);
  if (!Number.isInteger(rootGB) || rootGB < 8 || rootGB > policy.rootGB) {
    throw new Error("AWS qualification root volume is outside policy");
  }
  const userData = input["UserData"] ?? "";
  if (new TextEncoder().encode(userData).byteLength > maxUserDataBytes) {
    throw new Error("AWS qualification user data is too large");
  }
  return {
    ClientToken: clientToken,
    ImageId: imageId,
    InstanceType: instanceType,
    KeyName: physicalKeyName,
    MaxCount: "1",
    MinCount: "1",
    UserData: userData,
    "BlockDeviceMapping.1.DeviceName": "/dev/sda1",
    "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
    "BlockDeviceMapping.1.Ebs.Encrypted": "true",
    "BlockDeviceMapping.1.Ebs.VolumeSize": String(rootGB),
    "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
    "MetadataOptions.HttpEndpoint": "enabled",
    "MetadataOptions.HttpPutResponseHopLimit": "1",
    "MetadataOptions.HttpTokens": "required",
    "MetadataOptions.InstanceMetadataTags": "disabled",
    "NetworkInterface.1.AssociatePublicIpAddress": "true",
    "NetworkInterface.1.DeleteOnTermination": "true",
    "NetworkInterface.1.DeviceIndex": "0",
    "NetworkInterface.1.SecurityGroupId.1": policy.securityGroupId,
    "NetworkInterface.1.SubnetId": policy.subnetId,
    ...qualificationTags(input, "instance", identity, opId),
    ...qualificationTags(input, "volume", identity, opId, 2),
  };
}

function authorizedDescribe(
  input: Record<string, string>,
  field: string,
  owned: string[],
  runId: string,
  ownerSelf = false,
): Record<string, unknown> {
  const ids = indexedValues(input, field);
  if (ids.length > 0) return authorizedIDs(input, field, owned);
  const filters = qualificationReadFilters(input, runId);
  return { ...filters, ...(ownerSelf ? { "Owner.1": "self" } : {}) };
}

function authorizedImageRead(
  input: Record<string, string>,
  baseAmiId: string,
  owned: string[],
  runId: string,
): Record<string, unknown> {
  const ids = indexedValues(input, "ImageId");
  if (ids.length > 0) {
    if (ids.some((id) => id !== baseAmiId && !owned.includes(id))) {
      throw new Error("AWS qualification image read is outside the run ledger");
    }
    return Object.fromEntries(ids.map((id, index) => [`ImageId.${index + 1}`, id]));
  }
  return { ...qualificationReadFilters(input, runId), "Owner.1": "self" };
}

function qualificationReadFilters(
  input: Record<string, string>,
  runId: string,
): Record<string, unknown> {
  const filters: Record<string, unknown> = {};
  let next = 1;
  for (let index = 1; index <= 16; index += 1) {
    const name = input[`Filter.${index}.Name`];
    if (!name) continue;
    if (!name.startsWith("tag:") && name !== "name" && name !== "instance-state-name") {
      throw new Error("AWS qualification read filter is outside policy");
    }
    filters[`Filter.${next}.Name`] = name;
    for (let valueIndex = 1; valueIndex <= 16; valueIndex += 1) {
      const value = input[`Filter.${index}.Value.${valueIndex}`];
      if (value !== undefined) filters[`Filter.${next}.Value.${valueIndex}`] = value;
    }
    next += 1;
  }
  filters[`Filter.${next}.Name`] = "tag:crabbox_qualification_run";
  filters[`Filter.${next}.Value.1`] = runId;
  return filters;
}

function qualificationTags(
  input: Record<string, string>,
  resourceType: string,
  identity: AWSQualificationRunIdentity,
  opId: string,
  specificationIndex = 1,
): Record<string, string> {
  const output: Record<string, string> = {
    [`TagSpecification.${specificationIndex}.ResourceType`]: resourceType,
  };
  const sourceIndex = findTagSpecification(input, resourceType);
  const tags = sourceIndex ? tagMap(input, `TagSpecification.${sourceIndex}.Tag`) : new Map();
  injectAuthorityTags(tags, identity, opId);
  let index = 1;
  for (const [key, value] of tags) {
    output[`TagSpecification.${specificationIndex}.Tag.${index}.Key`] = key;
    output[`TagSpecification.${specificationIndex}.Tag.${index}.Value`] = value;
    index += 1;
  }
  return output;
}

function plainTags(
  input: Record<string, string>,
  identity: AWSQualificationRunIdentity,
  opId: string,
): Record<string, string> {
  const tags = tagMap(input, "Tag");
  injectAuthorityTags(tags, identity, opId);
  return Object.fromEntries(
    [...tags].flatMap(([key, value], index) => [
      [`Tag.${index + 1}.Key`, key],
      [`Tag.${index + 1}.Value`, value],
    ]),
  );
}

function injectAuthorityTags(
  tags: Map<string, string>,
  identity: AWSQualificationRunIdentity,
  opId: string,
): void {
  tags.set("crabbox_qualification_owner", boundedText(identity.owner, 256));
  tags.set("crabbox_qualification_run", identity.runId);
  tags.set("crabbox_qualification_sha", identity.candidateSha);
  tags.set("crabbox_qualification_expiry", identity.expiresAt);
  tags.set("crabbox_qualification_op", opId);
}

function tagMap(input: Record<string, string>, prefix: string): Map<string, string> {
  const tags = new Map<string, string>();
  for (let index = 1; index <= 64; index += 1) {
    const key = input[`${prefix}.${index}.Key`];
    const value = input[`${prefix}.${index}.Value`];
    if (!key || value === undefined || !preservedTagKeys.has(key)) continue;
    tags.set(key, boundedText(value, 256));
  }
  return tags;
}

function findTagSpecification(input: Record<string, string>, resourceType: string): number {
  for (let index = 1; index <= 8; index += 1) {
    if (input[`TagSpecification.${index}.ResourceType`] === resourceType) return index;
  }
  return 0;
}

function updateLedgerFromResponse(
  ledger: AWSQualificationLedger,
  action: string,
  body: string,
  parameters: Record<string, unknown>,
): void {
  const root = awsXMLRoot(body, action);
  if (action === "RunInstances") {
    const reservations = [{ instancesSet: root["instancesSet"] }];
    for (const reservation of reservations) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"]).map(
        record,
      )) {
        const instanceId = asString(instance["instanceId"]);
        if (instanceId) ledger.instanceIds = [instanceId];
        for (const mapping of items(record(instance["blockDeviceMapping"])["item"]).map(record)) {
          const volumeId = asString(record(mapping["ebs"])["volumeId"]);
          if (volumeId) ledger.volumeIds = [volumeId];
        }
      }
    }
  }
  if (action === "DescribeInstances") {
    const described = items(record(root["reservationSet"])["item"])
      .flatMap((reservation) => items(record(record(reservation)["instancesSet"])["item"]))
      .map(record);
    const states = new Map(
      described
        .map(
          (instance) =>
            [
              asString(instance["instanceId"]),
              asString(record(instance["instanceState"])["name"]),
            ] as const,
        )
        .filter(([instanceId]) => instanceId),
    );
    const requested = indexedUnknownValues(parameters, "InstanceId");
    ledger.instanceIds = ledger.instanceIds.filter((instanceId) => {
      if (!requested.includes(instanceId)) return true;
      const state = states.get(instanceId);
      return state !== undefined && state !== "terminated";
    });
  }
  if (action === "DeregisterImage") {
    ledger.imageIds = ledger.imageIds.filter((id) => id !== parameters["ImageId"]);
  }
  if (action === "DeleteSnapshot") {
    ledger.snapshotIds = ledger.snapshotIds.filter((id) => id !== parameters["SnapshotId"]);
  }
  if (action === "DeleteKeyPair") {
    ledger.keyPairIds = ledger.keyPairIds.filter((id) => id !== parameters["KeyPairId"]);
    if (ledger.keyPairIds.length === 0) ledger.keyPairNames = [];
  }
  if (action === "DescribeImages") {
    for (const image of items(record(root["imagesSet"])["item"]).map(record)) {
      const imageId = asString(image["imageId"]);
      if (!ledger.imageIds.includes(imageId)) continue;
      for (const mapping of items(record(image["blockDeviceMapping"])["item"]).map(record)) {
        const snapshotId = asString(record(mapping["ebs"])["snapshotId"]);
        if (snapshotId) ledger.snapshotIds = [snapshotId];
      }
    }
  }
}

function validateRunIdentity(identity: AWSQualificationRunIdentity): void {
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(identity.runId)) {
    throw new Error("AWS qualification run id is malformed");
  }
  if (!/^[0-9a-f]{40}$/.test(identity.candidateSha)) {
    throw new Error("AWS qualification candidate SHA must be exact");
  }
  if (!/^[0-9a-f]{64}$/.test(identity.deploymentHash)) {
    throw new Error("AWS qualification deployment hash must be exact");
  }
  if (!identity.owner.trim() || identity.owner.length > 256) {
    throw new Error("AWS qualification owner is malformed");
  }
  if (!Number.isFinite(Date.parse(identity.expiresAt))) {
    throw new Error("AWS qualification expiry is malformed");
  }
}

function validateRequestShape(
  request: AWSQualificationRequest,
  region: string,
): AWSQualificationRequest {
  if (!request || typeof request !== "object" || Array.isArray(request)) {
    throw new Error("AWS qualification request is malformed");
  }
  validateBoundedRequestInput(request);
  const unknownKeys = Object.keys(request).filter((key) => !requestKeys.has(key));
  if (unknownKeys.length > 0) {
    throw new Error(`AWS qualification request has unknown field: ${unknownKeys[0]}`);
  }
  if (typeof request.opId !== "string" || !/^[0-9a-f-]{36}$/.test(request.opId)) {
    throw new Error("AWS qualification opId is malformed");
  }
  if (request.region !== region) throw new Error("AWS qualification region is outside policy");
  if (typeof request.action !== "string" || !request.action || request.action.length > 128) {
    throw new Error("AWS qualification action is malformed");
  }
  if (
    request.service !== "ec2" &&
    request.service !== "servicequotas" &&
    request.service !== "sts"
  ) {
    throw new Error("AWS qualification service is malformed");
  }
  const parameters = flatStringMap(request.parameters);
  const normalized = {
    opId: request.opId,
    region: request.region,
    service: request.service,
    action: request.action,
    parameters,
  } satisfies AWSQualificationRequest;
  const encoded = new TextEncoder().encode(canonicalJSON(normalized));
  if (encoded.byteLength > maxRequestBytes) {
    throw new Error("AWS qualification request exceeds 64 KiB");
  }
  return normalized;
}

function validateController(
  controller: AWSQualificationControllerProps,
  deploymentHash: string,
): void {
  if (!/^[0-9a-f]{64}$/.test(controller.deploymentHash)) {
    throw new Error("AWS qualification controller deployment hash is malformed");
  }
  if (controller.deploymentHash !== deploymentHash) {
    throw new Error("AWS qualification controller is not bound to this deployment");
  }
}

async function qualificationPolicyHash(policy: AWSQualificationPolicy): Promise<string> {
  return await sha256Hex(canonicalJSON(policy));
}

async function qualificationPhysicalKeyName(runId: string): Promise<string> {
  return `cbxq-${(await sha256Hex(`key:${runId}`)).slice(0, 32)}`;
}

async function qualificationClientToken(runId: string, opId: string): Promise<string> {
  return `cbxq-${(await sha256Hex(`instance:${runId}:${opId}`)).slice(0, 59)}`;
}

function authorityPolicy(env: AWSQualificationAuthorityEnv): AWSQualificationPolicy {
  const accountId = env.CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID?.trim() ?? "";
  const baseAmiId = env.CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID?.trim() ?? "";
  const region = requireAWSRegion(env.CRABBOX_AWS_QUALIFICATION_REGION ?? "");
  const securityGroupId = env.CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID?.trim() ?? "";
  const subnetId = env.CRABBOX_AWS_QUALIFICATION_SUBNET_ID?.trim() ?? "";
  const rootGB = Number(env.CRABBOX_AWS_QUALIFICATION_ROOT_GB ?? maxRootGB);
  if (!/^\d{12}$/.test(accountId)) throw new Error("AWS qualification account id is malformed");
  if (!/^ami-[a-z0-9]+$/.test(baseAmiId))
    throw new Error("AWS qualification base AMI is malformed");
  if (!/^sg-[a-z0-9]+$/.test(securityGroupId)) {
    throw new Error("AWS qualification security group is malformed");
  }
  if (!/^subnet-[a-z0-9]+$/.test(subnetId)) {
    throw new Error("AWS qualification subnet is malformed");
  }
  if (!Number.isInteger(rootGB) || rootGB < 8 || rootGB > maxRootGB) {
    throw new Error("AWS qualification root volume limit must be 8 through 20 GiB");
  }
  return { accountId, baseAmiId, region, rootGB, securityGroupId, subnetId };
}

function authorityCredentials(env: AWSQualificationAuthorityEnv): AWSCredentials {
  const accessKeyId = env.AWS_ACCESS_KEY_ID?.trim() ?? "";
  const secretAccessKey = env.AWS_SECRET_ACCESS_KEY?.trim() ?? "";
  if (!accessKeyId || !secretAccessKey) throw new Error("AWS authority credentials are missing");
  const sessionToken = env.AWS_SESSION_TOKEN?.trim();
  return { accessKeyId, secretAccessKey, ...(sessionToken ? { sessionToken } : {}) };
}

function qualificationRun(
  env: AWSQualificationAuthorityEnv,
  runId: string,
): DurableObjectStub<AWSQualificationRun> {
  return env.AWS_QUALIFICATION_RUNS.get(env.AWS_QUALIFICATION_RUNS.idFromName(runId));
}

function stringParameters(input: Record<string, unknown>): Record<string, string> {
  const output: Record<string, string> = {};
  for (const [key, value] of Object.entries(input)) {
    if (typeof value !== "string" || key.length > 256 || value.length > maxRequestBytes) {
      throw new Error(`AWS qualification parameter is malformed: ${key}`);
    }
    output[key] = value;
  }
  return output;
}

function flatStringMap(value: unknown): Record<string, string> {
  if (!isPlainRecord(value)) {
    throw new Error("AWS qualification parameters must be a flat string map");
  }
  const entries = Object.entries(value);
  if (entries.length > 256) {
    throw new Error("AWS qualification request has too many parameters");
  }
  const output: Record<string, string> = {};
  for (const [key, entry] of entries) {
    if (typeof entry !== "string" || key.length > 256) {
      throw new Error(`AWS qualification parameter is malformed: ${key}`);
    }
    if (new TextEncoder().encode(entry).byteLength > maxRequestBytes) {
      throw new Error("AWS qualification request exceeds 64 KiB");
    }
    output[key] = entry;
  }
  return output;
}

function validateBoundedRequestInput(value: unknown): void {
  const pending: Array<{ depth: number; value: unknown }> = [{ depth: 0, value }];
  const seen = new WeakSet<object>();
  let bytes = 2;
  let nodes = 0;
  for (let index = 0; index < pending.length; index += 1) {
    const entry = pending[index]!;
    if (typeof entry.value === "string") {
      bytes += boundedUTF8Length(entry.value);
    } else if (
      entry.value === null ||
      typeof entry.value === "boolean" ||
      typeof entry.value === "number"
    ) {
      bytes += String(entry.value).length;
    } else if (typeof entry.value === "object" && !Array.isArray(entry.value)) {
      if (seen.has(entry.value)) {
        throw new Error("AWS qualification request contains a cycle");
      }
      if (entry.depth > 1) {
        throw new Error("AWS qualification request nesting exceeds policy");
      }
      seen.add(entry.value);
      const children = Object.entries(entry.value);
      nodes += children.length;
      if (nodes > maxRequestNodes) {
        throw new Error("AWS qualification request is too complex");
      }
      for (const [key, child] of children) {
        bytes += boundedUTF8Length(key) + 3;
        pending.push({ depth: entry.depth + 1, value: child });
      }
    } else {
      throw new Error("AWS qualification request contains an unsupported value");
    }
    if (bytes > maxRequestBytes) {
      throw new Error("AWS qualification request exceeds 64 KiB");
    }
  }
}

function boundedUTF8Length(value: string): number {
  if (value.length > maxRequestBytes) return maxRequestBytes + 1;
  return new TextEncoder().encode(value).byteLength;
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function authorizedIDs(
  input: Record<string, string>,
  field: string,
  owned: string[],
): Record<string, unknown> {
  const ids = indexedValues(input, field);
  if (ids.length === 0 || ids.some((id) => !owned.includes(id))) {
    throw new Error(`AWS qualification ${field} is outside the run ledger`);
  }
  return Object.fromEntries(ids.map((id, index) => [`${field}.${index + 1}`, id]));
}

function authorizedSingleID(
  input: Record<string, string>,
  field: string,
  owned: string[],
): Record<string, unknown> {
  requireOwned(input[field], owned, field);
  return { [field]: input[field] };
}

function indexedValues(input: Record<string, string>, field: string): string[] {
  const values: string[] = [];
  for (let index = 1; index <= 128; index += 1) {
    const value = input[`${field}.${index}`];
    if (value) values.push(value);
  }
  return values;
}

function indexedUnknownValues(input: Record<string, unknown>, field: string): string[] {
  const values: string[] = [];
  for (let index = 1; index <= 128; index += 1) {
    const value = input[`${field}.${index}`];
    if (typeof value === "string" && value) values.push(value);
  }
  return values;
}

function requireOwned(value: string | undefined, owned: string[], label: string): void {
  if (!value || !owned.includes(value)) {
    throw new Error(`AWS qualification ${label} is outside the run ledger`);
  }
}

function requireExact(value: string | undefined, expected: string, label: string): void {
  if (value !== expected) throw new Error(`AWS qualification ${label} is outside policy`);
}

function boundedName(value: string | undefined): string {
  const name = boundedText(value, 128);
  if (!name || !/^[A-Za-z0-9() ./_-]+$/.test(name)) {
    throw new Error("AWS qualification image name is malformed");
  }
  return name;
}

function boundedText(value: string | undefined, maximum: number): string {
  const text = value ?? "";
  if (text.length > maximum) throw new Error("AWS qualification text exceeds policy");
  return text;
}

function allLedgerIDs(ledger: AWSQualificationLedger): Set<string> {
  return new Set([
    ...ledger.imageIds,
    ...ledger.instanceIds,
    ...ledger.keyPairIds,
    ...ledger.snapshotIds,
    ...ledger.volumeIds,
  ]);
}

function emptyLedger(launchCount = 0): AWSQualificationLedger {
  return {
    imageIds: [],
    instanceIds: [],
    keyPairIds: [],
    keyPairNames: [],
    snapshotIds: [],
    volumeIds: [],
    launchCount,
  };
}

function mergeLedgers(
  left: AWSQualificationLedger,
  right: AWSQualificationLedger,
): AWSQualificationLedger {
  return {
    imageIds: [...new Set([...left.imageIds, ...right.imageIds])],
    instanceIds: [...new Set([...left.instanceIds, ...right.instanceIds])],
    keyPairIds: [...new Set([...left.keyPairIds, ...right.keyPairIds])],
    keyPairNames: [...new Set([...left.keyPairNames, ...right.keyPairNames])],
    snapshotIds: [...new Set([...left.snapshotIds, ...right.snapshotIds])],
    volumeIds: [...new Set([...left.volumeIds, ...right.volumeIds])],
    launchCount: Math.max(left.launchCount, right.launchCount),
  };
}

function awsXMLRoot(body: string, action: string): Record<string, unknown> {
  const parsed = record(parser.parse(body));
  return record(parsed[`${action}Response`] ?? parsed["Response"] ?? parsed);
}

async function boundedResponse(response: Response): Promise<AWSQualificationResponse> {
  const body = await response.text();
  if (new TextEncoder().encode(body).byteLength > maxResponseBytes) {
    throw new Error("AWS qualification response exceeds 64 KiB");
  }
  return { status: response.status, body };
}

function assertLifecycleCapacity(
  request: AWSQualificationRequest,
  ledger: AWSQualificationLedger,
  pending: Map<string, AWSQualificationIntent>,
): void {
  const pendingActions = new Set([...pending.values()].map((intent) => intent.request.action));
  if (request.action === "RunInstances") {
    if (ledger.instanceIds.length >= maxActiveInstances || pendingActions.has("RunInstances")) {
      throw new Error("AWS qualification allows one active instance");
    }
    if (ledger.launchCount >= maxLaunches) {
      throw new Error("AWS qualification launch budget is exhausted");
    }
  }
  if (
    request.action === "CreateImage" &&
    (ledger.imageIds.length >= maxActiveImages ||
      ledger.snapshotIds.length >= maxActiveSnapshots ||
      pendingActions.has("CreateImage"))
  ) {
    throw new Error("AWS qualification allows one active checkpoint image");
  }
  if (
    request.action === "ImportKeyPair" &&
    (ledger.keyPairIds.length > 0 || pendingActions.has("ImportKeyPair"))
  ) {
    throw new Error("AWS qualification allows one active key pair");
  }
}

function hasOwnedResources(ledger: AWSQualificationLedger): boolean {
  return (
    ledger.imageIds.length > 0 ||
    ledger.instanceIds.length > 0 ||
    ledger.keyPairIds.length > 0 ||
    ledger.snapshotIds.length > 0 ||
    ledger.volumeIds.length > 0
  );
}

function decodePublicKeyMaterial(material: string): string {
  if (!material || material.length > 24 * 1024 || !/^[A-Za-z0-9+/]+={0,2}$/.test(material)) {
    throw new Error("AWS qualification public key material is outside policy");
  }
  let decoded = "";
  try {
    decoded = atob(material);
  } catch {
    throw new Error("AWS qualification public key material is not base64");
  }
  if (
    new TextEncoder().encode(decoded).byteLength > 16 * 1024 ||
    !/^ssh-(?:ed25519|rsa) [A-Za-z0-9+/]+={0,3}(?: |$)/.test(decoded)
  ) {
    throw new Error("AWS qualification decoded public key is outside policy");
  }
  return decoded;
}

function publicKeyIdentity(value: string): string {
  return value.trim().split(/\s+/).slice(0, 2).join(" ");
}

function awsTagMap(value: unknown): Map<string, string> {
  return new Map(
    items(record(value)["item"])
      .map(record)
      .map((tag) => [asString(tag["key"]), asString(tag["value"])] as const)
      .filter(([key]) => key),
  );
}

function imageSnapshotIDs(image: Record<string, unknown>): string[] {
  return items(record(image["blockDeviceMapping"])["item"])
    .map(record)
    .map((mapping) => asString(record(mapping["ebs"])["snapshotId"]))
    .filter(Boolean);
}

function reservationsFromXML(body: string): Record<string, unknown>[] {
  return items(record(awsXMLRoot(body, "DescribeInstances")["reservationSet"])["item"]).map(record);
}

function canonicalJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as Record<string, unknown>)
      .toSorted(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJSON(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value) ?? "null";
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function items(value: unknown): unknown[] {
  if (Array.isArray(value)) return value;
  return value === undefined ? [] : [value];
}

function asString(value: unknown): string {
  return typeof value === "string"
    ? value
    : value === undefined || value === null
      ? ""
      : String(value);
}

function xmlEscape(value: string): string {
  return value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

function isRetryableReconciliationError(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return (
    message.includes("not uniquely visible") ||
    message.includes("could not be verified") ||
    message.includes("verification failed: http 5")
  );
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
