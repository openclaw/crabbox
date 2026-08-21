/* oxlint-disable eslint/no-await-in-loop -- lifecycle cloud mutations are deliberately ordered per exact member. */
import type { LeaseConfig } from "./config";
import type { CoordinatorStorage } from "./coordinator-runtime";
import type { CacheVolumeBinding, CacheVolumeConfig } from "./types";

export const awsCacheVolumeProtocol = 1;
export const awsCacheVolumeABI = "ext4-v1";

export type AWSCacheVolumeState =
  | "reserving"
  | "available"
  | "attached"
  | "quarantined"
  | "deleting";

export interface AWSCacheVolumeDescription {
  id: string;
  state: string;
  availabilityZone: string;
  encrypted: boolean;
  volumeType: string;
  sizeGB: number;
  multiAttachEnabled: boolean;
  attachments: string[];
  tags: Record<string, string>;
}

export interface AWSCacheVolumeCloud {
  callerAccountID(): Promise<string>;
  validateCacheVolumeInstanceType(config: LeaseConfig): Promise<void>;
  cacheVolumeAvailabilityZone(config: LeaseConfig): Promise<string>;
  createCacheVolume(
    availabilityZone: string,
    sizeGB: number,
    tags: Record<string, string>,
    clientToken: string,
  ): Promise<string>;
  findCacheVolumes(
    availabilityZone: string,
    tags: Record<string, string>,
  ): Promise<AWSCacheVolumeDescription[]>;
  describeCacheVolume(volumeID: string): Promise<AWSCacheVolumeDescription>;
  attachCacheVolume(volumeID: string, instanceID: string, device: string): Promise<void>;
  detachCacheVolume(volumeID: string, instanceID: string): Promise<void>;
  deleteCacheVolume(volumeID: string): Promise<void>;
}

export interface AWSCacheVolumePlan {
  protocol: 1;
  region: string;
  availabilityZone: string;
  leaseID: string;
  bindings: CacheVolumeBinding[];
  bootstrap: string;
  readyChecks: string;
}

interface AWSCacheVolumeRecord {
  version: 1;
  cacheSetID: string;
  memberID: string;
  accountDigest: string;
  tenantDigest: string;
  repoScopeDigest: string;
  keyDigest: string;
  abiDigest: string;
  name: string;
  path: string;
  sizeGB: number;
  region: string;
  availabilityZone: string;
  volumeID?: string;
  generation: number;
  state: AWSCacheVolumeState;
  leaseID?: string;
  instanceID?: string;
  purgeOnRelease?: boolean;
  fresh?: boolean;
  lastError?: string;
  lastErrorAt?: string;
  retryCount?: number;
  createdAt: string;
  updatedAt: string;
}

const recordPrefix = "aws-cache-volume:v1:";
const gcLastRunKey = "aws-cache-volume-gc:v1:last-run";
const maxMembers = 8;
const gcMinAgeMs = 7 * 24 * 60 * 60 * 1000;
const gcIntervalMs = 24 * 60 * 60 * 1000;

export class AWSCacheVolumeLifecycle {
  constructor(private readonly storage: CoordinatorStorage) {}

  async prepare(
    cloud: AWSCacheVolumeCloud,
    config: LeaseConfig,
    leaseID: string,
    tenantScope: string,
  ): Promise<AWSCacheVolumePlan | undefined> {
    if (config.cacheVolumes.length === 0) return undefined;
    await cloud.validateCacheVolumeInstanceType(config);
    const [accountID, availabilityZone, abiDigest] = await Promise.all([
      cloud.callerAccountID(),
      cloud.cacheVolumeAvailabilityZone(config),
      digest(awsCacheVolumeABI),
    ]);
    const tenantDigest = await digest(`${tenantScope}\0${accountID}`);
    const accountDigest = await digest(accountID);
    const repoScopeDigest = await digest(config.repoScope);
    const prepared: AWSCacheVolumeRecord[] = [];
    try {
      for (const volume of config.cacheVolumes) {
        prepared.push(
          await this.reserve(
            cloud,
            config,
            leaseID,
            accountDigest,
            tenantDigest,
            repoScopeDigest,
            abiDigest,
            volume,
          ),
        );
      }
    } catch (error) {
      await this.release(cloud, leaseID, false);
      throw error;
    }
    const bindings = prepared
      .map((record) => ({
        name: record.name,
        path: record.path,
        volumeID: record.volumeID!,
        generation: record.generation,
        abi: awsCacheVolumeABI,
      }))
      .toSorted((left, right) => left.name.localeCompare(right.name));
    return {
      protocol: 1,
      region: config.awsRegion,
      availabilityZone,
      leaseID,
      bindings,
      bootstrap: cacheVolumeBootstrap(prepared, config.sshUser),
      readyChecks: cacheVolumeReadyChecks(bindings),
    };
  }

