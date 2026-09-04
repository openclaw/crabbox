import {
  HetznerClient,
  HetznerProvisioningError,
  hetznerExactNotFound,
  hetznerServerOwnedByLease,
} from "./hetzner";
import { providerKeyForLease } from "./provider-key";
import {
  providerLabelsOwnedByLease,
  providerLabelValue,
  workspacePrewarmProviderOwner,
} from "./provider-labels";
import type { HetznerCleanupEvidence, HetznerServer, LeaseRecord } from "./types";

type SaveEvidence = (evidence: HetznerCleanupEvidence) => Promise<void>;

function keyOnlyLeaseIdentity(lease: LeaseRecord): boolean {
  return (
    lease.provider === "hetzner" &&
    /^cbx_[a-f0-9]{12}$/.test(lease.id) &&
    lease.providerKey === providerKeyForLease(lease.id) &&
    lease.serverID === 0 &&
    lease.cloudID === "" &&
    lease.providerCleanup === undefined
  );
}

export function hetznerDefiniteKeyOnlyFailure(lease: LeaseRecord, error: unknown): boolean {
  // A release's in-flight uncertainty can settle when this exact create definitively fails.
  return (
    keyOnlyLeaseIdentity(lease) &&
    error instanceof HetznerProvisioningError &&
    error.resourceMayExist === false &&
    error.serverID === undefined &&
    Number.isSafeInteger(error.providerKeyCleanupID) &&
    (error.providerKeyCleanupID ?? 0) > 0
  );
}

export function hetznerKeyOnlyCleanupID(lease: LeaseRecord): number | undefined {
  const keyID = Number(lease.providerKeyCleanupID);
  // The retained pending ID is issued only for a newly created, lease-owned key.
  if (
    !keyOnlyLeaseIdentity(lease) ||
    (lease.state !== "failed" && lease.state !== "released") ||
    lease.providerKeyCleanupPending !== true ||
    !Number.isSafeInteger(keyID) ||
    keyID <= 0 ||
    String(keyID) !== lease.providerKeyCleanupID ||
    lease.provisioningResourceMayExist !== false ||
    lease.provisioningRequestStartedAt !== undefined ||
    lease.provisioningRequestSettledAt !== undefined ||
    lease.provisioningRecoveryObservedAt !== undefined ||
    lease.provisioningRecoveryMissingSince !== undefined ||
    lease.provisioningCoordinatorVersion !== undefined
  )
    return undefined;
  return keyID;
}

function ownedServer(server: HetznerServer, lease: LeaseRecord): boolean {
  if (
    !server ||
    server.id !== lease.serverID ||
    !hetznerServerOwnedByLease(server, lease.id, lease.slug, lease.serverName)
  )
    return false;
  if (lease.slug?.trim()) return providerLabelsOwnedByLease(server.labels, lease, "hetzner");
  const owners = lease.providerOwner?.trim()
    ? [lease.providerOwner]
    : [lease.owner, ...(lease.workspaceID ? [workspacePrewarmProviderOwner] : [])];
  // The supported pre-slug contract did not require an owner label, but a conflict is explicit.
  return (
    server.labels["owner"] === undefined ||
    owners.some((owner) => server.labels["owner"] === providerLabelValue(owner))
  );
}

function deleteAction(
  response: unknown,
  serverID: number,
  actionID?: number,
): NonNullable<HetznerCleanupEvidence["action"]> {
  const action = (
    response as
      | {
          action?: {
            id?: number;
            command?: string;
            status?: string;
            resources?: { id: number; type: string }[];
            error?: unknown;
          };
        }
      | undefined
  )?.action;
  if (
    !action ||
    !Number.isSafeInteger(action.id) ||
    (action.id ?? 0) <= 0 ||
    (actionID !== undefined && action.id !== actionID) ||
    action.command !== "delete_server" ||
    !["running", "success", "error"].includes(action.status ?? "") ||
    !Array.isArray(action.resources) ||
    !action.resources.every(
      (resource) =>
        resource &&
        Number.isSafeInteger(resource.id) &&
        resource.id > 0 &&
        typeof resource.type === "string" &&
        resource.type.trim().length > 0,
    ) ||
    action.resources.filter((resource) => resource.type === "server").length !== 1 ||
    !action.resources.some((resource) => resource.type === "server" && resource.id === serverID) ||
    (action.status !== "error" && action.error != null)
  ) {
    throw new Error(`invalid Hetzner delete action binding for server ${serverID}`);
  }
  return { id: action.id!, status: action.status as "running" | "success" | "error" };
}

function timestamp(value: string | undefined): boolean {
  return value !== undefined && Number.isFinite(Date.parse(value));
}

