export interface LeasePolicy {
  defaultTTLSeconds: number;
  defaultIdleTimeoutSeconds: number;
  maxTTLSeconds: number;
  extension: "allow" | "deny";
  autoExtend: "allow" | "deny";
  runtimeExtension?: boolean;
}

export type PolicyOverrides = {
  orgs?: Record<string, Partial<Omit<LeasePolicy, "runtimeExtension">>>;
  owners?: Record<string, Partial<Omit<LeasePolicy, "runtimeExtension">>>;
};

export function isLeasePolicyOverrides(value: unknown): value is PolicyOverrides;