  async attach(
    cloud: AWSCacheVolumeCloud,
    plan: AWSCacheVolumePlan,
    instanceID: string,
  ): Promise<void> {
    for (const [index, binding] of plan.bindings.entries()) {
      const key = recordKey(binding.volumeID);
      const record = await this.storage.get<AWSCacheVolumeRecord>(key);
      if (
        !record ||
        record.leaseID !== plan.leaseID ||
        record.generation !== binding.generation ||
        record.state !== "reserving"
      ) {
        throw new Error(`AWS cache volume ${binding.volumeID} lost its durable reservation`);
      }
      const live = await cloud.describeCacheVolume(binding.volumeID);
      if (!cacheVolumeReusable(record, live)) {
        await this.storage.put(key, {
          ...record,
          state: "quarantined",
          updatedAt: new Date().toISOString(),
        });
        throw new Error(
          `AWS cache volume ${binding.volumeID} live properties no longer match its durable reservation`,
        );
      }
      await this.storage.put(key, {
        ...record,
        state: "attached",
        instanceID,
        updatedAt: new Date().toISOString(),
      });
      try {
        await attachCacheVolume(
          cloud,
          binding.volumeID,
          instanceID,
          `/dev/sd${String.fromCharCode("f".charCodeAt(0) + index)}`,
        );
        await waitForVolume(cloud, binding.volumeID, (volume) => {
          return (
            cacheVolumePropertiesMatch(record, volume) &&
            volume.attachments.length === 1 &&
            volume.attachments[0] === instanceID
          );
        });
      } catch (error) {
        await this.storage.put(key, {
          ...record,
          state: "quarantined",
          instanceID,
          updatedAt: new Date().toISOString(),
        });
        throw error;
      }
    }
  }

