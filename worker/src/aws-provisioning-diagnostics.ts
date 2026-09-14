/// <reference types="node/async_hooks" />
import { AsyncLocalStorage } from "node:async_hooks";

export type AWSTransportObservation = {
  requests: number;
  credentialsMs: number;
  credentialFailures: number;
  signInvocations: number;
  signCompletions: number;
  signFailures: number;
  signMs: number;
  requestMs: number;
  requestFailures: number;
};

const transportScope = new AsyncLocalStorage<(value: AWSTransportObservation) => void>();

export function currentAWSTransportObserver() {
  return transportScope.getStore();
}

const stepNames = [
  "key_pair",
  "image",
  "ingress_wait",
  "lifecycle_wait",
  "access_snapshot",
  "security_group",
  "security_group_lookup",
  "security_group_create",
  "security_group_vpc",
  "prune_ingress",
  "revoke_world",
  "revoke_world_absent",
  "authorize_ingress",
  "authorize_duplicate",
  "compact_ingress",
  "quota",
  "instance_create",
  "image_cleanup",
] as const;

type Step = (typeof stepNames)[number];
export type AWSProvisioningDiagnostics = ReturnType<typeof createAWSProvisioningDiagnostics>;

// Fixed buckets keep repeated permission calls bounded without losing their count.
// These observations are logs, never lease state or permission decisions.
export function createAWSProvisioningDiagnostics(leaseId: string, region: string) {
  const startedAt = Date.now();
  const steps = new Map<
    Step,
    {
      name: Step;
      count: number;
      totalMs: number;
      errors: number;
      transport?: AWSTransportObservation;
    }
  >(stepNames.map((name) => [name, { name, count: 0, totalMs: 0, errors: 0 }]));
  const record = (name: Step, durationMs: number, failed = false) => {
    const step = steps.get(name);
    if (!step) return;
    step.count += 1;
    step.totalMs += Math.max(0, durationMs);
    step.errors += Number(failed);
  };
  return {
    record,
    async measure<T>(name: Step, operation: () => Promise<T>): Promise<T> {
      const start = Date.now();
      let failed = true;
      try {
        // Scope the deepest measured operation, not a mutable shared EC2 client.
        // Concurrent leases and sibling preparations must never share counters.
        const result = await transportScope.run((observation) => {
          const step = steps.get(name);
          if (!step) return;
          if (!step.transport) {
            step.transport = { ...observation };
            return;
          }
          for (const key of [
            "requests",
            "credentialsMs",
            "credentialFailures",
            "signInvocations",
            "signCompletions",
            "signFailures",
            "signMs",
            "requestMs",
            "requestFailures",
          ] as const)
            step.transport[key] += observation[key];
        }, operation);
        failed = false;
        return result;
      } finally {
        record(name, Date.now() - start, failed);
      }
    },
    finish(outcome: "success" | "failure") {
      console.info(
        JSON.stringify({
          component: "crabbox_aws_provisioning",
          leaseId: leaseId.slice(0, 64),
          region: region.slice(0, 32),
          outcome,
          totalMs: Math.max(0, Date.now() - startedAt),
          steps: [...steps.values()].filter((step) => step.count > 0),
        }),
      );
    },
  };
}
