import { afterEach, describe, expect, it, vi } from "vitest";

import {
  EC2SpotClient,
  addRunInstancesTagSpecifications,
  applyAWSRunInstanceTargetOptions,
  awsAvailabilityZoneForRegion,
  awsInstanceTypeVCPUs,
  awsHostIDsFromSet,
  awsLaunchCandidates,
  awsMacHostIDFromDescribeHosts,
  awsMacHostOfferingsFromDescribeInstanceTypeOfferings,
  awsProvisioningErrorCategory,
  awsQuotaCodeForMarket,
  awsQuotaPreflightAttempt,
  awsRegionCandidates,
  awsReleaseHostsResult,
  crabboxSSHIngressRules,
  createSecurityGroupParams,
  isAWSInstanceCleanedAfterReadinessFailure,
  isAWSInvalidHostIDError,
  isAWSInstanceNotFoundError,
  isRetryableAWSProvisioningError,
  staleCrabboxSSHIngressRules,
} from "../src/aws";
import {
  awsMacOSInstanceTypeCandidates,
  awsPromotedAMIConfigKey,
  leaseConfig,
} from "../src/config";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

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

  it("extracts only Crabbox-owned SSH ingress rules from AWS security groups", () => {
    expect(
      crabboxSSHIngressRules(
        {
          ipPermissions: {
            item: [
              {
                fromPort: 2222,
                ipProtocol: "tcp",
                ipRanges: {
                  item: [
                    { cidrIp: "203.0.113.10/32", description: "Crabbox SSH" },
                    { cidrIp: "198.51.100.20/32", description: "other" },
                  ],
                },
                ipv6Ranges: {
                  item: { cidrIpv6: "2001:db8::1/128", description: "Crabbox SSH" },
                },
                toPort: 2222,
              },
              {
                fromPort: 443,
                ipProtocol: "tcp",
                ipRanges: { item: { cidrIp: "203.0.113.30/32", description: "Crabbox SSH" } },
                toPort: 443,
              },
            ],
          },
        },
        ["2222"],
      ),
    ).toEqual([
      { cidr: "203.0.113.10/32", family: "ipv4", port: "2222" },
      { cidr: "2001:db8::1/128", family: "ipv6", port: "2222" },
    ]);
  });

  it("selects stale Crabbox SSH ingress rules before adding the current source CIDR", () => {
    expect(
      staleCrabboxSSHIngressRules(
        {
          ipPermissions: {
            item: {
              fromPort: 2222,
              ipProtocol: "tcp",
              ipRanges: {
                item: [
                  { cidrIp: "203.0.113.10/32", description: "Crabbox SSH" },
                  { cidrIp: "198.51.100.20/32", description: "Crabbox SSH" },
                ],
              },
              toPort: 2222,
            },
          },
        },
        ["2222"],
        ["198.51.100.20/32"],
      ),
    ).toEqual([{ cidr: "203.0.113.10/32", family: "ipv4", port: "2222" }]);
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

  it("classifies stale AWS instance ID errors", () => {
    expect(
      isAWSInstanceNotFoundError("InvalidInstanceID.NotFound: The instance ID does not exist"),
    ).toBe(true);
    expect(
      isAWSInstanceNotFoundError(
        "<Error><Code>InvalidInstanceID.NotFound</Code><Message>missing</Message></Error>",
      ),
    ).toBe(true);
    expect(isAWSInstanceNotFoundError("UnauthorizedOperation: nope")).toBe(false);
  });

  it("classifies stale EC2 Mac host ID errors", () => {
    expect(
      isAWSInvalidHostIDError(
        "<Code>InvalidHostID.NotFound</Code><Message>The specified Dedicated host IDs do not exist.</Message>",
      ),
    ).toBe(true);
    expect(isAWSInvalidHostIDError("InvalidInstanceID.NotFound")).toBe(false);
  });

  it("selects an available EC2 Mac Dedicated Host from DescribeHosts", () => {
    expect(
      awsMacHostIDFromDescribeHosts({
        hostSet: {
          item: [
            { hostId: "h-stale", hostState: "available" },
            { hostId: "h-usable", hostState: "available" },
          ],
        },
      }),
    ).toBe("h-stale");
    expect(
      awsMacHostIDFromDescribeHosts(
        {
          hostSet: {
            item: [
              { hostId: "h-stale", hostState: "available" },
              { hostId: "h-usable", hostState: "available" },
            ],
          },
        },
        "h-stale",
      ),
    ).toBe("h-usable");
  });

  it("parses EC2 host id sets from AllocateHosts responses", () => {
    expect(awsHostIDsFromSet({ item: "h-000000000001" })).toEqual(["h-000000000001"]);
    expect(
      awsHostIDsFromSet({
        item: [{ hostId: "h-000000000001" }, { hostId: "h-000000000002" }],
      }),
    ).toEqual(["h-000000000001", "h-000000000002"]);
  });

  it("parses EC2 ReleaseHosts successful and unsuccessful sets", () => {
    expect(
      awsReleaseHostsResult({
        successful: { item: "h-000000000001" },
        unsuccessful: "",
      }),
    ).toEqual({
      successful: ["h-000000000001"],
      unsuccessful: [],
    });
    expect(
      awsReleaseHostsResult({
        successful: "",
        unsuccessful: {
          item: {
            resourceId: "h-000000000001",
            error: {
              code: "Client.InvalidHost.Occupied",
              message: "Dedicated host cannot be released as it is occupied",
            },
          },
        },
      }),
    ).toEqual({
      successful: [],
      unsuccessful: [
        {
          resourceID: "h-000000000001",
          code: "Client.InvalidHost.Occupied",
          message: "Dedicated host cannot be released as it is occupied",
        },
      ],
    });
  });

  it("parses EC2 Mac instance type offerings by availability zone", () => {
    expect(
      awsMacHostOfferingsFromDescribeInstanceTypeOfferings(
        {
          instanceTypeOfferingSet: {
            item: [
              {
                instanceType: "mac2.metal",
                location: "eu-west-1b",
                locationType: "availability-zone",
              },
              {
                instanceType: "mac2.metal",
                location: "eu-west-1a",
                locationType: "availability-zone",
              },
              {
                instanceType: "m7i.large",
                location: "eu-west-1a",
                locationType: "availability-zone",
              },
              {
                instanceType: "mac2.metal",
                location: "us-east-1a",
                locationType: "availability-zone",
              },
            ],
          },
        },
        "eu-west-1",
      ),
    ).toEqual([
      { region: "eu-west-1", availabilityZone: "eu-west-1a", instanceType: "mac2.metal" },
      { region: "eu-west-1", availabilityZone: "eu-west-1b", instanceType: "mac2.metal" },
    ]);
  });

  it("treats missing stale AWS instance cleanup as cleaned", () => {
    expect(
      isAWSInstanceCleanedAfterReadinessFailure(
        "InvalidInstanceID.NotFound: instance disappeared",
        "InvalidInstanceID.NotFound: instance disappeared",
      ),
    ).toBe(true);
    expect(
      isAWSInstanceCleanedAfterReadinessFailure(
        "InvalidInstanceID.NotFound: instance disappeared",
        "",
      ),
    ).toBe(true);
    expect(
      isAWSInstanceCleanedAfterReadinessFailure(
        "timed out waiting for AWS instance public IP",
        "UnauthorizedOperation: denied",
      ),
    ).toBe(false);
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

  it("builds ordered AWS region and availability-zone candidates", () => {
    expect(
      awsRegionCandidates(
        { awsRegion: "eu-west-1", capacityRegions: ["us-east-1", "eu-west-1"] },
        { CRABBOX_AWS_REGION: "eu-central-1", CRABBOX_CAPACITY_REGIONS: "us-west-2, us-east-1" },
        "eu-west-2",
      ),
    ).toEqual(["eu-west-2", "eu-west-1", "eu-central-1", "us-west-2", "us-east-1"]);
    expect(
      awsAvailabilityZoneForRegion(
        { capacityAvailabilityZones: ["us-east-1a", "eu-west-1b"] },
        { CRABBOX_CAPACITY_AVAILABILITY_ZONES: "eu-west-2a,eu-west-1c" },
        "eu-west-1",
      ),
    ).toBe("eu-west-1b");
  });

  it("treats macOS host and image misses as retryable regional AWS failures", () => {
    const hostMiss =
      "no available EC2 Mac Dedicated Host found in eu-west-1 for mac2.metal; allocate a host or set CRABBOX_HOST_ID";
    const imageMiss =
      "no AWS AMI found in eu-west-2 for name=amzn-ec2-macos-14.*-arm64 architecture=arm64_mac";

    expect(awsProvisioningErrorCategory(hostMiss)).toBe("capacity");
    expect(isRetryableAWSProvisioningError(hostMiss)).toBe(true);
    expect(awsProvisioningErrorCategory(imageMiss)).toBe("region");
    expect(isRetryableAWSProvisioningError(imageMiss)).toBe(true);
  });

  it("resolves macOS AMIs per fallback instance type", async () => {
    const imageQueries: string[] = [];
    const hostTypes: string[] = [];
    const runImages: string[] = [];
    const runTypes: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (new URL(request.url).hostname.startsWith("servicequotas.")) {
          return new Response(JSON.stringify({ Quota: { Value: 999 } }), {
            headers: { "content-type": "application/json" },
          });
        }
        const body = await request.clone().text();
        const params = new URLSearchParams(body);
        const action = params.get("Action") ?? "";
        if (action === "DescribeKeyPairs") {
          return ec2XMLResponse("<DescribeKeyPairsResponse />");
        }
        if (action === "DescribeImages") {
          const architecture = params.get("Filter.1.Value.1") ?? "";
          const name = params.get("Filter.2.Value.1") ?? "";
          imageQueries.push(`${name}:${architecture}`);
          const imageID =
            architecture === "x86_64_mac"
              ? "ami-x86-mac"
              : name === "amzn-ec2-macos-15.*-arm64"
                ? "ami-m4-mac"
                : "ami-arm-mac";
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse>
  <imagesSet>
    <item>
      <imageId>${imageID}</imageId>
      <name>macos</name>
      <imageState>available</imageState>
      <creationDate>2026-05-01T00:00:00.000Z</creationDate>
    </item>
  </imagesSet>
</DescribeImagesResponse>`);
        }
        if (action === "DescribeHosts") {
          const serverType = params.get("Filter.1.Value.1") ?? "";
          hostTypes.push(serverType);
          if (serverType === "mac1.metal") {
            return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeHostsResponse>
  <hostSet>
    <item>
      <hostId>h-mac1</hostId>
      <hostState>available</hostState>
    </item>
  </hostSet>
</DescribeHostsResponse>`);
          }
          return ec2XMLResponse("<DescribeHostsResponse><hostSet /></DescribeHostsResponse>");
        }
        if (action === "RunInstances") {
          runImages.push(params.get("ImageId") ?? "");
          runTypes.push(params.get("InstanceType") ?? "");
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse>
  <instancesSet>
    <item>
      <instanceId>i-mac1</instanceId>
      <instanceType>mac1.metal</instanceType>
      <placement><hostId>h-mac1</hostId></placement>
      <ipAddress>203.0.113.44</ipAddress>
      <instanceState><name>pending</name></instanceState>
      <tagSet><item><key>Name</key><value>crabbox-violet-prawn</value></item></tagSet>
    </item>
  </instancesSet>
</RunInstancesResponse>`);
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
        CRABBOX_AWS_SSH_CIDRS: "203.0.113.7/32",
      } as never,
      "eu-west-1",
    );
    const result = await client.createServerWithFallback(
      leaseConfig({
        provider: "aws",
        target: "macos",
        capacity: { market: "on-demand" },
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_abcdef123456",
      "violet-prawn",
      "alice@example.com",
    );

    expect(imageQueries).toEqual([
      "amzn-ec2-macos-14.*-arm64:arm64_mac",
      "amzn-ec2-macos-15.*-arm64:arm64_mac",
      "amzn-ec2-macos-14.*:x86_64_mac",
    ]);
    expect(hostTypes).toEqual(awsMacOSInstanceTypeCandidates);
    expect(runTypes).toEqual(["mac1.metal"]);
    expect(runImages).toEqual(["ami-x86-mac"]);
    expect(result.serverType).toBe("mac1.metal");
    expect(result.server.hostID).toBe("h-mac1");
  });

  it("does not reuse a pinned macOS AMI after changing fallback host families", async () => {
    const runImages: string[] = [];
    const imageQueries: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (new URL(request.url).hostname.startsWith("servicequotas.")) {
          return new Response(JSON.stringify({ Quota: { Value: 999 } }), {
            headers: { "content-type": "application/json" },
          });
        }
        const body = await request.clone().text();
        const params = new URLSearchParams(body);
        const action = params.get("Action") ?? "";
        if (action === "DescribeKeyPairs") {
          return ec2XMLResponse("<DescribeKeyPairsResponse />");
        }
        if (action === "DescribeImages") {
          const architecture = params.get("Filter.1.Value.1") ?? "";
          const name = params.get("Filter.2.Value.1") ?? "";
          imageQueries.push(`${name}:${architecture}`);
          const imageID = architecture === "x86_64_mac" ? "ami-x86-mac" : "ami-arm-mac";
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse>
  <imagesSet>
    <item>
      <imageId>${imageID}</imageId>
      <name>macos</name>
      <imageState>available</imageState>
      <creationDate>2026-05-01T00:00:00.000Z</creationDate>
    </item>
  </imagesSet>
</DescribeImagesResponse>`);
        }
        if (action === "DescribeHosts") {
          const serverType = params.get("Filter.1.Value.1") ?? "";
          if (serverType === "mac1.metal") {
            return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeHostsResponse>
  <hostSet>
    <item>
      <hostId>h-mac1</hostId>
      <hostState>available</hostState>
    </item>
  </hostSet>
</DescribeHostsResponse>`);
          }
          return ec2XMLResponse("<DescribeHostsResponse><hostSet /></DescribeHostsResponse>");
        }
        if (action === "RunInstances") {
          runImages.push(params.get("ImageId") ?? "");
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse>
  <instancesSet>
    <item>
      <instanceId>i-mac1</instanceId>
      <instanceType>mac1.metal</instanceType>
      <placement><hostId>h-mac1</hostId></placement>
      <ipAddress>203.0.113.44</ipAddress>
      <instanceState><name>pending</name></instanceState>
    </item>
  </instancesSet>
</RunInstancesResponse>`);
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
      } as never,
      "eu-west-1",
    );
    const result = await client.createServerWithFallback(
      leaseConfig({
        provider: "aws",
        target: "macos",
        capacity: { market: "on-demand" },
        awsAMI: "ami-promoted-mac2",
        serverType: "mac2.metal",
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_abcdef123456",
      "violet-prawn",
      "alice@example.com",
    );

    expect(imageQueries).toContain("amzn-ec2-macos-14.*:x86_64_mac");
    expect(runImages).toEqual(["ami-x86-mac"]);
    expect(result.serverType).toBe("mac1.metal");
  });

  it("uses scoped promoted macOS AMIs for fallback host families", async () => {
    const runImages: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (new URL(request.url).hostname.startsWith("servicequotas.")) {
          return new Response(JSON.stringify({ Quota: { Value: 999 } }), {
            headers: { "content-type": "application/json" },
          });
        }
        const body = await request.clone().text();
        const params = new URLSearchParams(body);
        const action = params.get("Action") ?? "";
        if (action === "DescribeKeyPairs") {
          return ec2XMLResponse("<DescribeKeyPairsResponse />");
        }
        if (action === "DescribeImages") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse>
  <imagesSet>
    <item>
      <imageId>ami-stock-mac</imageId>
      <name>macos</name>
      <imageState>available</imageState>
      <creationDate>2026-05-01T00:00:00.000Z</creationDate>
    </item>
  </imagesSet>
</DescribeImagesResponse>`);
        }
        if (action === "DescribeHosts") {
          const serverType = params.get("Filter.1.Value.1") ?? "";
          if (serverType === "mac1.metal") {
            return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeHostsResponse>
  <hostSet>
    <item>
      <hostId>h-mac1</hostId>
      <hostState>available</hostState>
    </item>
  </hostSet>
</DescribeHostsResponse>`);
          }
          return ec2XMLResponse("<DescribeHostsResponse><hostSet /></DescribeHostsResponse>");
        }
        if (action === "RunInstances") {
          runImages.push(params.get("ImageId") ?? "");
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse>
  <instancesSet>
    <item>
      <instanceId>i-mac1</instanceId>
      <instanceType>mac1.metal</instanceType>
      <placement><hostId>h-mac1</hostId></placement>
      <ipAddress>203.0.113.44</ipAddress>
      <instanceState><name>pending</name></instanceState>
    </item>
  </instancesSet>
</RunInstancesResponse>`);
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
      } as never,
      "eu-west-1",
    );
    const config = leaseConfig({
      provider: "aws",
      target: "macos",
      capacity: { market: "on-demand" },
      serverType: "mac2.metal",
      sshPublicKey: "ssh-ed25519 test",
    });
    config.awsPromotedAMIs[awsPromotedAMIConfigKey("eu-west-1", "mac1.metal")] =
      "ami-promoted-mac1";
    const result = await client.createServerWithFallback(
      config,
      "cbx_abcdef123456",
      "violet-prawn",
      "alice@example.com",
    );

    expect(runImages).toEqual(["ami-promoted-mac1"]);
    expect(result.serverType).toBe("mac1.metal");
  });

  it("prefers region-scoped promoted macOS AMIs over a pinned AMI", async () => {
    const runImages: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (new URL(request.url).hostname.startsWith("servicequotas.")) {
          return new Response(JSON.stringify({ Quota: { Value: 999 } }), {
            headers: { "content-type": "application/json" },
          });
        }
        const params = new URLSearchParams(await request.clone().text());
        const action = params.get("Action") ?? "";
        if (action === "DescribeKeyPairs") {
          return ec2XMLResponse("<DescribeKeyPairsResponse />");
        }
        if (action === "DescribeHosts") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeHostsResponse>
  <hostSet>
    <item>
      <hostId>h-mac2</hostId>
      <hostState>available</hostState>
    </item>
  </hostSet>
</DescribeHostsResponse>`);
        }
        if (action === "RunInstances") {
          runImages.push(params.get("ImageId") ?? "");
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse>
  <instancesSet>
    <item>
      <instanceId>i-mac2</instanceId>
      <instanceType>mac2.metal</instanceType>
      <placement><hostId>h-mac2</hostId></placement>
      <ipAddress>203.0.113.44</ipAddress>
      <instanceState><name>pending</name></instanceState>
    </item>
  </instancesSet>
</RunInstancesResponse>`);
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
      } as never,
      "us-west-2",
    );
    const config = leaseConfig({
      provider: "aws",
      target: "macos",
      capacity: { market: "on-demand" },
      awsAMI: "ami-eu-mac2",
      serverType: "mac2.metal",
      serverTypeExplicit: true,
      sshPublicKey: "ssh-ed25519 test",
    });
    config.awsPromotedAMIs[awsPromotedAMIConfigKey("us-west-2", "mac2.metal")] = "ami-us-mac2";

    await client.createServerWithFallback(
      config,
      "cbx_abcdef123456",
      "violet-prawn",
      "alice@example.com",
    );

    expect(runImages).toEqual(["ami-us-mac2"]);
  });

  it("discovers brokered macOS hosts in the configured subnet availability zone", async () => {
    const describeSubnetParams: Record<string, string>[] = [];
    const describeHostFilters: Record<string, string>[] = [];
    const runParams: Record<string, string>[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (new URL(request.url).hostname.startsWith("servicequotas.")) {
          return new Response(JSON.stringify({ Quota: { Value: 999 } }), {
            headers: { "content-type": "application/json" },
          });
        }
        const body = await request.clone().text();
        const params = Object.fromEntries(new URLSearchParams(body));
        const action = params.Action ?? "";
        if (action === "DescribeKeyPairs") {
          return ec2XMLResponse("<DescribeKeyPairsResponse />");
        }
        if (action === "DescribeSubnets") {
          describeSubnetParams.push(params);
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeSubnetsResponse>
  <subnetSet>
    <item>
      <subnetId>subnet-123</subnetId>
      <vpcId>vpc-123</vpcId>
      <availabilityZone>eu-west-1b</availabilityZone>
    </item>
  </subnetSet>
</DescribeSubnetsResponse>`);
        }
        if (action === "DescribeImages") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse>
  <imagesSet>
    <item>
      <imageId>ami-arm-mac</imageId>
      <name>macos</name>
      <imageState>available</imageState>
      <creationDate>2026-05-01T00:00:00.000Z</creationDate>
    </item>
  </imagesSet>