  async release(cloud: AWSCacheVolumeCloud, leaseID: string, purge = false): Promise<void> {
    if (!leaseID.trim()) throw new Error("AWS cache volume release requires an exact lease ID");
    const records = await this.storage.list<AWSCacheVolumeRecord>({ prefix: recordPrefix });
    const failures: string[] = [];
    for (const [key, record] of records) {
      if (record.leaseID !== leaseID) continue;
      let resolvedKey = key;
      let resolved = record;
      if (!resolved.volumeID) {
        let matches: AWSCacheVolumeDescription[];
        try {
          matches = await cloud.findCacheVolumes(
            resolved.availabilityZone,
            cacheVolumeTags(resolved),
          );
        } catch (error) {
          await this.storage.put(key, retryableCacheVolumeRecord(resolved, error));
          failures.push(error instanceof Error ? error.message : String(error));
          continue;
        }
        if (matches.length !== 1) {
          await this.storage.put(key, {
            ...resolved,
            state:
              matches.length > 1 || resolved.state === "quarantined" ? "quarantined" : "reserving",
            updatedAt: new Date().toISOString(),
          });
          if (matches.length > 1) {
            failures.push(
              `AWS cache volume member ${resolved.memberID} resolved to ${matches.length} volumes`,
            );
          }
          continue;
        }
        const match = matches[0]!;
        if (!cacheVolumePropertiesMatch(resolved, match)) {
          await this.storage.put(key, {
            ...resolved,
            state: "quarantined",
            updatedAt: new Date().toISOString(),
          });
          failures.push(
            `AWS cache volume member ${resolved.memberID} resolved to an incompatible volume`,
          );
          continue;
        }
        resolved = { ...resolved, volumeID: match.id, updatedAt: new Date().toISOString() };
        resolvedKey = recordKey(match.id);
        await this.storage.transaction(async (transaction) => {
          await transaction.put(resolvedKey, resolved);
          await transaction.delete(key);
        });
      }
      try {
        let volume: AWSCacheVolumeDescription;
        try {
          volume = await cloud.describeCacheVolume(resolved.volumeID!);
        } catch (error) {
          if (resolved.state === "deleting" && cacheVolumeNotFound(error)) {
            await this.storage.delete(resolvedKey);
            continue;
          }
          throw error;
        }
        if (!cacheVolumePropertiesMatch(resolved, volume)) {
          throw new Error(`AWS cache volume ${resolved.volumeID} ownership evidence changed`);
        }
        if (
          volume.attachments.length > 1 ||
          (volume.attachments.length === 1 && volume.attachments[0] !== resolved.instanceID)
        ) {
          throw new Error(`AWS cache volume ${resolved.volumeID} has an external attachment`);
        }
        if (volume.attachments.length === 1) {
          await cloud.detachCacheVolume(resolved.volumeID!, resolved.instanceID!);
          await waitForVolume(
            cloud,
            resolved.volumeID!,
            (current) =>
              cacheVolumePropertiesMatch(resolved, current) && current.attachments.length === 0,
          );
          volume = await cloud.describeCacheVolume(resolved.volumeID!);
        }
        if (!cacheVolumePropertiesMatch(resolved, volume) || volume.attachments.length !== 0) {
          throw new Error(`AWS cache volume ${resolved.volumeID} remained attached`);
        }
        if (purge || resolved.purgeOnRelease) {
          await this.storage.put(resolvedKey, {
            ...resolved,
            state: "deleting",
            updatedAt: new Date().toISOString(),
          });
          try {
            await cloud.deleteCacheVolume(resolved.volumeID!);
          } catch (error) {
            if (!cacheVolumeNotFound(error)) throw error;
          }
          await this.storage.delete(resolvedKey);
        } else {
          await this.storage.put(resolvedKey, {
            ...resolved,
            state: "available",
            leaseID: undefined,
            instanceID: undefined,
            purgeOnRelease: undefined,
            fresh: false,
            updatedAt: new Date().toISOString(),
          });
        }
      } catch (error) {
        await this.storage.put(resolvedKey, retryableCacheVolumeRecord(resolved, error));
        failures.push(error instanceof Error ? error.message : String(error));
      }
    }
    if (failures.length > 0) throw new Error(failures.join("; "));
  }

