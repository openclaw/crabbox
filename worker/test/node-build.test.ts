import { spawn } from "node:child_process";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { build } from "esbuild";
import { describe, expect, it } from "vitest";

describe("Node production bundle", () => {
  it("loads external CommonJS dependencies before validating configuration", async () => {
    const workerDirectory = fileURLToPath(new URL("..", import.meta.url));
    const outputRoot = join(workerDirectory, "dist-node");
    await mkdir(outputRoot, { recursive: true });
    const outputDirectory = await mkdtemp(join(outputRoot, "test-"));
    const outfile = join(outputDirectory, "server.mjs");

    try {
      await build({
        absWorkingDir: workerDirectory,
        entryPoints: ["node/server.ts"],
        outfile,
        bundle: true,
        packages: "external",
        platform: "node",
        format: "esm",
        target: "node22",
      });

      const result = await runNode(outfile);
      expect(result.code).not.toBe(0);
      expect(result.stderr).toContain("DATABASE_URL is required");
      expect(result.stderr).not.toContain("Named export");
    } finally {
      await rm(outputDirectory, { recursive: true, force: true });
    }
  });
});

async function runNode(entrypoint: string): Promise<{
  code: number | null;
  stderr: string;
}> {
  const env = { ...process.env };
  delete env.DATABASE_URL;

  return await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [entrypoint], { env });
    let stderr = "";
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk: string) => {
      stderr += chunk;
    });
    child.once("error", reject);
    child.once("close", (code) => resolve({ code, stderr }));
  });
}
