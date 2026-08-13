import { AwsClient } from "aws4fetch";
import { describe, expect, it } from "vitest";

import { artifactUploadResponse } from "../src/artifacts";
import { sha256Hex } from "../src/auth";
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
    "live: R2 enforces signed checksums and leaves no rejected object residue",
    async () => {
      const nonce = crypto.randomUUID();
      const matchingText = `crabbox-artifact-live-${nonce}-match\n`;
      const mismatchingText = `crabbox-artifact-live-${nonce}-wrong\n`;
      const matchingBody = new TextEncoder().encode(matchingText);
      const mismatchingBody = new TextEncoder().encode(mismatchingText);
      expect(mismatchingBody.byteLength).toBe(matchingBody.byteLength);
      const declaredSHA256 = await sha256Hex(matchingText);
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
      const prefix = crypto.randomUUID();
      const response = await artifactUploadResponse(
        env,
        {
          prefix: `${prefix}/matching`,
          files: [
            {
              name: "proof.txt",
              size: matchingBody.byteLength,
              contentType: "text/plain",
              sha256: declaredSHA256,
            },
          ],
        },
        { org: "example-org", owner: "alice@example.com" },
      );
      const grant = response.files[0]!;
      const readURL = new URL(grant.url);

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
      const cleanupURLs: URL[] = [];
      const matchingObjectURL = new URL(readURL);
      matchingObjectURL.search = "";
      cleanupURLs.push(matchingObjectURL);
      try {
        const upload = await fetch(grant.upload.url, {
          method: grant.upload.method,
          headers: grant.upload.headers,
          body: matchingBody,
        });
        expect(upload.ok).toBe(true);

        const download = await fetch(grant.url);
        expect(download.ok).toBe(true);
        expect(new Uint8Array(await download.arrayBuffer())).toEqual(matchingBody);

        const mismatchResponse = await artifactUploadResponse(
          env,
          {
            prefix: `${prefix}/mismatch`,
            files: [
              {
                name: "proof.txt",
                size: mismatchingBody.byteLength,
                contentType: "text/plain",
                sha256: declaredSHA256,
              },
            ],
          },
          { org: "example-org", owner: "alice@example.com" },
        );
        const mismatchGrant = mismatchResponse.files[0]!;
        const mismatchObjectURL = new URL(mismatchGrant.url);
        mismatchObjectURL.search = "";
        cleanupURLs.push(mismatchObjectURL);

        const mismatchUpload = await fetch(mismatchGrant.upload.url, {
          method: mismatchGrant.upload.method,
          headers: mismatchGrant.upload.headers,
          body: mismatchingBody,
        });
        expect(mismatchUpload.status).toBe(400);

        const rejectedObject = await fetch(mismatchGrant.url);
        expect(rejectedObject.status).toBe(404);
      } finally {
        const cleanups = await Promise.all(
          cleanupURLs.map((objectURL) => client.fetch(objectURL, { method: "DELETE" })),
        );
        for (const cleanup of cleanups) {
          expect(cleanup.ok).toBe(true);
        }
      }

      const missing = await fetch(grant.url);
      expect(missing.status).toBe(404);
    },
    30_000,
  );
});