  async garbageCollect(
    cloud: AWSCacheVolumeCloud,
    region: string,
    cutoff: Date,
  ): Promise<string[]> {
    const accountDigest = await digest(await cloud.callerAccountID());
    const records = await this.storage.list<AWSCacheVolumeRecord>({ prefix: recordPrefix });
    const deleted: string[] = [];
    const failures: string[] = [];
    for (const [key, record] of records) {
      const staleReservation =
        record.state === "reserving" &&
        !record.instanceID &&
        Date.parse(record.updatedAt) < cutoff.getTime();
      const reusableCandidate =
        !record.instanceID && (record.state === "available" || record.state === "quarantined");
      const deletionTombstone = !record.instanceID && record.state === "deleting";
      if (
        record.region !== region ||
        record.accountDigest !== accountDigest ||
        (!reusableCandidate && !staleReservation && !deletionTombstone) ||
        (!deletionTombstone && Date.parse(record.updatedAt) >= cutoff.getTime())
      ) {
        continue;
      }
      const claimed = await this.storage.transaction(async (transaction) => {
        const current = await transaction.get<AWSCacheVolumeRecord>(key);
        if (
          !current ||
          current.memberID !== record.memberID ||
          current.state !== record.state ||
          current.leaseID !== record.leaseID ||
          current.updatedAt !== record.updatedAt ||
          current.instanceID
        ) {
          return undefined;
        }
        const next = {
          ...current,
          state: "deleting" as const,
          updatedAt: new Date().toISOString(),
        };
        await transaction.put(key, next);
        return next;
      });
      if (!claimed) continue;
      try {
        let volumeID = claimed.volumeID;
        if (!volumeID) {
          const matches = await cloud.findCacheVolumes(
            claimed.availabilityZone,
            cacheVolumeTags(claimed),
          );
          if (matches.length === 0) {
            await this.storage.delete(key);
            continue;
          }
          if (matches.length > 1) {
            await this.storage.put(key, {
              ...claimed,
              state: "quarantined",
              updatedAt: new Date().toISOString(),
            });
            continue;
          }
          volumeID = matches[0]!.id;
        }
        let volume: AWSCacheVolumeDescription;
        try {
          volume = await cloud.describeCacheVolume(volumeID);
        } catch (error) {
          if (deletionTombstone && cacheVolumeNotFound(error)) {
            await this.storage.delete(key);
            continue;
          }
          throw error;
        }
        if (volume.attachments.length !== 0 || !cacheVolumePropertiesMatch(claimed, volume)) {
          await this.storage.put(key, {
            ...claimed,
            state: "quarantined",
            updatedAt: new Date().toISOString(),
          });
          continue;
        }
        await cloud.deleteCacheVolume(volumeID);
        await this.storage.delete(key);
        deleted.push(volumeID);
      } catch (error) {
        await this.storage.put(key, retryableCacheVolumeRecord(claimed, error));
        failures.push(error instanceof Error ? error.message : String(error));
      }
    }
    if (failures.length > 0) throw new Error(failures.join("; "));
    return deleted;
  }

  async garbageCollectIfDue(
    cloudForRegion: (region: string) => AWSCacheVolumeCloud,
    now = Date.now(),
  ): Promise<string[]> {
    const lastRun = await this.storage.get<number>(gcLastRunKey);
    if (lastRun !== undefined && now - lastRun < gcIntervalMs) return [];
    const records = await this.storage.list<AWSCacheVolumeRecord>({ prefix: recordPrefix });
    const regions = [
      ...new Set(
        [...records.values()].map((record) => record.region).filter((region) => region.length > 0),
      ),
    ].toSorted();
    const deleted: string[] = [];
    for (const region of regions) {
      deleted.push(
        ...(await this.garbageCollect(cloudForRegion(region), region, new Date(now - gcMinAgeMs))),
      );
    }
    await this.storage.put(gcLastRunKey, now);
    return deleted;
  }

