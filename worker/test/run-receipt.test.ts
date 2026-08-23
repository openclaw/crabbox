import { describe, expect, it } from "vitest";

import { verifyTerminalReceipt } from "../src/run-receipt";
import type { RunRecord, TerminalRunReceipt } from "../src/types";

describe("terminal run receipt", () => {
  it("verifies the Go signing golden", async () => {
    const receipt: TerminalRunReceipt = {
      schema_version: 2,
      receipt_type: "terminal",
      started_at: "2026-08-23T10:00:00Z",
      ended_at: "2026-08-23T10:00:01Z",
      provider: "aws",
      lease_id: "cbx_abc123",
      slug: "blue-lobster",
      run_id: "run_123",
      command: "sh -c \"printf '<ok>\\n'; exit 17\"",
      command_sha256: "sha256:5bcb64bd81339235f6b76ab37c6f4e5febc1b787fcaf4e27783410b477c8ed5c",
      exit_code: 17,
      sync_ms: 100,
      command_ms: 900,
      duration_ms: 1000,
      log_sha256: "sha256:6da5b18878e110928643ebcc38dbb7552cf3a296eea56493e23edc2b93eecff8",
      retained_log_sha256:
        "sha256:6da5b18878e110928643ebcc38dbb7552cf3a296eea56493e23edc2b93eecff8",
      log_truncated: false,
      public_key: "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=",
      signer: "sha256:56475aa75463474c0285df5dbf2bcab73da651358839e9b77481b2eab107708c",
      signature:
        "3N80Q2zyXRS0g3V3phmRmegGwIkO6n9eiXXO+d36LKbfbAl7FDTAupodcy76YKk7WD5voQk9R5a9prqjNmxKDg==",
    };
    const run: RunRecord = {
      id: "run_123",
      leaseID: "cbx_abc123",
      slug: "blue-lobster",
      owner: "alice@example.com",
      org: "example-org",
      provider: "aws",
      class: "standard",
      serverType: "small",
      command: ["sh", "-c", "printf '<ok>\\n'; exit 17"],
      state: "running",
      logBytes: 0,
      logTruncated: false,
      startedAt: "2026-08-23T10:00:00Z",
    };
    await expect(
      verifyTerminalReceipt(receipt, {
        run,
        exitCode: 17,
        syncMs: 100,
        commandMs: 900,
        log: "failed\n",
        logTruncated: false,
        observedAt: new Date("2026-08-23T10:00:01Z"),
      }),
    ).resolves.toEqual(receipt);
  });
});
