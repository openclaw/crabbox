import { AwsClient } from "aws4fetch";
import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import { XMLParser } from "fast-xml-parser";

import { sha256Hex } from "./auth";
import {
  awsQualificationInstanceTypes,
  awsQualificationMaxRunMs,
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
const maxUserDataBytes = 256 * 1024;
const parser = new XMLParser({ ignoreAttributes: false });

const allowedEC2Actions = new Set([
  "CreateImage",
  "CreateSnapshot",
  "CreateTags",
  "DeleteKeyPair",
  "DeleteSnapshot",
  "DeregisterImage",
  "DescribeImages",
  "DescribeInstances",
  "DescribeSecurityGroups",
  "DescribeSnapshots",
  "DescribeVolumes",
  "ImportKeyPair",
  "RunInstances",
  "TerminateInstances",
]);

const mutatingEC2Actions = new Set([
  "CreateImage",
  "CreateSnapshot",
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
  finalizedAt?: string;
}

interface AWSQualificationLedger {
  imageIds: string[];
  instanceIds: string[];
  keyPairIds: string[];
  keyPairNames: string[];
  snapshotIds: string[];
  volumeIds: string[];
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

export default class AWSQualificationControlPlane extends WorkerEntrypoint<AWSQualificationAuthorityEnv> {
  async enroll(identity: AWSQualificationRunIdentity): Promise<void> {
    await qualificationRun(this.env, identity.runId).enroll(identity);
  }

  async finalize(runId: string): Promise<AWSQualificationLedger> {
    return await qualificationRun(this.env, runId).finalize();
  }
}

export class AWSQualificationTransport extends WorkerEntrypoint<
  AWSQualificationAuthorityEnv,
  AWSQualificationRunIdentity
> {
  async execute(request: AWSQualificationRequest): Promise<AWSQualificationResponse> {
    return await qualificationRun(this.env, this.ctx.props.runId).execute(this.ctx.props, request);
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

  async enroll(identity: AWSQualificationRunIdentity): Promise<void> {
    return await this.serialized(async () => {
      const policy = authorityPolicy(this.env);
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
        if (canonicalJSON(existing.identity) !== canonicalJSON(identity)) {
          throw new Error("AWS qualification run identity is already enrolled");
        }
        return;
      }
      await this.ctx.storage.put({
        [stateKey]: {
          identity: structuredClone(identity),
          enrolledAt: new Date(now).toISOString(),
          accountVerified: false,
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

  async finalize(): Promise<AWSQualificationLedger> {
    return await this.serialized(() => this.finalizeSerialized());
  }

  override async alarm(): Promise<void> {
    await this.finalize();
  }

  private async executeSerialized(
    identity: AWSQualificationRunIdentity,
    request: AWSQualificationRequest,
  ): Promise<AWSQualificationResponse> {
    const run = await this.requireActiveRun(identity);
    const policy = authorityPolicy(this.env);
    validateRequestShape(request, policy.region);
    const requestHash = await sha256Hex(
      canonicalJSON({
        action: request.action,
        parameters: request.parameters,
        region: request.region,
        service: request.service,
      }),
    );
    const receipt = await this.ctx.storage.get<AWSQualificationReceipt>(
      `${receiptPrefix}${request.opId}`,
    );
    if (receipt) {
      if (receipt.requestHash !== requestHash) {
        throw new Error("AWS qualification opId was replayed with a different request");
      }
      return receipt.response;
    }
    const priorIntent = await this.ctx.storage.get<AWSQualificationIntent>(
      `${intentPrefix}${request.opId}`,
    );
    if (priorIntent) {
      if (priorIntent.requestHash !== requestHash) {
        throw new Error("AWS qualification pending opId has a different request");
      }
      const reconciled = await this.reconcileIntent(run, priorIntent, policy);
      if (reconciled) return reconciled;
    }

    const ledger = await this.ledger();
    const authorized = await authorizeRequest(request, requestHash, identity, policy, ledger);
    if (request.service !== "sts") {
      await this.ensureAccount(run, policy);
    }
    if (authorized.mutating && !priorIntent) {
      await this.ctx.storage.put(`${intentPrefix}${request.opId}`, {
        requestHash,
        request: structuredClone(request),
        startedAt: new Date().toISOString(),
      } satisfies AWSQualificationIntent);
    }
    if (request.action === "ImportKeyPair") {
      addUnique(ledger.keyPairNames, String(authorized.parameters["KeyName"] ?? ""));
      await this.ctx.storage.put(ledgerKey, ledger);
    }

    const response = await this.signer.execute(
      request.service,
      request.action,
      policy.region,
      authorized.parameters,
    );
    const result = await boundedResponse(response);
    if (result.status >= 200 && result.status < 300) {
      updateLedgerFromResponse(ledger, request.action, result.body, authorized.parameters);
      await this.ctx.storage.put(ledgerKey, ledger);
    }
    await this.ctx.storage.put(`${receiptPrefix}${request.opId}`, {
      requestHash,
      response: result,
    } satisfies AWSQualificationReceipt);
    await this.ctx.storage.delete(`${intentPrefix}${request.opId}`);
    return result;
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
    if (intent.request.action !== "CreateImage" && intent.request.action !== "CreateSnapshot") {
      return undefined;
    }
    const resourceType = intent.request.action === "CreateImage" ? "image" : "snapshot";
    const action = resourceType === "image" ? "DescribeImages" : "DescribeSnapshots";
    const filterName = resourceType === "image" ? "imagesSet" : "snapshotSet";
    const idName = resourceType === "image" ? "imageId" : "snapshotId";
    const response = await this.signer.execute("ec2", action, policy.region, {
      "Filter.1.Name": "tag:crabbox_qualification_run",
      "Filter.1.Value.1": run.identity.runId,
      "Filter.2.Name": "tag:crabbox_qualification_op",
      "Filter.2.Value.1": intent.request.opId,
      "Owner.1": "self",
    });
    const result = await boundedResponse(response);
    if (result.status < 200 || result.status >= 300) return undefined;
    const root = awsXMLRoot(result.body, action);
    const matches = items(record(root[filterName])["item"]).map(record);
    if (matches.length === 0) return undefined;
    if (matches.length !== 1) {
      throw new Error(`AWS qualification ${resourceType} reconciliation is ambiguous`);
    }
    const id = asString(matches[0]![idName]);
    if (!id) throw new Error(`AWS qualification ${resourceType} reconciliation returned no id`);
    const synthetic =
      resourceType === "image"
        ? `<CreateImageResponse><imageId>${xmlEscape(id)}</imageId></CreateImageResponse>`
        : `<CreateSnapshotResponse><snapshotId>${xmlEscape(id)}</snapshotId><status>pending</status></CreateSnapshotResponse>`;
    const ledger = await this.ledger();
    addUnique(resourceType === "image" ? ledger.imageIds : ledger.snapshotIds, id);
    await this.ctx.storage.put(ledgerKey, ledger);
    const receipt = {
      requestHash: intent.requestHash,
      response: { status: 200, body: synthetic },
    } satisfies AWSQualificationReceipt;
    await this.ctx.storage.put(`${receiptPrefix}${intent.request.opId}`, receipt);
    await this.ctx.storage.delete(`${intentPrefix}${intent.request.opId}`);
    return receipt.response;
  }

  private async ensureAccount(
    run: AWSQualificationRunState,
    policy: AWSQualificationPolicy,
  ): Promise<void> {
    if (run.accountVerified) return;
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
    return run;
  }

  private async finalizeSerialized(): Promise<AWSQualificationLedger> {
    const run = await this.ctx.storage.get<AWSQualificationRunState>(stateKey);
    const ledger = await this.ledger();
    if (!run || run.finalizedAt) return ledger;
    const policy = authorityPolicy(this.env);
    await this.ensureAccount(run, policy);
    const failures: string[] = [];
    await this.recoverPendingIntents(run, policy, failures);
    const recoveredLedger = await this.ledger();
    for (const imageId of recoveredLedger.imageIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- teardown order protects dependent snapshots.
      await this.cleanup("DeregisterImage", { ImageId: imageId }, failures);
    }
    for (const snapshotId of recoveredLedger.snapshotIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each exact resource has independent evidence.
      await this.cleanup("DeleteSnapshot", { SnapshotId: snapshotId }, failures);
    }
    for (const instanceId of recoveredLedger.instanceIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- terminate only ledger-owned instances.
      await this.cleanup("TerminateInstances", { "InstanceId.1": instanceId }, failures);
    }
    for (const keyPairId of recoveredLedger.keyPairIds) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- delete only ledger-owned key pairs.
      await this.cleanup("DeleteKeyPair", { KeyPairId: keyPairId }, failures);
    }
    for (const keyName of recoveredLedger.keyPairNames) {
      if (recoveredLedger.keyPairIds.length > 0) break;
      // oxlint-disable-next-line eslint/no-await-in-loop -- legacy AWS responses may omit KeyPairId.
      await this.cleanup("DeleteKeyPair", { KeyName: keyName }, failures);
    }
    await this.verifyZeroResidue(recoveredLedger, policy, failures);
    if (failures.length > 0) {
      await this.ctx.storage.setAlarm(Date.now() + cleanupRetryMs);
      throw new Error(`AWS qualification cleanup incomplete: ${failures.join("; ")}`);
    }
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
    const pending = await this.ctx.storage.list<AWSQualificationIntent>({
      prefix: intentPrefix,
    });
    for (const [key, intent] of pending) {
      try {
        if (intent.request.action === "CreateImage" || intent.request.action === "CreateSnapshot") {
          // oxlint-disable-next-line eslint/no-await-in-loop -- each pending intent owns one resource.
          const reconciled = await this.reconcileIntent(run, intent, policy);
          if (!reconciled) failures.push(`${intent.request.action} intent is unresolved`);
          continue;
        }
        if (intent.request.action === "RunInstances") {
          // oxlint-disable-next-line eslint/no-await-in-loop -- each pending intent has a separate ledger snapshot.
          const ledger = await this.ledger();
          // oxlint-disable-next-line eslint/no-await-in-loop -- deterministic ClientToken reconciles one intent.
          const authorized = await authorizeRequest(
            intent.request,
            intent.requestHash,
            run.identity,
            policy,
            ledger,
          );
          // oxlint-disable-next-line eslint/no-await-in-loop -- one exact pending allocation is reconciled.
          const response = await this.signer.execute(
            "ec2",
            "RunInstances",
            policy.region,
            authorized.parameters,
          );
          // oxlint-disable-next-line eslint/no-await-in-loop -- response belongs to this pending intent.
          const result = await boundedResponse(response);
          if (result.status < 200 || result.status >= 300) {
            failures.push(`RunInstances reconciliation http ${result.status}`);
            continue;
          }
          updateLedgerFromResponse(ledger, "RunInstances", result.body, authorized.parameters);
          // oxlint-disable-next-line eslint/no-await-in-loop -- persist this intent before reconciling another.
          await this.ctx.storage.put(ledgerKey, ledger);
          // oxlint-disable-next-line eslint/no-await-in-loop -- receipt belongs to this exact operation.
          await this.ctx.storage.put(key.replace(intentPrefix, receiptPrefix), {
            requestHash: intent.requestHash,
            response: result,
          } satisfies AWSQualificationReceipt);
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- clear only the intent just reconciled.
        await this.ctx.storage.delete(key);
      } catch (error) {
        failures.push(
          `${intent.request.action} reconciliation: ${
            error instanceof Error ? error.message : String(error)
          }`,
        );
      }
    }
  }

  private async cleanup(
    action: string,
    parameters: Record<string, unknown>,
    failures: string[],
  ): Promise<void> {
    try {
      const response = await this.signer.execute(
        "ec2",
        action,
        authorityPolicy(this.env).region,
        parameters,
      );
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

  private async verifyZeroResidue(
    ledger: AWSQualificationLedger,
    policy: AWSQualificationPolicy,
    failures: string[],
  ): Promise<void> {
    await this.verifyAbsent(
      "DescribeInstances",
      "InstanceId",
      ledger.instanceIds,
      "reservationSet",
      failures,
    );
    await this.verifyAbsent("DescribeImages", "ImageId", ledger.imageIds, "imagesSet", failures);
    await this.verifyAbsent(
      "DescribeSnapshots",
      "SnapshotId",
      ledger.snapshotIds,
      "snapshotSet",
      failures,
    );
    await this.verifyAbsent("DescribeVolumes", "VolumeId", ledger.volumeIds, "volumeSet", failures);
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
    action: string,
    idField: string,
    ids: string[],
    resultField: string,
    failures: string[],
  ): Promise<void> {
    if (ids.length === 0) return;
    const parameters = Object.fromEntries(ids.map((id, index) => [`${idField}.${index + 1}`, id]));
    try {
      const response = await this.signer.execute(
        "ec2",
        action,
        authorityPolicy(this.env).region,
        parameters,
      );
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
    const client = new AwsClient({ ...credentials, service, region });
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
  requestHash: string,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
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
    parameters: authorizeEC2(request, requestHash, identity, policy, ledger),
  };
}

function authorizeEC2(
  request: AWSQualificationRequest,
  requestHash: string,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
): Record<string, unknown> {
  const input = stringParameters(request.parameters);
  switch (request.action) {
    case "DescribeSecurityGroups":
      requireExact(input["GroupId.1"], policy.securityGroupId, "security group");
      return { "GroupId.1": policy.securityGroupId };
    case "ImportKeyPair": {
      const name = input["KeyName"] ?? "";
      const material = input["PublicKeyMaterial"] ?? "";
      if (!/^crabbox-[A-Za-z0-9._-]{1,180}$/.test(name)) {
        throw new Error("AWS qualification key pair name is outside policy");
      }
      if (!material.startsWith("ssh-") || material.length > 16 * 1024) {
        throw new Error("AWS qualification public key is outside policy");
      }
      return {
        KeyName: name,
        PublicKeyMaterial: material,
        ...qualificationTags(input, "key-pair", identity, request.opId),
      };
    }
    case "DeleteKeyPair":
      requireLedgerID(input, ledger.keyPairIds, ledger.keyPairNames, "KeyPairId", "KeyName");
      return input["KeyPairId"] ? { KeyPairId: input["KeyPairId"] } : { KeyName: input["KeyName"] };
    case "RunInstances":
      return authorizedRunInstances(input, requestHash, request.opId, identity, policy, ledger);
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
    case "CreateSnapshot": {
      requireOwned(input["VolumeId"], ledger.volumeIds, "volume");
      return {
        VolumeId: input["VolumeId"],
        Description: boundedText(input["Description"], 255),
        ...qualificationTags(input, "snapshot", identity, request.opId),
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
  requestHash: string,
  opId: string,
  identity: AWSQualificationRunIdentity,
  policy: AWSQualificationPolicy,
  ledger: AWSQualificationLedger,
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
  const keyName = input["KeyName"] ?? "";
  requireOwned(keyName, ledger.keyPairNames, "key pair");
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
    ClientToken: `cbxq-${requestHash.slice(0, 59)}`,
    ImageId: imageId,
    InstanceType: instanceType,
    KeyName: keyName,
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
  if (action === "RunInstances" || action === "DescribeInstances") {
    const reservations =
      action === "RunInstances"
        ? [{ instancesSet: root["instancesSet"] }]
        : items(record(root["reservationSet"])["item"]);
    for (const reservation of reservations) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"]).map(
        record,
      )) {
        const instanceId = asString(instance["instanceId"]);
        if (instanceId) addUnique(ledger.instanceIds, instanceId);
        for (const mapping of items(record(instance["blockDeviceMapping"])["item"]).map(record)) {
          const volumeId = asString(record(mapping["ebs"])["volumeId"]);
          if (volumeId) addUnique(ledger.volumeIds, volumeId);
        }
      }
    }
  }
  if (action === "CreateImage") addUnique(ledger.imageIds, asString(root["imageId"]));
  if (action === "CreateSnapshot") addUnique(ledger.snapshotIds, asString(root["snapshotId"]));
  if (action === "ImportKeyPair") {
    addUnique(ledger.keyPairIds, asString(root["keyPairId"]));
    addUnique(ledger.keyPairNames, String(parameters["KeyName"] ?? ""));
  }
  if (action === "DescribeImages") {
    for (const image of items(record(root["imagesSet"])["item"]).map(record)) {
      const imageId = asString(image["imageId"]);
      if (!ledger.imageIds.includes(imageId)) continue;
      for (const mapping of items(record(image["blockDeviceMapping"])["item"]).map(record)) {
        addUnique(ledger.snapshotIds, asString(record(mapping["ebs"])["snapshotId"]));
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
  if (!identity.owner.trim() || identity.owner.length > 256) {
    throw new Error("AWS qualification owner is malformed");
  }
  if (!Number.isFinite(Date.parse(identity.expiresAt))) {
    throw new Error("AWS qualification expiry is malformed");
  }
}

function validateRequestShape(request: AWSQualificationRequest, region: string): void {
  if (!/^[0-9a-f-]{36}$/.test(request.opId)) {
    throw new Error("AWS qualification opId is malformed");
  }
  if (request.region !== region) throw new Error("AWS qualification region is outside policy");
  if (!request.action || request.action.length > 128) {
    throw new Error("AWS qualification action is malformed");
  }
  if (Object.keys(request.parameters).length > 256) {
    throw new Error("AWS qualification request has too many parameters");
  }
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
    if (typeof value !== "string" || key.length > 256 || value.length > maxUserDataBytes) {
      throw new Error(`AWS qualification parameter is malformed: ${key}`);
    }
    output[key] = value;
  }
  return output;
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

function requireLedgerID(
  input: Record<string, string>,
  ids: string[],
  names: string[],
  idField: string,
  nameField: string,
): void {
  if (input[idField]) return requireOwned(input[idField], ids, idField);
  requireOwned(input[nameField], names, nameField);
}

function indexedValues(input: Record<string, string>, field: string): string[] {
  const values: string[] = [];
  for (let index = 1; index <= 128; index += 1) {
    const value = input[`${field}.${index}`];
    if (value) values.push(value);
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

function emptyLedger(): AWSQualificationLedger {
  return {
    imageIds: [],
    instanceIds: [],
    keyPairIds: [],
    keyPairNames: [],
    snapshotIds: [],
    volumeIds: [],
  };
}

function addUnique(values: string[], value: string): void {
  if (value && !values.includes(value)) values.push(value);
}

function awsXMLRoot(body: string, action: string): Record<string, unknown> {
  const parsed = record(parser.parse(body));
  return record(parsed[`${action}Response`] ?? parsed["Response"] ?? parsed);
}

async function boundedResponse(response: Response): Promise<AWSQualificationResponse> {
  const body = await response.text();
  if (new TextEncoder().encode(body).byteLength > 1024 * 1024) {
    throw new Error("AWS qualification response exceeds 1 MiB");
  }
  return { status: response.status, body };
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
