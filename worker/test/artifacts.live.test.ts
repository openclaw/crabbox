import { AwsClient } from "aws4fetch";
import { describe, expect, it } from "vitest";

import { artifactUploadResponse } from "../src/artifacts";
import type { Env } from "../src/types";

const liveAccessKeyID = process.env.CRABBOX_ARTIFACTS_ACCESS_KEY_ID;
const liveSecretAccessKey = process.env.CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY;
const liveSessionToken = process.env.CRABBOX_ARTIFACTS_SESSION_TOKEN;
const liveBucket = process.env.CRABBOX_ARTIFACTS_BUCKET;
const liveEndpointURL = process.env.CRABBOX_ARTIFACTS_ENDPOINT_URL;
const liveConfigured =
  process.env.CRABBOX_ARTIFACTS_LIVE === "1" &&
  Boolean(liveAccessKeyID && liveSecretAccessKey && liveBucket && liveEndpointURL);

describe("artifact broker live", () => {
  it.skipIf(!liveConfigured)(
    "live: signs private R2 reads and leaves no object residue",
    async () => {
      const body = new TextEncoder().encode(`crabbox-artifact-live-${crypto.randomUUID()}\n`);
      const env = {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: liveBucket,
        CRABBOX_ARTIFACTS_PREFIX: "crabbox-live",
        CRABBOX_ARTIFACTS_BASE_URL: "https://public-reads.invalid",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: liveEndpointURL,
        CRABBOX_ARTIFACTS_REGION: "auto",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: liveAccessKeyID,
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: liveSecretAccessKey,
        CRABBOX_ARTIFACTS_SESSION_TOKEN: liveSessionToken,
      } as Env;
      const response = await artifactUploadResponse(
        env,
        {
          prefix: crypto.randomUUID(),
          files: [{ name: "proof.txt", size: body.byteLength, contentType: "text/plain" }],
        },
        { org: "example-org", owner: "alice@example.com" },
      );
      const grant = response.files[0]!;
      const readURL = new URL(grant.url);
      const objectURL = new URL(readURL);
      objectURL.search = "";

      expect(response.accessPolicy).toBe("signed-url");
      expect(grant.accessPolicy).toBe("signed-url");
      expect(readURL.origin).toBe(new URL(liveEndpointURL!).origin);
      expect(readURL.searchParams.get("X-Amz-Signature")).toBeTruthy();

      const client = new AwsClient({
        accessKeyId: liveAccessKeyID!,
        secretAccessKey: liveSecretAccessKey!,
        sessionToken: liveSessionToken,
        service: "s3",
        region: "auto",
        retries: 0,
      });
      try {
        const upload = await fetch(grant.upload.url, {
          method: grant.upload.method,
          headers: grant.upload.headers,
          body,
        });
        expect(upload.ok).toBe(true);

        const download = await fetch(grant.url);
        expect(download.ok).toBe(true);
        expect(new Uint8Array(await download.arrayBuffer())).toEqual(body);
      } finally {
        const cleanup = await client.fetch(objectURL, { method: "DELETE" });
        expect(cleanup.ok).toBe(true);
      }

      const missing = await fetch(grant.url);
      expect(missing.status).toBe(404);
    },
    30_000,
  );
});
