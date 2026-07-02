import { describe, expect, it } from "vitest";

import { artifactUploadResponse } from "../src/artifacts";
import type { Env } from "../src/types";

describe("artifact broker namespaces", () => {
  it("encodes slash, backslash, and Unicode identity values opaquely", async () => {
    const org = "org/团队";
    const owner = "alice\\团队";
    const response = await artifactUploadResponse(
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_PREFIX: "qa",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      } as Env,
      { prefix: "proof", files: [{ name: "result.txt", size: 1 }] },
      { org, owner },
    );

    const expected = `qa/v2/org/${Buffer.from(org).toString("base64url")}/owner/${Buffer.from(owner).toString("base64url")}/proof`;
    expect(response.prefix).toBe(expected);
    expect(response.files[0].key).toBe(`${expected}/result.txt`);
  });

  it("preserves distinct UTF-8 identity byte sequences", async () => {
    const [composed, decomposed] = await Promise.all(
      ["caf\u00e9", "cafe\u0301"].map(async (org) =>
        artifactUploadResponse(
          {
            CRABBOX_ARTIFACTS_BACKEND: "r2",
            CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
            CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
            CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
            CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
          } as Env,
          { files: [{ name: "result.txt", size: 1 }] },
          { org, owner: "alice" },
        ),
      ),
    );
    expect(composed.files[0].key).not.toBe(decomposed.files[0].key);
  });

  it.each([
    { label: "organization", scope: { org: "", owner: "alice" } },
    { label: "organization", scope: { org: "  ", owner: "alice" } },
    { label: "owner", scope: { org: "example-org", owner: "" } },
    { label: "owner", scope: { org: "example-org", owner: "\t" } },
  ])("rejects an empty $label identity", async ({ label, scope }) => {
    await expect(
      artifactUploadResponse(
        {
          CRABBOX_ARTIFACTS_BACKEND: "r2",
          CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
          CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
          CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
          CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
        } as Env,
        { files: [{ name: "result.txt", size: 1 }] },
        scope,
      ),
    ).rejects.toThrow(`artifact upload ${label} identity is required`);
  });
});
