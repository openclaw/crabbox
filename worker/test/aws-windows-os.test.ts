import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { leaseConfig, type LeaseConfig } from "../src/config";
import type { Env } from "../src/types";

afterEach(() => vi.unstubAllGlobals());

describe("AWS Windows OS selector", () => {
  it.each([undefined, "ubuntu:26.04", "windows-server:2022", "windows-server:2025"])(
    "resolves %s from Amazon and preserves exact AMI overrides",
    async (os) => {
      const queries: URLSearchParams[] = [];
      vi.stubGlobal(
        "fetch",
        vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
          const request = input instanceof Request ? input : new Request(input, init);
          queries.push(new URLSearchParams(await request.text()));
          return new Response(
            `<DescribeImagesResponse><imagesSet><item><imageId>ami-old</imageId><creationDate>2026-01-01</creationDate></item><item><imageId>ami-latest</imageId><creationDate>2026-09-01</creationDate></item></imagesSet></DescribeImagesResponse>`,
          );
        }),
      );
      const env = { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env;
      const client = new EC2SpotClient(env, "eu-west-1") as unknown as {
        resolveAMI(config: LeaseConfig): Promise<string>;
      };
      const config = leaseConfig({
        provider: "aws",
        target: "windows",
        os,
        sshPublicKey: "ssh-ed25519 test",
      });
      expect(await client.resolveAMI(config)).toBe("ami-latest");
      expect(queries).toHaveLength(1);
      expect(queries[0]!.get("Owner.1")).toBe("amazon");
      expect(queries[0]!.get("Filter.1.Value.1")).toBe("x86_64");
      expect(queries[0]!.get("Filter.2.Value.1")).toBe(
        `Windows_Server-${os === "windows-server:2025" ? "2025" : "2022"}-English-Full-Base-*`,
      );
      expect(await client.resolveAMI({ ...config, awsAMI: "ami-pinned" })).toBe("ami-pinned");
      env.CRABBOX_AWS_AMI = "ami-operator";
      expect(await client.resolveAMI(config)).toBe("ami-operator");
      expect(queries).toHaveLength(1);
    },
  );

  it.each(["2022", "2025"])("supports Windows Server %s in native and WSL2 mode", (year) => {
    for (const windowsMode of ["normal", "wsl2"] as const) {
      expect(
        leaseConfig({
          provider: "aws",
          target: "windows",
          windowsMode,
          os: `windows-server:${year}`,
          sshPublicKey: "ssh-ed25519 test",
        }).os,
      ).toBe(`windows-server:${year}`);
    }
  });

  it.each([
    { provider: "aws", target: "linux" },
    { provider: "aws", target: "macos" },
    { provider: "azure", target: "windows" },
    { provider: "hetzner", target: "linux" },
  ] as const)("rejects incompatible $provider/$target before provisioning", (input) => {
    expect(() =>
      leaseConfig({ ...input, os: "windows-server:2025", sshPublicKey: "ssh-ed25519 test" }),
    ).toThrow("requires provider=aws target=windows");
  });
});