  private async reserve(
    cloud: AWSCacheVolumeCloud,
    config: LeaseConfig,
    leaseID: string,
    accountDigest: string,
    tenantDigest: string,
    repoScopeDigest: string,
    abiDigest: string,
    volume: CacheVolumeConfig,
  ): Promise<AWSCacheVolumeRecord> {
    const keyDigest = await digest(volume.key);
    const availabilityZone = await cloud.cacheVolumeAvailabilityZone(config);
    const decision = await this.storage.transaction(async (transaction) => {
      const records = await transaction.list<AWSCacheVolumeRecord>({ prefix: recordPrefix });
      const base = [...records.entries()]
        .filter(
          ([, record]) =>
            record.accountDigest === accountDigest &&
            record.tenantDigest === tenantDigest &&
            record.repoScopeDigest === repoScopeDigest &&
            record.keyDigest === keyDigest &&
            record.region === config.awsRegion &&
            record.availabilityZone === availabilityZone,
        )
        .toSorted(([, left], [, right]) => left.generation - right.generation);
      let maxGeneration = 0;
      let memberCount = 0;
      for (const [key, record] of base) {
        maxGeneration = Math.max(maxGeneration, record.generation);
        if (record.abiDigest !== abiDigest) {
          await transaction.put(key, {
            ...record,
            state: "quarantined",
            updatedAt: new Date().toISOString(),
          });
          continue;
        }
        const requestedSizeGB = volume.sizeGB ?? 20;
        if (record.sizeGB < requestedSizeGB) {
          await transaction.put(key, {
            ...record,
            state: "quarantined",
            updatedAt: new Date().toISOString(),
          });
          continue;
        }
        if (record.state !== "quarantined" && (record.volumeID || record.state === "reserving")) {
          memberCount += 1;
        }
        if (record.state === "reserving" && record.leaseID === leaseID) {
          const pending = {
            ...record,
            name: volume.name!,
            path: volume.path,
            purgeOnRelease: config.purgeOnRelease,
            updatedAt: new Date().toISOString(),
          };
          await transaction.put(key, pending);
          return { existing: Boolean(record.volumeID), key, record: pending };
        }
        if (record.state !== "available" || !record.volumeID) continue;
        const reserved = {
          ...record,
          state: "reserving" as const,
          leaseID,
          name: volume.name!,
          path: volume.path,
          purgeOnRelease: config.purgeOnRelease,
          updatedAt: new Date().toISOString(),
        };
        await transaction.put(key, reserved);
        return { existing: true as const, key, record: reserved };
      }
      if (memberCount >= maxMembers) {
        throw new Error(
          `AWS cache volume ${volume.name} has no available members in ${availabilityZone}`,
        );
      }
      const now = new Date().toISOString();
      const provisional: AWSCacheVolumeRecord = {
        version: 1,
        cacheSetID: crypto.randomUUID(),
        memberID: crypto.randomUUID(),
        accountDigest,
        tenantDigest,
        repoScopeDigest,
        keyDigest,
        abiDigest,
        name: volume.name!,
        path: volume.path,
        sizeGB: volume.sizeGB ?? 20,
        region: config.awsRegion,
        availabilityZone,
        generation: maxGeneration + 1,
        state: "reserving",
        leaseID,
        purgeOnRelease: config.purgeOnRelease,
        fresh: true,
        createdAt: now,
        updatedAt: now,
      };
      const key = `${recordPrefix}pending:${provisional.memberID}`;
      await transaction.put(key, provisional);
      return { existing: false as const, key, record: provisional };
    });
    if (decision.existing) {
      let current: AWSCacheVolumeDescription;
      try {
        current = await cloud.describeCacheVolume(decision.record.volumeID!);
      } catch (error) {
        await this.storage.put(decision.key, retryableCacheVolumeRecord(decision.record, error));
        throw error;
      }
      if (cacheVolumeReusable(decision.record, current)) {
        return decision.record;
      }
      await this.storage.transaction(async (transaction) => {
        const latest = await transaction.get<AWSCacheVolumeRecord>(decision.key);
        if (latest?.leaseID === leaseID && latest.state === "reserving") {
          await transaction.put(decision.key, {
            ...latest,
            state: "quarantined",
            updatedAt: new Date().toISOString(),
          });
        }
      });
      return await this.reserve(
        cloud,
        config,
        leaseID,
        accountDigest,
        tenantDigest,
        repoScopeDigest,
        abiDigest,
        volume,
      );
    }
    const provisional = decision.record;
    let matches: AWSCacheVolumeDescription[];
    try {
      matches = await cloud.findCacheVolumes(availabilityZone, cacheVolumeTags(provisional));
    } catch (error) {
      await this.storage.put(decision.key, retryableCacheVolumeRecord(provisional, error));
      throw error;
    }
    if (matches.length > 1) {
      await this.storage.put(decision.key, {
        ...provisional,
        state: "quarantined",
        updatedAt: new Date().toISOString(),
      });
      throw new Error(
        `AWS cache volume member ${provisional.memberID} resolved to multiple volumes`,
      );
    }
    if (matches.length === 1 && !cacheVolumeReusable(provisional, matches[0]!)) {
      await this.storage.put(decision.key, {
        ...provisional,
        state: "quarantined",
        updatedAt: new Date().toISOString(),
      });
      throw new Error(
        `AWS cache volume member ${provisional.memberID} resolved to an incompatible volume`,
      );
    }
    let volumeID = matches[0]?.id;
    if (!volumeID) {
      try {
        volumeID = await cloud.createCacheVolume(
          availabilityZone,
          provisional.sizeGB,
          cacheVolumeTags(provisional),
          provisional.memberID,
        );
      } catch (error) {
        await this.storage.put(decision.key, retryableCacheVolumeRecord(provisional, error));
        throw error;
      }
    }
    const committed = { ...provisional, volumeID, updatedAt: new Date().toISOString() };
    await this.storage.transaction(async (transaction) => {
      await transaction.put(recordKey(volumeID), committed);
      await transaction.delete(decision.key);
    });
    try {
      await waitForVolume(cloud, volumeID, (current) => cacheVolumeReusable(committed, current));
    } catch (error) {
      await this.storage.put(recordKey(volumeID), retryableCacheVolumeRecord(committed, error));
      throw error;
    }
    return committed;
  }
}

