export const awsQualificationMaxRunMs = 120 * 60 * 1000;

export const awsQualificationInstanceTypes = ["t3.small", "t3a.small"] as const;

export type AWSQualificationService = "ec2" | "servicequotas" | "ssm" | "sts";

export interface AWSQualificationRunIdentity {
  runId: string;
  owner: string;
  candidateSha: string;
  expiresAt: string;
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
