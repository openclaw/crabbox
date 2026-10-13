import {
  AWSQueryError,
  AWSLeaseObservationError,
  EC2SpotClient,
  type AWSLeaseOperation,
} from "./aws";
import { awsCleanupCompletionBlocker, awsLeaseOwnsKey } from "./aws-cleanup-completion";
import { sanitizeAWSRegion } from "./aws-region";
import type { CoordinatorStorage } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { completeLeaseProviderCleanup } from "./lease-cleanup";
import { leaseMaintenanceIndexReady } from "./lease-maintenance-index";
import { leaseCurrentIndex, leaseMaintenanceIndex, putLeaseRecord } from "./lease-record-store";
import { isRegisteredLease, leaseIsLive } from "./lease-state";
import { providerMachineOwnedByLease } from "./provider-labels";
import type { Env, LeaseRecord, ProviderMachine } from "./types";

export const retainedReconcileStateKey = "retained-reconcile-state";
export const retainedAbsenceIntervalMs = 10 * 60_000;
const claimDurationMs = 5 * 60_000;

type Counters = Record<string, Record<string, number>>;
export interface RetainedReconcileState {
  version: 1;
  cursor?: string;
  cycles: number;
  scanned: number;
  lastPassAt?: string;
  lastPass?: { counters: Counters; sample: { id: string; provider: string; outcome: string }[] };
  counters: Counters;
  backoff: Record<string, { until: number; failures: number }>;
}
export interface RetainedSettings {
  mode: "on" | "report" | "off";
  delete: boolean;
  graceSeconds: number;
  budget: number;
  lookupBudget: number;
  intervalSeconds: number;
}
function integer(value: string | undefined, fallback: number, min: number, max: number): number {
  if (!value || !/^\d+$/.test(value)) return fallback;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed >= min && parsed <= max ? parsed : fallback;
}
export function retainedReconcileSettings(env: Env): RetainedSettings {
  const value = env.CRABBOX_RETAINED_RECONCILE;
  return {
    mode: value === undefined || value === "on" ? "on" : value === "report" ? "report" : "off",
    delete: env.CRABBOX_RETAINED_RECONCILE_DELETE === "1",
    graceSeconds: integer(env.CRABBOX_RETAINED_RECONCILE_GRACE_SECONDS, 3600, 60, 31_536_000),
    budget: integer(env.CRABBOX_RETAINED_RECONCILE_BUDGET, 200, 1, 200),
    lookupBudget: integer(env.CRABBOX_RETAINED_RECONCILE_LOOKUP_BUDGET, 64, 5, 256),
    intervalSeconds: integer(env.CRABBOX_RETAINED_RECONCILE_INTERVAL_SECONDS, 120, 60, 86_400),
  };
}
function emptyState(): RetainedReconcileState {
  return { version: 1, cycles: 0, scanned: 0, counters: {}, backoff: {} };
}
export async function retainedReconcileStatus(storage: CoordinatorStorage, env: Env) {
  return {
    settings: retainedReconcileSettings(env),
    ...((await storage.get<RetainedReconcileState>(retainedReconcileStateKey, {
      noCache: true,
      allowConcurrency: true,
    })) ?? emptyState()),
  };
}

export async function retainedReconcileDue(
  storage: CoordinatorStorage,
  env: Env,
  now: number,
): Promise<boolean> {
  const settings = retainedReconcileSettings(env);
  if (settings.mode === "off") return false;
  const state = await storage.get<RetainedReconcileState>(retainedReconcileStateKey, {
    noCache: true,
    allowConcurrency: true,
  });
  const lastPass = Date.parse(state?.version === 1 ? (state.lastPassAt ?? "") : "");
  return !Number.isFinite(lastPass) || now >= lastPass + settings.intervalSeconds * 1000;
}

