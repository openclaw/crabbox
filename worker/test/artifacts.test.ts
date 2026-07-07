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

  it("keeps reads signed when a base URL is not explicitly public", async () => {
    const response = await artifactUploadResponse(
      artifactEnv({ CRABBOX_ARTIFACTS_BASE_URL: "https://artifacts.example.com" }),
      { prefix: "proof", files: [{ name: "result.txt", size: 1 }] },
      { org: "example-org", owner: "alice" },
    );

    expect(response.accessPolicy).toBe("signed-url");
    expect(response.files[0].accessPolicy).toBe("signed-url");
    expect(response.files[0].url).toContain("X-Amz-Signature=");
    expect(response.files[0].url).not.toContain("artifacts.example.com");
    expect(response.prefix).not.toContain("/public/");
  });

  it("requires a base URL for explicit public reads", async () => {
    await expect(
      artifactUploadResponse(
        artifactEnv({ CRABBOX_ARTIFACTS_PUBLIC_READS: "1" }),
        { files: [{ name: "result.txt", size: 1 }] },
        { org: "example-org", owner: "alice" },
      ),
    ).rejects.toThrow("artifact broker public reads require CRABBOX_ARTIFACTS_BASE_URL");
  });

  it("uses an unguessable per-grant namespace for explicit public reads", async () => {
    const env = artifactEnv({
      CRABBOX_ARTIFACTS_BASE_URL: "https://artifacts.example.com",
      CRABBOX_ARTIFACTS_PUBLIC_READS: "true",
    });
    const [first, second] = await Promise.all(
      [1, 2].map(() =>
        artifactUploadResponse(
          env,
          { prefix: "proof", files: [{ name: "result.txt", size: 1 }] },
          { org: "example-org", owner: "alice" },
        ),
      ),
    );

    expect(first.accessPolicy).toBe("public");
    expect(first.files[0].url).toBe(`https://artifacts.example.com/${first.files[0].key}`);
    expect(first.prefix).toMatch(/\/public\/[A-Za-z0-9_-]{22}\/proof$/u);
    expect(second.prefix).not.toBe(first.prefix);
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

function artifactEnv(overrides: Partial<Env> = {}): Env {
  return {
    CRABBOX_ARTIFACTS_BACKEND: "r2",
    CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
    CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
    CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
    CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
    ...overrides,
  } as Env;
}
