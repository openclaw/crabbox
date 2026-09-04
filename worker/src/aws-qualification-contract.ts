export const awsQualificationMaxRunMs = 120 * 60 * 1000;

export const awsQualificationInstanceTypes = ["t3.small", "t3a.small"] as const;

export const awsQualificationAttestationVersion = 1;

export type AWSQualificationService = "ec2" | "servicequotas" | "sts";

export interface AWSQualificationRunIdentity {
  runId: string;
  owner: string;
  candidateSha: string;
  candidateWorker: string;
  deploymentHash: string;
  expiresAt: string;
}

export interface AWSQualificationControllerProps {
  deploymentHash: string;
}

export interface AWSQualificationRequest {
  opId: string;
  region: string;
  service: AWSQualificationService;
  action: string;
  parameters: Record<string, unknown>;
}

export interface AWSQualificationResponse {
  status: number;
  body: string;
}

export interface AWSQualificationTransportBinding {
  execute(request: AWSQualificationRequest): Promise<AWSQualificationResponse>;
}

export type AWSQualificationCleanupState = "claimed" | "finalizing" | "finalized";

export interface AWSQualificationRegistryRecord {
  version: 1;
  runId: string;
  candidateSha: string;
  candidateWorker: string;
  deploymentHash: string;
  expiresAt: string;
  cleanupState: AWSQualificationCleanupState;
  claimedAt: string;
  finalizedAt?: string;
}

export interface AWSQualificationResourceCounts {
  images: number;
  instances: number;
  keyPairs: number;
  snapshots: number;
  volumes: number;
}

export interface AWSQualificationOperationEvidence {
  version: 1;
  opDigest: string;
  requestDigest: string;
  action: string;
  requestedAt: string;
  denialReason?: string;
  signerDispatches: Array<{
    beforeSequence: number;
    beforeAt: string;
    afterSequence?: number;
    afterAt?: string;
    outcome?: "accepted" | "rejected";
    statusClass?: number;
  }>;
}

export interface AWSQualificationFinalReceipt {
  version: 1;
  startedAt: string;
  finalizeAttempts: number;
  resourcesAtStart: AWSQualificationResourceCounts;
  pendingAtStart: Array<{
    opDigest: string;
    requestDigest: string;
    action: string;
    phase: "prepared" | "dispatched" | "legacy";
  }>;
  cleanupAttemptsTotal: number;
  cleanupAttemptsTruncated: number;
  cleanupAttempts: Array<{
    sequence: number;
    action: string;
    targetDigest: string;
    startedAt: string;
    completedAt?: string;
    outcome?: "accepted" | "absent" | "rejected" | "error";
    statusClass?: number;
  }>;
  inventoryTotal: number;
  inventoryTruncated: number;
  inventory: Array<{
    sequence: number;
    phase: "pre-cleanup" | "post-cleanup";
    at: string;
    outcome: "accepted" | "rejected";
    counts: AWSQualificationResourceCounts;
    failureCodes: string[];
  }>;
  verificationTotal: number;
  verificationTruncated: number;
  verification: Array<{
    sequence: number;
    action: string;
    at: string;
    outcome: "accepted" | "absent" | "present" | "rejected" | "error";
  }>;
  completedAt?: string;
  finalCounts?: AWSQualificationResourceCounts;
  failureCodes: string[];
}

export interface AWSQualificationAttestation {
  version: 1;
  runId: string;
  candidateSha: string;
  candidateWorker: string;
  deploymentHash: string;
  authoritySha: string;
  authorityVersion: string;
  policyHash: string;
  enrolledAt: string;
  expiresAt: string;
  finalizingAt?: string;
  finalized: boolean;
  finalizedAt?: string;
  operations: AWSQualificationOperationEvidence[];
  finalReceipt?: AWSQualificationFinalReceipt;
}
