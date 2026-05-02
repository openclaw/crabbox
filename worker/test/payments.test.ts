import { describe, expect, it } from "vitest";

import { MppxConfigError, paymentConfigured, paymentGuardFromEnv } from "../src/payments";
import type { Env } from "../src/types";

const VALID_RECIPIENT = "0x3B098A4Bd4fd4414Be203c39057A82a00CD0d33F";

function env(overrides: Partial<Env> = {}): Env {
  return {
    CRABBOX_DEFAULT_ORG: "default-org",
    CRABBOX_SESSION_SECRET: "session-secret",
    CRABBOX_MPP_RECIPIENT: VALID_RECIPIENT,
    CRABBOX_MPP_SECRET_KEY: "mpp-secret",
    ...overrides,
  } as Env;
}

describe("paymentGuardFromEnv", () => {
  it("returns undefined when MPP recipient is unset", () => {
    expect(paymentGuardFromEnv(env({ CRABBOX_MPP_RECIPIENT: "" }))).toBeUndefined();
  });

  it("throws on a malformed recipient", () => {
    expect(() => paymentGuardFromEnv(env({ CRABBOX_MPP_RECIPIENT: "not-an-address" }))).toThrow(
      MppxConfigError,
    );
  });

  it("throws on a malformed currency", () => {
    expect(() => paymentGuardFromEnv(env({ CRABBOX_MPP_CURRENCY: "0xnothex" }))).toThrow(
      MppxConfigError,
    );
  });

  it("throws when MPP secret key is missing", () => {
    expect(() => paymentGuardFromEnv(env({ CRABBOX_MPP_SECRET_KEY: "" }))).toThrow(MppxConfigError);
  });

  it("throws when session secret is missing while MPP is configured", () => {
    expect(() => paymentGuardFromEnv(env({ CRABBOX_SESSION_SECRET: "" }))).toThrow(MppxConfigError);
  });

  it("succeeds with a fully valid env", () => {
    expect(paymentGuardFromEnv(env())).toBeDefined();
  });
});

describe("paymentConfigured", () => {
  it("is false when recipient is unset", () => {
    expect(paymentConfigured(env({ CRABBOX_MPP_RECIPIENT: "" }))).toBe(false);
  });

  it("is false on malformed recipient (fail-closed)", () => {
    expect(paymentConfigured(env({ CRABBOX_MPP_RECIPIENT: "not-an-address" }))).toBe(false);
  });

  it("is false when MPP secret key is missing", () => {
    expect(paymentConfigured(env({ CRABBOX_MPP_SECRET_KEY: "" }))).toBe(false);
  });

  it("is true with a fully valid env", () => {
    expect(paymentConfigured(env())).toBe(true);
  });
});
