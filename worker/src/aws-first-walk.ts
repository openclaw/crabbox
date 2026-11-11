import { XMLParser } from "fast-xml-parser";

import type {
  AWSQualificationFirstWalkRequest,
  AWSQualificationFirstWalkStatus,
  AWSQualificationRunIdentity,
} from "./aws-qualification-contract";
import { sha256Hex } from "./encoding";

// State belongs to AWSQualificationRun, including after a failed/uncertain RPC.
export const firstWalkKey = "first-walk";
export const firstWalkDevice = "/dev/sdf";
export interface FirstWalkState {
  request: AWSQualificationFirstWalkRequest;
  phase: "prepared" | "creating" | "created" | "attaching" | "attached";
  zone: string;
  sizeGB: number;
  volumeID?: string;
}
export interface FirstWalkOwner {
  identity: AWSQualificationRunIdentity;
  accountID: string;
  read(): Promise<FirstWalkState | undefined>;
  write(state: FirstWalkState): Promise<void>;
  query(action: string, parameters: Record<string, string>, mutating?: boolean): Promise<string>;
}
const parser = new XMLParser({ parseTagValue: false });
type ObjectRecord = Record<string, unknown>;
const obj = (value: unknown): ObjectRecord =>
  value !== null && typeof value === "object" ? (value as ObjectRecord) : {};
const list = (value: unknown): ObjectRecord[] =>
  (value === undefined ? [] : Array.isArray(value) ? value : [value]).map(obj);
