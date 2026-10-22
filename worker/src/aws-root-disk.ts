import { sanitizeAWSRegion } from "./aws-region";
import type { AWSRootDiskIdentity, ProviderMachine } from "./types";

// Only provider observations during acquisition, before handing the machine to
// a caller, can establish this binding. A later mapping is not original evidence.
export function acquisitionRootDisk(
  server: ProviderMachine,
  leaseID: string,
  providerScope: string,
  region: string,
): AWSRootDiskIdentity | undefined {
  if (!server.awsRootVolumeID || !server.awsRootDeviceName) return undefined;
  const identity = {
    leaseID,
    instanceID: server.cloudID,
    volumeID: server.awsRootVolumeID,
    deviceName: server.awsRootDeviceName,
    region,
    providerScope,
  };
  return server.provider === "aws" &&
    server.labels["lease"] === leaseID &&
    validAWSRootDiskIdentity(identity)
    ? identity
    : undefined;
}

export function validAWSRootDiskIdentity(value: unknown): value is AWSRootDiskIdentity {
  if (!value || typeof value !== "object") return false;
  const disk = value as AWSRootDiskIdentity;
  return (
    typeof disk.leaseID === "string" &&
    /^cbx_[a-f0-9]{12}$/.test(disk.leaseID) &&
    typeof disk.instanceID === "string" &&
    /^i-[a-f0-9]+$/.test(disk.instanceID) &&
    typeof disk.volumeID === "string" &&
    /^vol-[a-f0-9]+$/.test(disk.volumeID) &&
    typeof disk.deviceName === "string" &&
    /^\/dev\/[a-z0-9]+$/.test(disk.deviceName) &&
    typeof disk.region === "string" &&
    Boolean(disk.region) &&
    sanitizeAWSRegion(disk.region) === disk.region &&
    typeof disk.providerScope === "string" &&
    /^aws:account:\d{12}$/.test(disk.providerScope)
  );
}

export function sameAWSRootDisk(a: AWSRootDiskIdentity, b: AWSRootDiskIdentity): boolean {
  return (
    a.leaseID === b.leaseID &&
    a.instanceID === b.instanceID &&
    a.volumeID === b.volumeID &&
    a.deviceName === b.deviceName &&
    a.region === b.region &&
    a.providerScope === b.providerScope
  );
}
