import { describe, expect, it } from "vitest";

import {
  addRunInstancesTagSpecifications,
  applyAWSRunInstanceTargetOptions,
  awsInstanceTypeVCPUs,
  awsLaunchCandidates,
  awsProvisioningErrorCategory,
  awsQuotaCodeForMarket,
  awsQuotaPreflightAttempt,
  createSecurityGroupParams,
} from "../src/aws";

describe("aws provider", () => {
  it("uses the EC2 query parameter names for security group creation", () => {
    const params = createSecurityGroupParams("crabbox-runners", "vpc-123");

    expect(params).toMatchObject({
      GroupDescription: "Crabbox ephemeral test runners",
      GroupName: "crabbox-runners",
      VpcId: "vpc-123",
      "TagSpecification.1.ResourceType": "security-group",
      "TagSpecification.1.Tag.1.Key": "Name",
      "TagSpecification.1.Tag.1.Value": "crabbox-runners",
      "TagSpecification.1.Tag.2.Key": "crabbox",
      "TagSpecification.1.Tag.2.Value": "true",
      "TagSpecification.1.Tag.3.Key": "created_by",
      "TagSpecification.1.Tag.3.Value": "crabbox",
    });
    expect(params).not.toHaveProperty("Description");
  });

  it("does not tag Spot request resources for On-Demand launches", () => {
    const spotParams: Record<string, string> = {};
    addRunInstancesTagSpecifications(spotParams, { crabbox: "true", Name: "crabbox-cbx" }, "spot");
    expect(spotParams["TagSpecification.3.ResourceType"]).toBe("spot-instances-request");

    const onDemandParams: Record<string, string> = {};
    addRunInstancesTagSpecifications(
      onDemandParams,
      { crabbox: "true", Name: "crabbox-cbx" },
      "on-demand",
    );
    expect(onDemandParams["TagSpecification.1.ResourceType"]).toBe("instance");
    expect(onDemandParams["TagSpecification.2.ResourceType"]).toBe("volume");
    expect(onDemandParams).not.toHaveProperty("TagSpecification.3.ResourceType");
    expect(onDemandParams).not.toHaveProperty("TagSpecification.3.Tag.1.Key");
  });

  it("enables nested virtualization only for Windows WSL2 launches", () => {
    const wsl2Params: Record<string, string> = {};
    applyAWSRunInstanceTargetOptions(wsl2Params, { target: "windows", windowsMode: "wsl2" });
    expect(wsl2Params["CpuOptions.NestedVirtualization"]).toBe("enabled");

    const nativeParams: Record<string, string> = {};
    applyAWSRunInstanceTargetOptions(nativeParams, { target: "windows", windowsMode: "normal" });
    expect(nativeParams).not.toHaveProperty("CpuOptions.NestedVirtualization");
  });

  it("classifies account policy launch failures as fallback candidates", () => {
    expect(
      awsProvisioningErrorCategory(
        "aws RunInstances: http 400: InvalidParameterCombination: The instance type c7a.48xlarge is not eligible for Free Tier",
      ),
    ).toBe("policy");
    expect(awsProvisioningErrorCategory("InsufficientInstanceCapacity: nope")).toBe("capacity");
    expect(awsProvisioningErrorCategory("VcpuLimitExceeded: nope")).toBe("quota");
  });

  it("adds a small policy fallback for class requests but not exact types", () => {
    expect(
      awsLaunchCandidates({
        class: "beast",
        target: "linux",
        windowsMode: "normal",
        serverType: "c7a.48xlarge",
        serverTypeExplicit: false,
      }),
    ).toContain("t3.small");
    expect(
      awsLaunchCandidates({
        class: "beast",
        target: "linux",
        windowsMode: "normal",
        serverType: "t3.small",
        serverTypeExplicit: true,
      }),
    ).toEqual(["t3.small"]);
    expect(
      awsLaunchCandidates({
        class: "standard",
        target: "windows",
        windowsMode: "wsl2",
        serverType: "m8i.large",
        serverTypeExplicit: false,
      }),
    ).not.toContain("t3.large");
  });

  it("maps AWS instance types to vCPU quota units", () => {
    expect(awsInstanceTypeVCPUs("c7a.48xlarge")).toBe(192);
    expect(awsInstanceTypeVCPUs("c7a.xlarge")).toBe(4);
    expect(awsInstanceTypeVCPUs("t3.small")).toBe(2);
    expect(awsInstanceTypeVCPUs("c7gn.metal")).toBeUndefined();
  });

  it("builds quota preflight attempts when applied quota is too low", () => {
    expect(awsQuotaCodeForMarket("spot")).toBe("L-34B43A08");
    expect(awsQuotaCodeForMarket("on-demand")).toBe("L-1216C47A");
    expect(awsQuotaPreflightAttempt("c7a.48xlarge", "on-demand", "eu-west-1", 32)).toEqual({
      serverType: "c7a.48xlarge",
      market: "on-demand",
      category: "quota",
      message: "quota L-1216C47A in eu-west-1 is 32 vCPUs; c7a.48xlarge needs 192 vCPUs",
    });
    expect(awsQuotaPreflightAttempt("t3.small", "on-demand", "eu-west-1", 32)).toBeUndefined();
    expect(awsQuotaPreflightAttempt("c7gn.metal", "spot", "eu-west-1", 32)).toBeUndefined();
  });
});