function recordKey(volumeID: string): string {
  return `${recordPrefix}${volumeID}`;
}

function cacheVolumeTags(record: AWSCacheVolumeRecord): Record<string, string> {
  return {
    crabbox: "true",
    created_by: "crabbox",
    crabbox_cache_set: record.cacheSetID,
    crabbox_cache_member: record.memberID,
    crabbox_cache_generation: String(record.generation),
    crabbox_cache_abi: record.abiDigest,
  };
}

function tagsMatch(record: AWSCacheVolumeRecord, tags: Record<string, string>): boolean {
  return Object.entries(cacheVolumeTags(record)).every(([key, value]) => tags[key] === value);
}

function cacheVolumePropertiesMatch(
  record: AWSCacheVolumeRecord,
  volume: AWSCacheVolumeDescription,
): boolean {
  return (
    volume.availabilityZone === record.availabilityZone &&
    volume.encrypted &&
    volume.volumeType === "gp3" &&
    volume.sizeGB === record.sizeGB &&
    !volume.multiAttachEnabled &&
    tagsMatch(record, volume.tags)
  );
}

function cacheVolumeReusable(
  record: AWSCacheVolumeRecord,
  volume: AWSCacheVolumeDescription,
): boolean {
  return (
    cacheVolumePropertiesMatch(record, volume) &&
    volume.state === "available" &&
    volume.attachments.length === 0
  );
}

function retryableCacheVolumeRecord(
  record: AWSCacheVolumeRecord,
  error: unknown,
): AWSCacheVolumeRecord {
  const now = new Date().toISOString();
  return {
    ...record,
    state: "quarantined",
    lastError: error instanceof Error ? error.message : String(error),
    lastErrorAt: now,
    retryCount: (record.retryCount ?? 0) + 1,
    updatedAt: now,
  };
}

async function waitForVolume(
  cloud: AWSCacheVolumeCloud,
  volumeID: string,
  ready: (volume: AWSCacheVolumeDescription) => boolean,
): Promise<void> {
  const deadline = Date.now() + 120_000;
  while (true) {
    const volume = await cloud.describeCacheVolume(volumeID);
    if (ready(volume)) return;
    if (Date.now() >= deadline)
      throw new Error(`timed out waiting for AWS cache volume ${volumeID}`);
    await new Promise((resolve) => setTimeout(resolve, 2_000));
  }
}