function validEvidence(evidence: HetznerCleanupEvidence, lease: LeaseRecord): boolean {
  if (
    evidence.version !== 1 ||
    evidence.provider !== "hetzner" ||
    evidence.leaseID !== lease.id ||
    evidence.serverID !== lease.serverID ||
    (evidence.dispatchStartedAt !== undefined && !timestamp(evidence.dispatchStartedAt)) ||
    (evidence.deleteNotFoundAt !== undefined &&
      (!timestamp(evidence.deleteNotFoundAt) || !evidence.dispatchStartedAt || evidence.action)) ||
    (evidence.action &&
      (!evidence.dispatchStartedAt ||
        !Number.isSafeInteger(evidence.action.id) ||
        evidence.action.id <= 0 ||
        !["running", "success", "error"].includes(evidence.action.status)))
  )
    return false;
  if (!evidence.confirmation) return true;
  if (!timestamp(evidence.confirmation.at)) return false;
  switch (evidence.confirmation.method) {
    case "already-absent":
      return !evidence.dispatchStartedAt && !evidence.action && !evidence.deleteNotFoundAt;
    case "delete-not-found-and-server-absent":
      return !!evidence.deleteNotFoundAt && !evidence.action;
    case "delete-action-success-and-server-absent":
      return evidence.action?.status === "success";
    default:
      return false;
  }
}

// Only brokered owned cleanup uses this journal; provisioning rollback retains its existing path.
export async function confirmHetznerServerCleanup(
  client: HetznerClient,
  lease: LeaseRecord,
  save: SaveEvidence,
  deadline: number,
): Promise<void> {
  if (
    lease.provider !== "hetzner" ||
    !/^cbx_[a-f0-9]{12}$/.test(lease.id) ||
    !Number.isSafeInteger(lease.serverID) ||
    lease.serverID <= 0 ||
    (lease.cloudID && lease.cloudID !== String(lease.serverID))
  ) {
    throw new Error("unresolved Hetzner cleanup: invalid or conflicting exact server identity");
  }
  let evidence: HetznerCleanupEvidence = lease.providerCleanup ?? {
    version: 1,
    provider: "hetzner",
    leaseID: lease.id,
    serverID: lease.serverID,
  };
  if (!validEvidence(evidence, lease)) throw new Error("invalid retained Hetzner cleanup evidence");
  const persist = async (next: HetznerCleanupEvidence) => {
    await save(next);
    evidence = next;
  };
  const confirm = async (method: NonNullable<HetznerCleanupEvidence["confirmation"]>["method"]) => {
    await persist({ ...evidence, confirmation: { method, at: new Date().toISOString() } });
  };
  const absent = async (): Promise<boolean> => {
    let server: HetznerServer;
    try {
      server = await client.getServer(lease.serverID, deadline);
    } catch (error) {
      if (hetznerExactNotFound(error, "GET", `/servers/${lease.serverID}`)) return true;
      throw error;
    }
    if (!ownedServer(server, lease))
      throw new Error(
        `refusing to delete Hetzner server ${lease.serverID}: ownership does not match lease ${lease.id}`,
      );
    return false;
  };
  if (evidence.confirmation) {
    await persist(evidence); // Recheck the cleanup owner before resuming key cleanup.
    return;
  }
  if (!evidence.dispatchStartedAt) {
    if (await absent()) {
      await confirm("already-absent");
      return;
    }
    await persist({ ...evidence, dispatchStartedAt: new Date().toISOString() });
    let response: unknown;
    try {
      response = await client.deleteServerAction(lease.serverID, deadline);
    } catch (error) {
      if (!hetznerExactNotFound(error, "DELETE", `/servers/${lease.serverID}`)) throw error;
      await persist({ ...evidence, deleteNotFoundAt: new Date().toISOString() });
    }
    if (!evidence.deleteNotFoundAt) {
      await persist({ ...evidence, action: deleteAction(response, lease.serverID) });
    }
  }
  if (!evidence.action && !evidence.deleteNotFoundAt) {
    throw new Error(
      "unresolved Hetzner delete acknowledgement; retained dispatch requires resolution",
    );
  }
  while (Date.now() < deadline) {
    if (evidence.action?.status === "error")
      throw new Error(`Hetzner delete action ${evidence.action.id} failed`);
    if (evidence.action?.status === "running") {
      const action = deleteAction(
        // oxlint-disable-next-line eslint/no-await-in-loop -- observe this exact acknowledged action sequentially.
        await client.getServerAction(evidence.action.id, deadline),
        lease.serverID,
        evidence.action.id,
      );
      // oxlint-disable-next-line eslint/no-await-in-loop -- persist action progress before observing absence.
      await persist({ ...evidence, action });
      if (action.status === "error") throw new Error(`Hetzner delete action ${action.id} failed`);
    }
    // GET absence cannot complete a running action: the server may disappear at dispatch.
    // oxlint-disable-next-line eslint/no-await-in-loop -- poll absence only after successful action or exact DELETE 404.
    if (evidence.action?.status !== "running" && (await absent())) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- durable confirmation precedes SSH-key deletion.
      await confirm(
        evidence.action
          ? "delete-action-success-and-server-absent"
          : "delete-not-found-and-server-absent",
      );
      return;
    }
    // oxlint-disable-next-line eslint/no-await-in-loop -- bounded provider observation interval.
    await new Promise((resolve) =>
      setTimeout(resolve, Math.min(2_000, Math.max(0, deadline - Date.now()))),
    );
  }
  throw new Error(`Hetzner cleanup observation timed out for server ${lease.serverID}`);
}