const str = (value: unknown): string => (typeof value === "string" ? value : "");
function root(body: string, action: string): ObjectRecord {
  return obj(obj(parser.parse(body))[`${action}Response`]);
}
function tags(value: unknown): Map<string, string> {
  return new Map(list(obj(value)["item"]).map((tag) => [str(tag["key"]), str(tag["value"])]));
}
function ownerTags(owner: FirstWalkOwner): Record<string, string> {
  const values = {
    crabbox_qualification_run: owner.identity.runId,
    crabbox_qualification_owner: owner.identity.owner,
    crabbox_qualification_sha: owner.identity.candidateSha,
    crabbox_qualification_expiry: owner.identity.expiresAt,
    crabbox_qualification_op: "first-walk-data",
  };
  return Object.fromEntries(
    Object.entries(values).flatMap(([key, value], i) => [
      [`TagSpecification.1.Tag.${i + 1}.Key`, key],
      [`TagSpecification.1.Tag.${i + 1}.Value`, value],
    ]),
  );
}
async function instance(owner: FirstWalkOwner, id: string): Promise<ObjectRecord> {
  const body = await owner.query("DescribeInstances", { "InstanceId.1": id });
  const reservations = list(obj(root(body, "DescribeInstances")["reservationSet"])["item"]);
  if (reservations.length !== 1 || str(reservations[0]?.["ownerId"]) !== owner.accountID)
    throw new Error("first-walk instance account is not verified");
  const entries = list(obj(reservations[0]?.["instancesSet"])["item"]);
  const entry = entries[0];
  if (
    entries.length !== 1 ||
    str(entry?.["instanceId"]) !== id ||
    str(obj(entry?.["instanceState"])["name"]) !== "running" ||
    tags(entry?.["tagSet"]).get("crabbox_qualification_run") !== owner.identity.runId
  )
    throw new Error("first-walk instance is not the exact running owned instance");
  return entry!;
}
function verifyVolume(owner: FirstWalkOwner, state: FirstWalkState, volume: ObjectRecord): void {
  const labels = tags(volume["tagSet"]);
  if (
    !/^vol-[a-f0-9]+$/.test(str(volume["volumeId"])) ||
    (state.volumeID && str(volume["volumeId"]) !== state.volumeID) ||
    str(volume["snapshotId"]) !== state.request.snapshotID ||
    str(volume["availabilityZone"]) !== state.zone ||
    Number(volume["size"]) !== state.sizeGB ||
    str(volume["volumeType"]) !== "gp3" ||
    str(volume["encrypted"]) !== "true" ||
    Number(volume["iops"]) !== 3000 ||
    Number(volume["throughput"]) !== 125 ||
    labels.get("crabbox_qualification_run") !== owner.identity.runId ||
    labels.get("crabbox_qualification_owner") !== owner.identity.owner ||
    labels.get("crabbox_qualification_sha") !== owner.identity.candidateSha ||
    labels.get("crabbox_qualification_expiry") !== owner.identity.expiresAt ||
    labels.get("crabbox_qualification_op") !== "first-walk-data"
  )
    throw new Error("first-walk volume identity or policy mismatch");
}
async function discover(owner: FirstWalkOwner, state: FirstWalkState): Promise<ObjectRecord[]> {
  const parameters = state.volumeID
    ? { "VolumeId.1": state.volumeID }
    : {
        "Filter.1.Name": "tag:crabbox_qualification_run",
        "Filter.1.Value.1": owner.identity.runId,
        "Filter.2.Name": "tag:crabbox_qualification_op",
        "Filter.2.Value.1": "first-walk-data",
      };
  const body = await owner.query("DescribeVolumes", parameters);
  const response = root(body, "DescribeVolumes");
  if (response["nextToken"]) throw new Error("first-walk volume inventory is paginated");
  const entries = list(obj(response["volumeSet"])["item"]);
  if (entries.length > 1) throw new Error("first-walk volume inventory exceeds one copy");
  for (const entry of entries) verifyVolume(owner, state, entry);
  return entries;
}
export function firstWalkStatus(
  identity: AWSQualificationRunIdentity,
  state: FirstWalkState | undefined,
  finalized: boolean,
): AWSQualificationFirstWalkStatus {
  return {
    runID: identity.runId,
    deploymentHash: identity.deploymentHash,
    finalized,
    ...(state
      ? {
          request: state.request,
          phase: state.phase,
          volumeID: state.volumeID,
          device: firstWalkDevice,
        }
      : {}),
  };
}
export async function prepareFirstWalk(
  owner: FirstWalkOwner,
  request: AWSQualificationFirstWalkRequest,
): Promise<FirstWalkState> {
  let state = await owner.read();
  if (state && JSON.stringify(state.request) !== JSON.stringify(request))
    throw new Error("first-walk request is immutable");
  const guest = await instance(owner, request.instanceID);
  if (!state) {
    const mappings = list(obj(guest["blockDeviceMapping"])["item"]);
    if (mappings.filter((m) => str(obj(m["ebs"])["volumeId"])).length !== 1)
      throw new Error("first-walk requires a stock instance with one root EBS volume");
    if (mappings.some((m) => str(m["deviceName"]) === firstWalkDevice))
      throw new Error("first-walk device is already occupied");
    const body = await owner.query("DescribeSnapshots", {
      "SnapshotId.1": request.snapshotID,
      "Owner.1": "self",
    });
    const entries = list(obj(root(body, "DescribeSnapshots")["snapshotSet"])["item"]);
    const capture = entries[0];
    const sizeGB = Number(capture?.["volumeSize"]);
    if (
      entries.length !== 1 ||
      str(capture?.["snapshotId"]) !== request.snapshotID ||
      str(capture?.["ownerId"]) !== owner.accountID ||
      str(capture?.["status"]) !== "completed" ||
      !Number.isInteger(sizeGB) ||
      sizeGB < 1 ||
      sizeGB > 400
    )
      throw new Error("first-walk capture is not a single owned completed snapshot up to 400 GiB");
    const zone = str(obj(guest["placement"])["availabilityZone"]);
    if (!/^[a-z]{2}-[a-z]+-\d[a-z]$/.test(zone)) throw new Error("first-walk AZ is not verified");
    state = { request, phase: "prepared", zone, sizeGB };
    await owner.write(state);
  }
  if (state.phase === "prepared") {
    // Persist before dispatch. Even an empty reconciliation after uncertainty never
    // authorizes another CreateVolume; the owner must finalize this run instead.
    state.phase = "creating";
    await owner.write(state);
    const response = await owner.query(
      "CreateVolume",
      {
        AvailabilityZone: state.zone,
        SnapshotId: request.snapshotID,
        Size: String(state.sizeGB),
        VolumeType: "gp3",
        Iops: "3000",
        Throughput: "125",
        Encrypted: "true",
        ClientToken: await sha256Hex(`${owner.identity.runId}:first-walk-data`),
        "TagSpecification.1.ResourceType": "volume",
        ...ownerTags(owner),
      },
      true,
    );
    const id = str(root(response, "CreateVolume")["volumeId"]);
    if (!/^vol-[a-f0-9]+$/.test(id))
      throw new Error("first-walk create returned no exact volume ID");
    state.volumeID = id;
    await owner.write(state);
  }
  const volumes = await discover(owner, state);
  if (volumes.length !== 1)
    throw new Error("first-walk create outcome unresolved; finalize instead of replacing");
  const volume = volumes[0]!;
  state.volumeID = str(volume["volumeId"]);
  const attachments = list(obj(volume["attachmentSet"])["item"]);
  if (
    attachments.length > 1 ||
    attachments.some(
      (a) => str(a["instanceId"]) !== request.instanceID || str(a["device"]) !== firstWalkDevice,
    )
  )
    throw new Error("first-walk attachment identity mismatch");
  if (state.phase === "creating") {
    state.phase = "created";
    await owner.write(state);
  }
  if (!attachments.length) {
    if (state.phase !== "created" || str(volume["status"]) !== "available")
      throw new Error("first-walk volume/attachment not ready; retry reads or finalize");
    state.phase = "attaching";
    await owner.write(state);
    await owner.query(
      "AttachVolume",
      { VolumeId: state.volumeID, InstanceId: request.instanceID, Device: firstWalkDevice },
      true,
    );
    throw new Error("first-walk attachment dispatched; retry for readback");
  }
  if (str(attachments[0]?.["status"]) !== "attached")
    throw new Error("first-walk attachment not yet settled");
  const current = await instance(owner, request.instanceID);
  const mapping = list(obj(current["blockDeviceMapping"])["item"]).find(
    (m) => str(m["deviceName"]) === firstWalkDevice,
  );
  if (str(obj(mapping?.["ebs"])["volumeId"]) !== state.volumeID)
    throw new Error("first-walk instance mapping mismatch");
  if (str(obj(mapping?.["ebs"])["deleteOnTermination"]) !== "true") {
    await owner.query(
      "ModifyInstanceAttribute",
      {
        InstanceId: request.instanceID,
        "BlockDeviceMapping.1.DeviceName": firstWalkDevice,
        "BlockDeviceMapping.1.Ebs.VolumeId": state.volumeID,
        "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
      },
      true,
    );
    throw new Error("first-walk delete-on-termination requires readback");
  }
  state.phase = "attached";
  await owner.write(state);
  return state;
}

// Called only by fenced finalization, AFTER the instance termination request.
// Never force-detach a mounted filesystem. Termination settles all guest writers;
// a busy volume leaves the run/pin pending and the existing alarm retries cleanup.
export async function cleanupFirstWalk(owner: FirstWalkOwner): Promise<string[]> {
  const state = await owner.read();
  if (!state) return [];
  let entries: ObjectRecord[];
  try {
    entries = await discover(owner, state);
  } catch (error) {
    if (state.volumeID && String(error).includes("InvalidVolume.NotFound")) return [state.volumeID];
    throw error;
  }
  const ids: string[] = [];
  for (const volume of entries) {
    const id = str(volume["volumeId"]);
    ids.push(id);
    if (list(obj(volume["attachmentSet"])["item"]).length || str(volume["status"]) !== "available")
      throw new Error("first-walk copy still attached or busy");
    // oxlint-disable-next-line eslint/no-await-in-loop -- settle each exact owned volume before proceeding.
    await owner.query("DeleteVolume", { VolumeId: id }, true);
  }
  return ids;
}