function cacheVolumeBootstrap(records: AWSCacheVolumeRecord[], sshUser: string): string {
  const lines = ["    install -d -m 0755 /var/lib/crabbox/cache-volumes"];
  for (const record of records) {
    const serial = record.volumeID!.replaceAll("-", "");
    lines.push(
      "    cache_device=''",
      "    for cache_wait in $(seq 1 120); do",
      "      for cache_sys in /sys/block/nvme*n1; do",
      '        [ -r "$cache_sys/device/serial" ] || continue',
      `        [ "$(tr -d '[:space:]' < "$cache_sys/device/serial")" = ${shellQuote(serial)} ] || continue`,
      '        cache_device="/dev/${cache_sys##*/}"',
      "        break",
      "      done",
      '      [ -n "$cache_device" ] && break',
      "      sleep 1",
      "    done",
      `    [ -n "$cache_device" ] || { echo ${shellQuote(`cache volume ${record.volumeID} not attached`)} >&2; exit 1; }`,
      '    root_source="$(findmnt -n -o SOURCE /)"',
      '    [ "$cache_device" != "$root_source" ] || { echo "refusing cache root device" >&2; exit 1; }',
      `    cache_path=${shellQuote(record.path)}`,
      "    cache_walk=/",
      "    old_ifs=$IFS; IFS=/",
      "    for cache_part in ${cache_path#/}; do",
      '      [ -n "$cache_part" ] || continue',
      '      cache_walk="${cache_walk%/}/$cache_part"',
      '      [ ! -L "$cache_walk" ] || { echo "refusing symlink cache mount path" >&2; exit 1; }',
      "    done",
      "    IFS=$old_ifs",
      '    cache_fs="$(blkid -o value -s TYPE "$cache_device" 2>/dev/null || true)"',
      '    if [ -z "$cache_fs" ]; then',
      '      mkfs.ext4 -F "$cache_device"; cache_fs=ext4',
      '    elif [ "$cache_fs" = ext4 ]; then',
      '      if e2fsck -p "$cache_device"; then :; else cache_fsck=$?; [ "$cache_fsck" -le 1 ] || { wipefs -a "$cache_device"; mkfs.ext4 -F "$cache_device"; }; fi',
      "    fi",
      '    [ "$cache_fs" = ext4 ] || { echo "cache volume filesystem is not ext4" >&2; exit 1; }',
      '    install -d -m 0755 "$cache_path"',
      '    [ ! -L "$cache_path" ] || { echo "refusing symlink cache mount path" >&2; exit 1; }',
      '    [ "$(readlink -f -- "$cache_path")" = "$cache_path" ] || { echo "refusing indirect cache mount path" >&2; exit 1; }',
      '    mount -o nodev,nosuid "$cache_device" "$cache_path"',
    );
    if (record.fresh !== true) {
      lines.push(
        `    if [ ! -f "$cache_path/.crabbox-cache-abi" ] || [ "$(cat "$cache_path/.crabbox-cache-abi")" != ${shellQuote(awsCacheVolumeABI)} ]; then`,
        '      umount "$cache_path"',
        '      wipefs -a "$cache_device"',
        '      mkfs.ext4 -F "$cache_device"',
        '      mount -o nodev,nosuid "$cache_device" "$cache_path"',
        "    fi",
      );
    }
    lines.push(
      `    printf '%s\\n' ${shellQuote(awsCacheVolumeABI)} > "$cache_path/.crabbox-cache-abi"`,
      `    chown ${shellQuote(`${sshUser}:${sshUser}`)} "$cache_path"`,
    );
  }
  return lines.join("\n");
}

async function attachCacheVolume(
  cloud: AWSCacheVolumeCloud,
  volumeID: string,
  instanceID: string,
  device: string,
): Promise<void> {
  const deadline = Date.now() + 120_000;
  for (;;) {
    try {
      await cloud.attachCacheVolume(volumeID, instanceID, device);
      return;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("IncorrectInstanceState") || Date.now() >= deadline) throw error;
      await new Promise((resolve) => setTimeout(resolve, 2_000));
    }
  }
}

function cacheVolumeReadyChecks(bindings: CacheVolumeBinding[]): string {
  return bindings
    .flatMap((binding) => [
      `      mountpoint -q ${shellQuote(binding.path)}`,
      `      test "$(cat ${shellQuote(binding.path)}/.crabbox-cache-abi)" = ${shellQuote(awsCacheVolumeABI)}`,
    ])
    .join("\n");
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

function cacheVolumeNotFound(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes("InvalidVolume.NotFound") || message.includes("cache volume not found");
}

async function digest(value: string): Promise<string> {
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(bytes)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