// Called only after the failed pass has unwound, under the provider maintenance lock.
export async function recordRetainedReconcileFailure(
  storage: CoordinatorStorage,
  started: number,
): Promise<void> {
  const stored = await storage.get<RetainedReconcileState>(retainedReconcileStateKey, {
    noCache: true,
    allowConcurrency: true,
  });
  const state = stored?.version === 1 ? stored : emptyState();
  await storage.put(retainedReconcileStateKey, {
    ...state,
    lastPassAt: new Date(started).toISOString(),
    lastPass: { counters: { all: { pass_failed: 1 } }, sample: [] },
    counters: {
      ...state.counters,
      all: {
        ...state.counters["all"],
        pass_failed: (state.counters["all"]?.["pass_failed"] ?? 0) + 1,
      },
    },
  } satisfies RetainedReconcileState);
}

export type RetainedObservation = {
  outcome: string;
  credentialFingerprint?: string;
  server?: ProviderMachine;
  retryAfter?: string | undefined;
};
export interface RetainedReconcileHooks {
  exclusive<T>(work: () => Promise<T>): Promise<T>;
  ownsProvisioning(id: string): Promise<boolean>;
  deleteLease(lease: LeaseRecord, guard: () => Promise<void>, signal: AbortSignal): Promise<void>;
  observe?: typeof observeAWSRetained;
  now?: () => number;
}
export class RetainedLookupBudget {
  readonly signal = AbortSignal.timeout(20_000);
  constructor(public remaining: number) {}
  take(): void {
    if (this.remaining <= 0 || this.signal.aborted) throw new Error("retained_lookup_budget");
    this.remaining--;
  }
}

// Skip uncertain allocation/action ownership; a missing instance cannot retire these journals.
export function retainedUnresolvedReason(lease: LeaseRecord): string | undefined {
  if (isRegisteredLease(lease)) return "registered_resource";
  if (!lease.cloudID) return "missing_identity";
  if (
    (lease.cleanupStartedAt && !lease.retainedReconcileDeleteClaim) ||
    lease.providerCleanup ||
    lease.provisioningRequestStartedAt ||
    lease.provisioningRequestSettledAt ||
    lease.provisioningCoordinatorVersion ||
    lease.provisioningRecoveryObservedAt ||
    lease.provisioningRecoveryMissingSince ||
    lease.provisioningResourceMayExist ||
    lease.provisioningFailureRetryable ||
    lease.runtimeAdapterDeleteRequestedAt ||
    lease.portablePoolAccess
  )
    return "unresolved_action";
  if (lease.provider !== "aws") {
    if (!lease.providerScope && !lease.providerProject) return "missing_original_scope";
    return "no_scoped_exact_read";
  }
  if (!/^i-[a-f0-9]{8,17}$/.test(lease.cloudID)) return "missing_identity";
  if (!/^aws:account:\d{12}$/.test(lease.providerScope ?? "") || !lease.region)
    return "missing_original_scope";
  try {
    if (sanitizeAWSRegion(lease.region) !== lease.region) return "invalid_scope";
  } catch {
    return "invalid_scope";
  }
  // Dedicated-host lifecycle has a separate ownership/billing contract.
  if (
    lease.target === "macos" ||
    lease.hostID ||
    lease.hostId ||
    lease.serverType.startsWith("mac")
  )
    return "dedicated_host";
  if (
    lease.lifetimePolicy?.retainedIdentity &&
    !lease.lifetimePolicy.retainedIdentity.startsWith(`${lease.cloudID}:`)
  )
    return "unresolved_resource_identity";
  if ((lease.providerKeyCleanupPending || lease.providerKeyCleanupID) && !awsLeaseOwnsKey(lease))
    return "unresolved_key";
  return undefined;
}

function errorObservation(error: unknown): RetainedObservation {
  if (error instanceof Error && error.message === "retained_lookup_budget")
    return { outcome: "lookup_budget" };
  if (
    error instanceof AWSQueryError &&
    (error.status === 429 || /Throttl|RequestLimit|SlowDown/.test(error.code))
  )
    return { outcome: "throttled", retryAfter: error.retryAfter };
  return { outcome: "lookup_error" };
}

