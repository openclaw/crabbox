import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { expect, it } from "vitest";

import { cloudInit } from "../src/bootstrap";
import { leaseConfig } from "../src/config";

it.skipIf(process.platform === "win32")(
  "reuses the prepared browser through the generated coordinator bootstrap",
  () => {
    const script = cloudInit(
      leaseConfig({ provider: "aws", browser: true, sshPublicKey: "ssh-ed25519 fixture" }),
    );
    const start = script.indexOf("    systemctl enable ssh || true\n");
    const end = script.indexOf('    if [ -n "$browser_path" ]; then\n', start);
    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeGreaterThan(start);
    const fixture = readFileSync(
      new URL("../../testdata/bootstrap/installed-browser-fixture.sh", import.meta.url),
      "utf8",
    );
    const directory = mkdtempSync(join(tmpdir(), "crabbox-bootstrap-reuse-"));
    try {
      const result = spawnSync(
        "bash",
        ["-c", fixture + "\n" + script.slice(start, end) + '\nprintf "%s\\n" "$browser_path"\n'],
        {
          env: {
            PATH: "/usr/bin:/bin",
            FIXTURE_BIN: join(directory, "bin"),
            FIXTURE_LOG: join(directory, "calls"),
          },
          encoding: "utf8",
          timeout: 5000,
        },
      );
      expect(result.status).toBe(0);
      expect(result.stdout.trim()).toBe(join(directory, "bin", "google-chrome"));
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
);