</DescribeImagesResponse>`);
        }
        if (action === "DescribeHosts") {
          describeHostFilters.push(params);
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeHostsResponse>
  <hostSet>
    <item>
      <hostId>h-mac2-b</hostId>
      <hostState>available</hostState>
      <availabilityZone>eu-west-1b</availabilityZone>
    </item>
  </hostSet>
</DescribeHostsResponse>`);
        }
        if (action === "RunInstances") {
          runParams.push(params);
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse>
  <instancesSet>
    <item>
      <instanceId>i-mac2</instanceId>
      <instanceType>mac2.metal</instanceType>
      <placement><hostId>h-mac2-b</hostId></placement>
      <ipAddress>203.0.113.45</ipAddress>
      <instanceState><name>pending</name></instanceState>
    </item>
  </instancesSet>
</RunInstancesResponse>`);
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      {
        AWS_ACCESS_KEY_ID: "test",
        AWS_SECRET_ACCESS_KEY: "secret",
        CRABBOX_AWS_SECURITY_GROUP_ID: "sg-123",
        CRABBOX_AWS_SUBNET_ID: "subnet-123",
      } as never,
      "eu-west-1",
    );
    const result = await client.createServerWithFallback(
      leaseConfig({
        provider: "aws",
        target: "macos",
        capacity: { market: "on-demand" },
        serverType: "mac2.metal",
        serverTypeExplicit: true,
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_abcdef123456",
      "violet-prawn",
      "alice@example.com",
    );

    expect(describeSubnetParams[0]).toMatchObject({ "SubnetId.1": "subnet-123" });
    expect(describeHostFilters[0]).toMatchObject({
      "Filter.1.Name": "instance-type",
      "Filter.1.Value.1": "mac2.metal",
      "Filter.2.Name": "state",
      "Filter.2.Value.1": "available",
      "Filter.3.Name": "tag:crabbox",
      "Filter.3.Value.1": "true",
      "Filter.4.Name": "availability-zone",
      "Filter.4.Value.1": "eu-west-1b",
    });
    expect(runParams[0]).toMatchObject({
      "NetworkInterface.1.SubnetId": "subnet-123",
      "Placement.HostId": "h-mac2-b",
      "Placement.Tenancy": "host",
    });
    expect(result.server.hostID).toBe("h-mac2-b");
  });

  it("waits for transient AMIs before launching from EBS snapshots", async () => {
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      "eu-west-1",
    ) as unknown as EC2SpotClient & {
      ensureSSHKey: () => Promise<void>;
      registerSnapshotImage: () => Promise<string>;
      waitForImageAvailable: (imageID: string) => Promise<string>;
      ensureSecurityGroup: () => Promise<string>;
      ec2: (action: string, params?: Record<string, string>) => Promise<unknown>;
      createServer: (...args: unknown[]) => Promise<{
        provider: "aws";
        id: number;
        cloudID: string;
        name: string;
        status: string;
        serverType: string;
        host: string;
        labels: Record<string, string>;
      }>;
    };
    const calls: string[] = [];
    client.ensureSSHKey = async () => {
      calls.push("ensure-key");
    };
    client.registerSnapshotImage = async () => {
      calls.push("register-snapshot");
      return "ami-transient";
    };
    client.waitForImageAvailable = async (imageID: string) => {
      calls.push(`wait:${imageID}`);
      return imageID;
    };
    client.ensureSecurityGroup = async () => {
      calls.push("security-group");
      return "sg-123";
    };
    client.ec2 = async (action, params) => {
      calls.push(`${action}:${params?.ImageId ?? ""}`);
      return {};
    };
    client.createServer = async (...args: unknown[]) => {
      calls.push(`launch:${String(args[4])}`);
      return {
        provider: "aws",
        id: 1,
        cloudID: "i-123",
        name: "crabbox-blue-lobster",
        status: "running",
        serverType: "t3.small",
        host: "192.0.2.10",
        labels: {},
      };
    };

    await client.createServerWithFallback(
      leaseConfig({
        provider: "aws",
        serverType: "t3.small",
        serverTypeExplicit: true,
        awsSnapshot: "snap-000000000001",
        sshPublicKey: "ssh-ed25519 test",
        capacity: { market: "on-demand" },
      }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    expect(calls).toEqual([
      "ensure-key",
      "register-snapshot",
      "wait:ami-transient",
      "security-group",
      "launch:ami-transient",
      "DeregisterImage:ami-transient",
    ]);
  });

  it("deregisters transient AMIs when snapshot image waiting fails", async () => {
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      "eu-west-1",
    ) as unknown as EC2SpotClient & {
      ensureSSHKey: () => Promise<void>;
      registerSnapshotImage: () => Promise<string>;
      waitForImageAvailable: (imageID: string) => Promise<string>;
      ec2: (action: string, params?: Record<string, string>) => Promise<unknown>;
    };
    const calls: string[] = [];
    client.ensureSSHKey = async () => {
      calls.push("ensure-key");
    };
    client.registerSnapshotImage = async () => {
      calls.push("register-snapshot");
      return "ami-transient";
    };
    client.waitForImageAvailable = async (imageID: string) => {
      calls.push(`wait:${imageID}`);
      throw new Error("timed out waiting");
    };
    client.ec2 = async (action, params) => {
      calls.push(`${action}:${params?.ImageId ?? ""}`);
      return {};
    };

    await expect(
      client.createServerWithFallback(
        leaseConfig({
          provider: "aws",
          serverType: "t3.small",
          serverTypeExplicit: true,
          awsSnapshot: "snap-000000000001",
          sshPublicKey: "ssh-ed25519 test",
          capacity: { market: "on-demand" },
        }),
        "cbx_123456789abc",
        "blue-lobster",
        "owner",
      ),
    ).rejects.toThrow("timed out waiting");

    expect(calls).toEqual([
      "ensure-key",
      "register-snapshot",
      "wait:ami-transient",
      "DeregisterImage:ami-transient",
    ]);
  });

  it("registers snapshot AMIs with stored boot metadata", async () => {
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      "eu-west-1",
    ) as unknown as EC2SpotClient & {
      registerSnapshotImage: (snapshotID: string, leaseID: string) => Promise<string>;
      ec2: (action: string, params?: Record<string, string>) => Promise<Record<string, unknown>>;
    };
    const registerParams: Record<string, string>[] = [];
    client.ec2 = async (action, params = {}) => {
      if (action === "DescribeSnapshots") {
        return {
          snapshotSet: {
            item: {
              snapshotId: "snap-000000000001",
              tagSet: {
                item: [
                  { key: "crabbox_root_device_name", value: "/dev/xvda" },
                  { key: "crabbox_architecture", value: "arm64" },
                ],
              },
            },
          },
        };
      }
      if (action === "RegisterImage") {
        registerParams.push(params);
        return { imageId: "ami-transient" };
      }
      throw new Error(`unexpected ${action}`);
    };

    const imageID = await client.registerSnapshotImage("snap-000000000001", "cbx_123456789abc");

    expect(imageID).toBe("ami-transient");
    expect(registerParams[0]).toMatchObject({
      Architecture: "arm64",
      RootDeviceName: "/dev/xvda",
      "BlockDeviceMapping.1.DeviceName": "/dev/xvda",
    });
  });

  it("stores source boot metadata on EBS snapshot checkpoints", async () => {
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      "eu-west-1",
    ) as unknown as EC2SpotClient & {
      createDiskSnapshot: (instanceID: string, name: string) => Promise<unknown>;
      ec2: (action: string, params?: Record<string, string>) => Promise<Record<string, unknown>>;
    };
    const snapshotParams: Record<string, string>[] = [];
    client.ec2 = async (action, params = {}) => {
      if (action === "DescribeInstances") {
        return {
          reservationSet: {
            item: {
              instancesSet: {
                item: {
                  rootDeviceName: "/dev/xvda",
                  architecture: "arm64",
                  blockDeviceMapping: {
                    item: {
                      deviceName: "/dev/xvda",
                      ebs: { volumeId: "vol-000000000001" },
                    },
                  },
                },
              },
            },
          },
        };
      }
      if (action === "CreateSnapshot") {
        snapshotParams.push(params);
        return { snapshotId: "snap-000000000001", status: "pending" };
      }
      throw new Error(`unexpected ${action}`);
    };

    await client.createDiskSnapshot("i-000000000001", "checkpoint");

    expect(snapshotParams[0]).toMatchObject({
      VolumeId: "vol-000000000001",
      "TagSpecification.1.Tag.4.Key": "crabbox_root_device_name",
      "TagSpecification.1.Tag.4.Value": "/dev/xvda",
      "TagSpecification.1.Tag.5.Key": "crabbox_architecture",
      "TagSpecification.1.Tag.5.Value": "arm64",
    });
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
      region: "eu-west-1",
      serverType: "c7a.48xlarge",
      market: "on-demand",
      category: "quota",
      message: "quota L-1216C47A in eu-west-1 is 32 vCPUs; c7a.48xlarge needs 192 vCPUs",
    });
    expect(awsQuotaPreflightAttempt("t3.small", "on-demand", "eu-west-1", 32)).toBeUndefined();
    expect(awsQuotaPreflightAttempt("c7gn.metal", "spot", "eu-west-1", 32)).toBeUndefined();
  });

  it("retries snapshot deletion after deregistering an image", async () => {
    vi.useFakeTimers();
    const actions: string[] = [];
    let deleteSnapshotCalls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const body = await request.clone().text();
        const action = new URLSearchParams(body).get("Action") ?? "";
        actions.push(action);
        if (action === "DescribeImages") {
          return ec2XMLResponse(`<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse>
  <imagesSet>
    <item>
      <imageId>ami-000000000001</imageId>
      <name>checkpoint</name>
      <imageState>available</imageState>
      <blockDeviceMapping>
        <item><ebs><snapshotId>snap-000000000001</snapshotId></ebs></item>
      </blockDeviceMapping>
    </item>
  </imagesSet>
</DescribeImagesResponse>`);
        }
        if (action === "DeregisterImage") {
          return ec2XMLResponse("<DeregisterImageResponse />");
        }
        if (action === "DeleteSnapshot") {
          deleteSnapshotCalls++;
          if (deleteSnapshotCalls === 1) {
            return ec2XMLResponse(
              "<Response><Errors><Error><Code>InvalidSnapshot.InUse</Code><Message>snapshot is currently in use</Message></Error></Errors></Response>",
              400,
            );
          }
          return ec2XMLResponse("<DeleteSnapshotResponse />");
        }
        return ec2XMLResponse(
          `<Response><Errors><Error><Code>Unexpected</Code><Message>${action}</Message></Error></Errors></Response>`,
          500,
        );
      }),
    );

    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as never,
      "eu-west-1",
    );
    const deletion = client.deleteImage("ami-000000000001");
    await vi.waitFor(() => expect(deleteSnapshotCalls).toBe(1));
    expect(actions).toEqual(["DescribeImages", "DeregisterImage", "DeleteSnapshot"]);
    await vi.advanceTimersByTimeAsync(1_000);
    await deletion;

    expect(actions).toEqual([
      "DescribeImages",
      "DeregisterImage",
      "DeleteSnapshot",
      "DeleteSnapshot",
    ]);
  });
});

function ec2XMLResponse(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "content-type": "application/xml" } });
}
