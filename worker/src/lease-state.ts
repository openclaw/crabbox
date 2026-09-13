import type { LeaseRecord } from "./types";

// An elapsed heartbeat deadline does not terminalize a lease or free capacity.
// The owning lifecycle transition must commit a terminal state first.
export function leaseIsLive(lease: Pick<LeaseRecord, "state">): boolean {
  return lease.state === "active" || lease.state === "provisioning";
}

// Legacy rows without a lifecycle field keep their managed interpretation.
export function isRegisteredLease(lease: Pick<LeaseRecord, "lifecycle">): boolean {
  return lease.lifecycle === "registered";
}