async function observeCompanions(
  lease: LeaseRecord,
  session: AWSLeaseOperation,
  server: ProviderMachine | undefined,
  budget: RetainedLookupBudget,
): Promise<string> {
  if (server && server.status !== "terminated") return "present";
  if (server && !providerMachineOwnedByLease(server, lease, "aws")) return "ownership_mismatch";
  const volumeComplete = true;
  if (lease.lifetimePolicy?.retainedIdentity) {
    budget.take();
    // Same exact retained-volume verifier used by release, on this credential snapshot.
    try {
      await session.confirmRetainedVolumeDeleted(lease.lifetimePolicy.retainedIdentity);
    } catch (error) {
      if (
        error instanceof AWSLeaseObservationError &&
        error.message === "retained root volume deletion has not been confirmed"
      )
        return "retained_volume_present";
      throw error;
    }
  }
  let keyComplete = true;
  if (awsLeaseOwnsKey(lease)) {
    budget.take();
    keyComplete = await session.sshKeyAbsent(lease.providerKey, lease.id);
  }
  return (
    awsCleanupCompletionBlocker(lease, { instanceComplete: true, volumeComplete, keyComplete }) ??
    "absent"
  );
}

export async function observeAWSRetained(
  env: Env,
  leases: LeaseRecord[],
  budget: RetainedLookupBudget,
): Promise<Map<string, RetainedObservation>> {
  const result = new Map<string, RetainedObservation>();
  if (!leases.length) return result;
  try {
    budget.take(); // STS identity is part of the provider lookup budget.
    await new EC2SpotClient(env, leases[0]!.region!, undefined, budget.signal).withLeaseOperation(
      async (session) => {
        const identity = await session.verifiedIdentity();
        if (
          leases.some(
            (lease) =>
              lease.providerScope !== `aws:account:${identity.account}` ||
              lease.region !== session.region,
          )
        ) {
          for (const lease of leases) result.set(lease.id, { outcome: "scope_mismatch" });
          return;
        }
        const credentialFingerprint = await session.credentialFingerprint();
        const eligible = leases.filter((lease) => {
          const previous = lease.retainedReconcileObservation;
          if (
            (previous && previous.credentialFingerprint !== credentialFingerprint) ||
            (lease.retainedReconcileDeleteCredentialFingerprint &&
              lease.retainedReconcileDeleteCredentialFingerprint !== credentialFingerprint)
          ) {
            result.set(lease.id, { outcome: "credential_changed" });
            return false;
          }
          return true;
        });
        if (!eligible.length) return;
        let servers: Map<string, ProviderMachine>;
        try {
          budget.take();
          servers = await session.findServers([...new Set(eligible.map((lease) => lease.cloudID))]);
        } catch (error) {
          // AWS rejects an entire InstanceId batch if any ID is gone. Only exact
          // NotFound permits bounded individual reads; no inventory/name fallback.
          if (!(error instanceof AWSQueryError) || error.code !== "InvalidInstanceID.NotFound")
            throw error;
          servers = new Map();
          for (const lease of eligible) {
            try {
              budget.take();
              // oxlint-disable-next-line eslint/no-await-in-loop -- exact fallback consumes the shared bounded budget.
              const server = await session.findServer(lease.cloudID);
              if (server) servers.set(lease.cloudID, server);
            } catch (failure) {
              result.set(lease.id, errorObservation(failure));
              if (errorObservation(failure).outcome !== "lookup_budget") throw failure;
            }
          }
        }
        for (const lease of eligible) {
          if (result.has(lease.id)) continue;
          const server = servers.get(lease.cloudID);
          try {
            // oxlint-disable-next-line eslint/no-await-in-loop -- companion evidence stays inside the fixed credential session.
            const outcome = await observeCompanions(lease, session, server, budget);
            result.set(lease.id, { outcome, credentialFingerprint, ...(server ? { server } : {}) });
          } catch (error) {
            result.set(lease.id, errorObservation(error));
            if (errorObservation(error).outcome === "throttled") throw error;
          }
        }
      },
    );
  } catch (error) {
    // A failed or throttled session cannot publish even an earlier partial observation.
    for (const lease of leases) result.set(lease.id, errorObservation(error));
  }
  return result;
}

