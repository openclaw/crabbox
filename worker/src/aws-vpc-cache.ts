import type { CoordinatorStorageView } from "./coordinator-runtime";

const vpcCacheTTL = 5 * 60_000;

// Non-secret routing hints only; each create still reads and validates its group.
export class AWSVPCCache {
  constructor(
    private readonly storage: Pick<CoordinatorStorageView, "get" | "put" | "delete">,
    private readonly account: string,
    private readonly region: string,
  ) {}

  private key(subnet: string): string {
    return `aws-vpc-cache:${this.account}:${this.region}:${subnet || "default"}`;
  }

  async resolve(subnet: string, load: () => Promise<string>): Promise<string> {
    const key = this.key(subnet);
    const cached = await this.storage.get<{ vpcID: string; expiresAt: number }>(key);
    if (cached?.vpcID && cached.expiresAt > Date.now()) return cached.vpcID;
    const vpcID = await load();
    await this.storage.put(key, { vpcID, expiresAt: Date.now() + vpcCacheTTL });
    return vpcID;
  }

  async invalidate(subnet: string): Promise<void> {
    await this.storage.delete(this.key(subnet));
  }
}
