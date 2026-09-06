import { describe, expect, it } from "vitest";

import {
  ProvisioningAttemptHistory,
  ProvisioningAttemptsError,
} from "../src/provisioning-attempts";

describe("provisioning attempt history", () => {
  it("keeps snapshots independent and preserves terminal text and cause", () => {
    const history = new ProvisioningAttemptHistory();
    expect(history.result()).toEqual({});
    const attempt = { serverType: "small", message: "capacity unavailable" };
    history.record(attempt, "small: capacity unavailable");
    attempt.message = "changed";
    const snapshot = history.result();
    snapshot.attempts![0]!.message = "changed snapshot";
    const cause = new Error("original");
    const error = history.error("exact type: ", { cause });
    history.record({ serverType: "large", message: "quota exceeded" }, "large: quota exceeded");
    expect(error).toMatchObject({
      message: "exact type: small: capacity unavailable",
      cause,
      attempts: [{ serverType: "small", message: "capacity unavailable" }],
    });
    expect(Object.isFrozen(error.attempts)).toBe(true);
    expect(Object.isFrozen(error.attempts[0])).toBe(true);
    expect(history.result().attempts).toHaveLength(2);
  });

  it("carries child failures once while retaining the parent's diagnostic", () => {
    const history = new ProvisioningAttemptHistory();
    const child = new ProvisioningAttemptsError("child failure", [
      { serverType: "small", message: "quota" },
    ]);
    history.recordFailure(
      child,
      { serverType: "unused", message: "synthetic" },
      "region: child failure",
    );
    const error = history.error();
    expect(error.message).toBe("region: child failure");
    expect(error.attempts).toEqual(child.attempts);
  });

  it("records an ordinary later failure after preceding successful-stage history", () => {
    const history = new ProvisioningAttemptHistory();
    const preceding = { serverType: "small", message: "quota" };
    const later = { serverType: "large", message: "readiness timeout" };
    history.recordFailure(new Error("readiness timeout"), later, "region: readiness timeout", [
      preceding,
    ]);
    expect(history.error()).toMatchObject({
      message: "region: readiness timeout",
      attempts: [preceding, later],
    });
  });
});