function baseline(lease: LeaseRecord): string {
  const { retainedReconcileObservation: _observation, ...rest } = lease;
  return JSON.stringify(rest);
}
function retryDelay(value: string | undefined, now: number, failures: number): number {
  const retry =
    value && (/^\d+(?:\.\d+)?$/.test(value) ? Number(value) * 1000 : Date.parse(value) - now);
  return Math.max(
    60_000,
    Math.min(3600_000, 60_000 * 2 ** Math.min(failures, 6)),
    Number.isFinite(retry) ? Number(retry) : 0,
  );
}

async function noActiveBinding(storage: CoordinatorStorage, lease: LeaseRecord): Promise<boolean> {
  const ready = await storage.get(leaseMaintenanceIndex.stateKey, {
    noCache: true,
    allowConcurrency: true,
  });
  if (!leaseMaintenanceIndexReady(ready as Parameters<typeof leaseMaintenanceIndexReady>[0]))
    return false;
  // If ordinary work is large, defer deletion rather than doing an unbounded safety scan.
  const members = await storage.list({
    prefix: leaseMaintenanceIndex.prefix,
    limit: 201,
    noCache: true,
    allowConcurrency: true,
  });
  if (members.size > 200) return false;
  for (const key of members.keys()) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- bounded live-binding proof immediately precedes owned deletion.
    const other = await storage.get<LeaseRecord>(
      `lease:${key.slice(leaseMaintenanceIndex.prefix.length)}`,
      { noCache: true, allowConcurrency: true },
    );
    if (other && leaseIsLive(other) && other.provider === "aws" && other.cloudID === lease.cloudID)
      return false;
  }
  return true;
}

