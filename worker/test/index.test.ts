import { describe, expect, it } from "vitest";

import { extractCredentialPayer } from "../src/index";

function paymentHeader(credential: {
  source?: string;
  challenge?: unknown;
  payload?: unknown;
}): string {
  const json = JSON.stringify(credential);
  const b64 = Buffer.from(json, "utf-8")
    .toString("base64")
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/, "");
  return `Payment ${b64}`;
}

describe("extractCredentialPayer", () => {
  it("returns the payer address from a did:pkh source", () => {
    const credential = {
      challenge: { id: "abc", realm: "test" },
      payload: { type: "transaction", signature: "0xff" },
      source: "did:pkh:eip155:4217:0xD6242951159Ec311f5810b2b9fC6427999D6a336",
    };
    const request = new Request("https://example.test/v1/leases", {
      method: "POST",
      headers: { Authorization: paymentHeader(credential) },
    });
    expect(extractCredentialPayer(request)).toBe("0xD6242951159Ec311f5810b2b9fC6427999D6a336");
  });

  it("returns undefined for missing Authorization", () => {
    const request = new Request("https://example.test/v1/leases", { method: "POST" });
    expect(extractCredentialPayer(request)).toBeUndefined();
  });

  it("returns undefined for non-Payment scheme", () => {
    const request = new Request("https://example.test/v1/leases", {
      method: "POST",
      headers: { Authorization: "Bearer some-token" },
    });
    expect(extractCredentialPayer(request)).toBeUndefined();
  });

  it("returns undefined for malformed credential", () => {
    const request = new Request("https://example.test/v1/leases", {
      method: "POST",
      headers: { Authorization: "Payment not-base64-at-all" },
    });
    expect(extractCredentialPayer(request)).toBeUndefined();
  });
});
