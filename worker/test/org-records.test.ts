import { describe, expect, it } from "vitest";

import { publicLeaseRecord } from "../src/org-records";
import type { LeaseRecord } from "../src/types";

describe("public lease provider identity", () => {
  it.each(["provisioning", "active", "released", "failed"] as const)(
    "retains the stored app, scope and region for %s leases",
    (state) => {
      const record: LeaseRecord = {
        id: "cbx_abcdef123456",
        provider: "koyeb",
        target: "linux",
        state,
        cloudID: "11111111-1111-4111-8111-111111111111",
        serverID: 0,
        serverName: "worker",
        providerKey: "test-key",
        host: state === "released" ? "" : "worker.my-app-workers-2.internal",
        sshUser: "crabbox",
        sshPort: "22",
        workRoot: "/workspace",
        keep: false,
        ttlSeconds: 3600,
        estimatedHourlyUSD: 0.1,
        maxEstimatedUSD: 1,
        owner: "alice@example.com",
        org: "example-org",
        profile: "general",
        class: "standard",
        serverType: "large",
        providerProject: "66666666-6666-4666-8666-666666666666",
        providerScope: "koyeb:context:v1:stored-second-app",
        region: "was",
        createdAt: "2026-09-15T00:00:00Z",
        updatedAt: "2026-09-15T00:00:00Z",
        lastTouchedAt: "2026-09-15T00:00:00Z",
        expiresAt: "2026-09-15T01:00:00Z",
        idleTimeoutSeconds: 600,
        releaseDeletesServer: true,
        ...(state === "released" ? { cleanupCompletedAt: "2026-09-15T00:30:00Z" } : {}),
      };
      const stored = structuredClone(record);
      const visible = publicLeaseRecord(record);
      expect(visible).toMatchObject({
        providerProject: record.providerProject,
        providerScope: record.providerScope,
        region: record.region,
        ...(state === "released" ? { cleanupStatus: "complete", host: "" } : {}),
      });
      expect(record).toEqual(stored);
    },
  );
});