export async function maintainRetainedLeases(
  storage: CoordinatorStorage,
  env: Env,
  hooks: RetainedReconcileHooks,
): Promise<void> {
  const settings = retainedReconcileSettings(env);
  if (settings.mode === "off") return;
  const now = hooks.now ?? Date.now;
  const started = now();
  const stored = await storage.get<RetainedReconcileState>(retainedReconcileStateKey, {
    noCache: true,
    allowConcurrency: true,
  });
  const state = stored?.version === 1 ? stored : emptyState();
  const counters: Counters = {};
  let deletionsRemaining = 1;
  const sample: { id: string; provider: string; outcome: string }[] = [];
  const count = (provider: string, outcome: string, id?: string) => {
    if (!Object.hasOwn(counters, provider)) counters[provider] = {};
    counters[provider]![outcome] = (counters[provider]![outcome] ?? 0) + 1;
    if (!Object.hasOwn(state.counters, provider)) state.counters[provider] = {};
    state.counters[provider]![outcome] = (state.counters[provider]![outcome] ?? 0) + 1;
    if (id && sample.length < 5 && outcome === "present") sample.push({ id, provider, outcome });
  };
  const ready = await storage.get(leaseCurrentIndex.stateKey, {
    noCache: true,
    allowConcurrency: true,
  });
  if (
    !leaseMaintenanceIndexReady(
      ready as Parameters<typeof leaseMaintenanceIndexReady>[0],
      leaseCurrentIndex,
    )
  ) {
    count("all", "index_not_ready");
  } else {
    const page = await storage.list({
      prefix: leaseCurrentIndex.prefix,
      limit: settings.budget,
      ...(state.cursor ? { startAfter: state.cursor } : {}),
      noCache: true,
      allowConcurrency: true,
    });
    const rows = await Promise.all(
      [...page.keys()].map(async (key) => {
        const lease = await storage.get<LeaseRecord>(
          `lease:${key.slice(leaseCurrentIndex.prefix.length)}`,
          { noCache: true, allowConcurrency: true },
        );
        if (
          lease?.retainedReconcileDeleteClaim &&
          lease.retainedReconcileDeleteClaim !== "awaiting-confirmation"
        ) {
          const deadline = Date.parse(lease.cleanupClaimExpiresAt ?? "");
          if (!Number.isFinite(deadline) || deadline > started) return { key, lease: undefined };
        }
        return { key, lease };
      }),
    );
    const budget = new RetainedLookupBudget(settings.lookupBudget);
    const observations = new Map<string, RetainedObservation>();
    // Group only the bounded hydrated page; preserve cursor order when applying results.
    const groups = new Map<string, LeaseRecord[]>();
    for (const { lease } of rows) {
      if (
        !lease ||
        !leaseCurrentIndex.member(lease) ||
        leaseIsLive(lease) ||
        retainedUnresolvedReason(lease)
      )
        continue;
      const group = `${lease.providerScope}:${lease.region}`;
      groups.set(group, [...(groups.get(group) ?? []), lease]);
    }
    if ((state.backoff["aws"]?.until ?? 0) <= started) {
      for (const leases of groups.values()) {
        const selected: LeaseRecord[] = [];
        let reserved = 2; // STS plus batch; reserve worst-case exact fallback and companions.
        for (const lease of leases) {
          const cost =
            1 +
            Number(awsLeaseOwnsKey(lease)) +
            Number(Boolean(lease.lifetimePolicy?.retainedIdentity));
          if (reserved + cost <= budget.remaining) {
            selected.push(lease);
            reserved += cost;
          } else observations.set(lease.id, { outcome: "lookup_budget" });
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- one provider session at a time, outside the lifecycle mutex.
        const results = await (hooks.observe ?? observeAWSRetained)(env, selected, budget);
        for (const [id, observation] of results) observations.set(id, observation);
        if (
          [...results.values()].some(
            (value) => value.outcome === "throttled" || value.outcome === "lookup_error",
          )
        )
          break;
      }
    }
    for (const { key, lease } of rows) {
      if (
        lease &&
        !leaseIsLive(lease) &&
        !retainedUnresolvedReason(lease) &&
        observations.get(lease.id)?.outcome === "lookup_budget"
      )
        break;
      const previousCursor = state.cursor;
      state.cursor = key;
      state.scanned++;
      if (!lease || !leaseCurrentIndex.member(lease) || leaseIsLive(lease)) {
        count(lease?.provider ?? "all", "not_terminal");
        continue;
      }
      const reason = retainedUnresolvedReason(lease);
      if (reason) {
        count(lease.provider, reason);
        continue;
      }
      const observation = observations.get(lease.id);
      if (!observation) {
        count(lease.provider, "backoff");
        continue;
      }
      const outcome = observation.outcome;
      count(lease.provider, outcome, lease.id);
      if (
        outcome === "throttled" ||
        outcome === "lookup_error" ||
        outcome === "scope_mismatch" ||
        outcome === "credential_changed"
      ) {
        const failures = (state.backoff["aws"]?.failures ?? 0) + 1;
        const failedAt = now();
        state.backoff["aws"] = {
          failures,
          until: failedAt + retryDelay(observation.retryAfter, failedAt, failures),
        };
        continue;
      }
      if (settings.mode === "report") continue;
      const snapshot = baseline(lease);
      // oxlint-disable-next-line eslint/no-await-in-loop -- fingerprint is bound to this exact candidate before a short transactional commit.
      const fingerprint = await sha256Hex(snapshot);
      if (outcome === "absent" && observation.credentialFingerprint) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- only publication holds the lifecycle fence; all cloud I/O has finished.
        await hooks.exclusive(() =>
          storage.transaction(async (transaction) => {
            const current = await transaction.get<LeaseRecord>(`lease:${lease.id}`, {
              noCache: true,
            });
            if (
              !current ||
              baseline(current) !== snapshot ||
              leaseIsLive(current) ||
              (await hooks.ownsProvisioning(current.id)) ||
              (current.retainedReconcileDeleteClaim &&
                current.retainedReconcileDeleteClaim !== "awaiting-confirmation" &&
                !(Date.parse(current.cleanupClaimExpiresAt ?? "") <= started))
            ) {
              count("aws", "changed");
              return;
            }
            let observationFingerprint = fingerprint;
            if (
              current.retainedReconcileDeleteClaim &&
              current.retainedReconcileDeleteClaim !== "awaiting-confirmation"
            ) {
              current.retainedReconcileDeleteClaim = "awaiting-confirmation";
              delete current.cleanupStartedAt;
              delete current.cleanupClaimExpiresAt;
              observationFingerprint = await sha256Hex(baseline(current));
            }
            const previous = current.retainedReconcileObservation;
            if (previous && previous.credentialFingerprint !== observation.credentialFingerprint) {
              count("aws", "credential_changed");
              return;
            }
            // First evidence is timed after provider work. Comparing the next pass's
            // start against it cannot shorten the interval when provider latency varies.
            const observedAt = new Date(now()).toISOString();
            const first =
              previous?.fingerprint === observationFingerprint
                ? previous.firstAbsentAt
                : observedAt;
            current.retainedReconcileObservation = {
              fingerprint: observationFingerprint,
              credentialFingerprint: observation.credentialFingerprint!,
              firstAbsentAt: first,
              lastAbsentAt: observedAt,
            };
            if (started - Date.parse(first) >= retainedAbsenceIntervalMs) {
              delete current.retainedReconcileDeleteClaim;
              delete current.retainedReconcileDeleteCredentialFingerprint;
              delete current.releaseDeletesServer;
              delete current.cleanupError;
              delete current.cleanupAttempts;
              delete current.cleanupRetryAt;
              delete current.cleanupFailedAt;
              delete current.cleanupStartedAt;
              delete current.cleanupClaimExpiresAt;
              delete current.providerKeyCleanupPending;
              delete current.providerKeyCleanupID;
              completeLeaseProviderCleanup(current, observedAt);
              current.cleanupConfirmedBy = "retained-reconcile";
              count("aws", "confirmed");
            } else count("aws", "first_absence");
            await putLeaseRecord(transaction, current);
          }),
        );
      } else if (outcome === "present") {
        // A positive read interrupts the two-absence sequence.
        // oxlint-disable-next-line eslint/no-await-in-loop -- discard stale absence evidence under the same short publication fence.
        await hooks.exclusive(() =>
          storage.transaction(async (transaction) => {
            const current = await transaction.get<LeaseRecord>(`lease:${lease.id}`, {
              noCache: true,
            });
            if (current && baseline(current) === snapshot && current.retainedReconcileObservation) {
              delete current.retainedReconcileObservation;
              await putLeaseRecord(transaction, current);
            }
          }),
        );
        if (!settings.delete || !observation.server || !observation.credentialFingerprint) continue;
        const terminalAt = Date.parse(lease.endedAt ?? lease.releasedAt ?? "");
        if (!Number.isFinite(terminalAt) || started - terminalAt < settings.graceSeconds * 1000) {
          count("aws", "grace");
          continue;
        }
        if (!providerMachineOwnedByLease(observation.server, lease, "aws")) {
          count("aws", "ownership_mismatch");
          continue;
        }
        if (!deletionsRemaining) {
          if (previousCursor) state.cursor = previousCursor;
          else delete state.cursor;
          count("aws", "delete_budget");
          break;
        }
        deletionsRemaining--;
        // oxlint-disable-next-line eslint/no-await-in-loop -- one bounded owned-delete operation per pass, outside the lifecycle mutex.
        const failure = await deletePresent(
          storage,
          lease,
          snapshot,
          observation.credentialFingerprint,
          hooks,
          now,
          count,
        );
        if (failure) {
          count("aws", failure.outcome);
          const failures = (state.backoff["aws"]?.failures ?? 0) + 1;
          state.backoff["aws"] = {
            failures,
            until: now() + retryDelay(failure.retryAfter, now(), failures),
          };
        }
      }
    }
    if ((page.size < settings.budget && state.cursor === rows.at(-1)?.key) || !page.size) {
      delete state.cursor;
      state.cycles++;
    }
  }
  const awsPass = counters["aws"];
  if (
    awsPass &&
    !awsPass["throttled"] &&
    !awsPass["lookup_error"] &&
    !awsPass["scope_mismatch"] &&
    !awsPass["credential_changed"] &&
    (awsPass["absent"] || awsPass["present"])
  )
    delete state.backoff["aws"];
  state.lastPassAt = new Date(started).toISOString();
  state.lastPass = { counters, sample };
  await storage.put(retainedReconcileStateKey, state);
  console.log(
    JSON.stringify({
      event: "coordinator.retained_reconcile",
      at: state.lastPassAt,
      cursor: state.cursor ?? null,
      cycles: state.cycles,
      counters,
      sample,
    }),
  );
}

