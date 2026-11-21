import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { applyAWSRunInstanceTargetOptions, awsLaunchCandidates } from "../src/aws";
import {
  awsInstanceTypeProvidesLinuxKVM,
  awsInstanceTypeSupportsNestedVirtualization,
  awsNestedVirtualizationFallbackInstanceType,
  awsNestedVirtualizationFamilies,
  awsNestedVirtualizationInstanceTypeCandidatesForClass,
  InvalidAWSNestedVirtualizationError,
  leaseConfig,
} from "../src/config";
import type { LeaseRequest } from "../src/types";

const classes = ["tiny", "small", "standard", "fast", "large", "beast"];

function nestedLease(input: Partial<LeaseRequest> = {}) {
  return leaseConfig({
    provider: "aws",
    sshPublicKey: "ssh-ed25519 test",
    awsNestedVirtualization: true,
    ...input,
  });
}

describe("AWS nested virtualization", () => {
  it("classifies supported families, metal sizes and unsupported types", () => {
    for (const family of awsNestedVirtualizationFamilies) {
      expect(awsInstanceTypeSupportsNestedVirtualization(`${family}.large`)).toBe(true);
    }
    expect(awsInstanceTypeSupportsNestedVirtualization("C8I.2XLARGE")).toBe(true);
    for (const value of ["c7a.2xlarge", "m7a.large", "t3.small", "c7g.2xlarge", "c8i", ""]) {
      expect(awsInstanceTypeProvidesLinuxKVM(value)).toBe(false);
    }
    expect(awsInstanceTypeSupportsNestedVirtualization("c7i.metal-24xl")).toBe(false);
    expect(awsInstanceTypeProvidesLinuxKVM("c7i.metal-24xl")).toBe(true);
    expect(awsInstanceTypeProvidesLinuxKVM("m5.metal")).toBe(true);
  });

  it("matches the Go CLI family list and class candidates", () => {
    const goAWS = readFileSync(new URL("../../internal/cli/aws.go", import.meta.url), "utf8");
    const familyBlock = /var awsNestedVirtualizationFamilies = map\[string\]bool\{([^}]*)\}/.exec(
      goAWS,
    )?.[1];
    expect(familyBlock).toBeDefined();
    const goFamilies = [...familyBlock!.matchAll(/"([a-z0-9-]+)":\s*true/g)].map(
      (match) => match[1],
    );
    expect(goFamilies.toSorted()).toEqual([...awsNestedVirtualizationFamilies].toSorted());

    const start = goAWS.indexOf("func AWSNestedVirtualizationInstanceTypeCandidatesForClass(");
    expect(start).toBeGreaterThanOrEqual(0);
    const body = goAWS.slice(start, goAWS.indexOf("\n}\n", start));
    for (const name of classes) {
      const values = new RegExp(`case "${name}":\\s*return \\[\\]string\\{([^}]*)\\}`).exec(
        body,
      )?.[1];
      expect(values).toBeDefined();
      const goCandidates = [...values!.matchAll(/"([^"]+)"/g)].map((match) => match[1]);
      expect(awsNestedVirtualizationInstanceTypeCandidatesForClass(name)).toEqual(goCandidates);
    }
    expect(goAWS).toContain(
      `const awsNestedVirtualizationFallbackInstanceType = "${awsNestedVirtualizationFallbackInstanceType}"`,
    );
  });

  it("selects only supported families for class-based Linux leases", () => {
    for (const name of classes) {
      const config = nestedLease({ class: name });
      expect(config.awsNestedVirtualization).toBe(true);
      const candidates = awsLaunchCandidates(config);
      expect(candidates).toEqual(awsNestedVirtualizationInstanceTypeCandidatesForClass(name));
      expect(config.serverType).toBe(candidates[0]);
      for (const candidate of candidates) {
        expect(awsInstanceTypeSupportsNestedVirtualization(candidate)).toBe(true);
      }
    }
  });

  it("replaces a class-derived non-nested serverType from the client", () => {
    const config = nestedLease({ class: "beast", serverType: "c7a.48xlarge" });
    expect(config.serverType).toBe("c8i.48xlarge");
    expect(awsLaunchCandidates(config)).not.toContain("c7a.48xlarge");
  });

  it("keeps fallback candidates inside supported families", () => {
    const candidates = awsLaunchCandidates({
      serverType: "c7a.48xlarge",
      serverTypeExplicit: false,
      class: "c8i.4xlarge",
      target: "linux",
      windowsMode: "normal",
      architecture: "amd64",
      awsNestedVirtualization: true,
    });
    expect(candidates).toEqual(["c8i.4xlarge", awsNestedVirtualizationFallbackInstanceType]);
  });

  it("accepts explicit supported and metal types", () => {
    expect(
      awsLaunchCandidates(nestedLease({ serverType: "c8i.2xlarge", serverTypeExplicit: true })),
    ).toEqual(["c8i.2xlarge"]);
    expect(
      awsLaunchCandidates(nestedLease({ serverType: "c7i.metal-24xl", serverTypeExplicit: true })),
    ).toEqual(["c7i.metal-24xl"]);
  });

  it.each([
    [{ serverType: "c7a.2xlarge", serverTypeExplicit: true }, /c7a\.2xlarge is not supported/],
    [{ class: "t3.large" }, /class=t3\.large is not supported/],
    [{ architecture: "arm64" }, /requires architecture=amd64/],
    [{ target: "windows" }, /requires target=linux/],
    [{ provider: "hetzner" }, /requires provider=aws/],
    [{ awsInstanceTypes: ["c8i.large", "t3.small"] }, /t3\.small not supported/],
    [{ awsNestedVirtualization: "yes" }, /must be a boolean/],
  ] as const)("rejects %o", (input, message) => {
    expect(() => nestedLease(input as Partial<LeaseRequest>)).toThrow(
      InvalidAWSNestedVirtualizationError,
    );
    expect(() => nestedLease(input as Partial<LeaseRequest>)).toThrow(message);
  });

  it("leaves Windows WSL2 and default Linux leases unchanged", () => {
    const wsl2 = nestedLease({ target: "windows", windowsMode: "wsl2", class: "standard" });
    expect(wsl2.awsNestedVirtualization).toBeUndefined();
    expect(awsLaunchCandidates(wsl2)[0]).toBe("m8i.large");

    const linux = leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 test" });
    expect(linux.awsNestedVirtualization).toBeUndefined();
    expect(linux.serverType).toBe("c7a.48xlarge");
    expect(
      leaseConfig({
        provider: "aws",
        sshPublicKey: "ssh-ed25519 test",
        awsNestedVirtualization: false,
      }).awsNestedVirtualization,
    ).toBeUndefined();
  });

  it("sets CpuOptions for nested Linux launches except bare metal", () => {
    const nested: Record<string, string> = {};
    applyAWSRunInstanceTargetOptions(nested, {
      target: "linux",
      windowsMode: "normal",
      serverType: "c8i.2xlarge",
      awsNestedVirtualization: true,
    });
    expect(nested["CpuOptions.NestedVirtualization"]).toBe("enabled");

    const metal: Record<string, string> = {};
    applyAWSRunInstanceTargetOptions(metal, {
      target: "linux",
      windowsMode: "normal",
      serverType: "c7i.metal-24xl",
      awsNestedVirtualization: true,
    });
    expect(metal).not.toHaveProperty("CpuOptions.NestedVirtualization");

    const plain: Record<string, string> = {};
    applyAWSRunInstanceTargetOptions(plain, {
      target: "linux",
      windowsMode: "normal",
      serverType: "c8i.2xlarge",
    });
    expect(plain).not.toHaveProperty("CpuOptions.NestedVirtualization");
  });
});