async function deletePresent(
  storage: CoordinatorStorage,
  lease: LeaseRecord,
  snapshot: string,
  credentialFingerprint: string,
  hooks: RetainedReconcileHooks,
  now: () => number,
  count: (provider: string, outcome: string) => void,
): Promise<RetainedObservation | undefined> {
  const claimed = await hooks.exclusive(async () => {
    const current = await storage.get<LeaseRecord>(`lease:${lease.id}`, { noCache: true });
    if (
      !current ||
      baseline(current) !== snapshot ||
      (current.cleanupStartedAt && !current.retainedReconcileDeleteClaim) ||
      (current.retainedReconcileDeleteClaim &&
        current.retainedReconcileDeleteClaim !== "awaiting-confirmation" &&
        !(Date.parse(current.cleanupClaimExpiresAt ?? "") <= now())) ||
      (await hooks.ownsProvisioning(current.id)) ||
      !(await noActiveBinding(storage, current))
    )
      return undefined;
    const claim = new Date(now()).toISOString();
    current.retainedReconcileDeleteClaim = claim;
    current.retainedReconcileDeleteCredentialFingerprint = credentialFingerprint;
    current.cleanupStartedAt = claim;
    current.cleanupClaimExpiresAt = new Date(now() + claimDurationMs).toISOString();
    await putLeaseRecord(storage, current);
    return current;
  });
  if (!claimed) {
    count("aws", "delete_deferred");
    return;
  }
  const guard = async () => {
    const current = await storage.get<LeaseRecord>(`lease:${lease.id}`, { noCache: true });
    if (
      !current ||
      baseline(current) !== baseline(claimed) ||
      !(await noActiveBinding(storage, current))
    )
      throw new Error("retained deletion binding changed");
  };
  let failure: RetainedObservation | undefined;
  try {
    await hooks.deleteLease(claimed, guard, AbortSignal.timeout(20_000));
  } catch (error) {
    failure = errorObservation(error);
  }
  await hooks.exclusive(async () => {
    const current = await storage.get<LeaseRecord>(`lease:${lease.id}`, { noCache: true });
    if (!current || baseline(current) !== baseline(claimed)) return;
    current.retainedReconcileDeleteClaim = "awaiting-confirmation";
    delete current.cleanupStartedAt;
    delete current.cleanupClaimExpiresAt;
    delete current.retainedReconcileObservation;
    await putLeaseRecord(storage, current);
  });
  count("aws", failure ? "delete_failed" : "delete_dispatched");
  return failure;
}
