import { afterEach, describe, expect, it, vi } from "vitest";

import {
  EC2SpotClient,
  awsOrphanSweepCredentialsConfigured,
  awsPrivateWorkspaceConfig,
  awsRunInstancesParams,
  type AWSPrivateWorkspaceConfig,
} from "../src/aws";
import { leaseConfig, type LeaseConfig } from "../src/config";
import { AWSProvider } from "../src/fleet";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";

const expectedAccountID = "123456789012";
const region = "us-west-2";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("private AWS workspaces", () => {
  it("signs every request with freshly resolved task credentials", async () => {
    let generation = 0;
    const credentials = vi.fn<
      () => Promise<{ accessKeyId: string; secretAccessKey: string; sessionToken: string }>
    >(async () => {
      generation += 1;
      return {
        accessKeyId: `TASKKEY${generation}`,
        secretAccessKey: `task-secret-${generation}`,
        sessionToken: `task-token-${generation}`,
      };
    });
    const authorizations: string[] = [];
    const sessionTokens: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        authorizations.push(request.headers.get("authorization") ?? "");
        sessionTokens.push(request.headers.get("x-amz-security-token") ?? "");
        return ec2XMLResponse(
          "<DescribeInstancesResponse><reservationSet /></DescribeInstancesResponse>",
        );
      }),
    );
    const client = new EC2SpotClient({ awsCredentialProvider: credentials } as Env, region);

    await client.listCrabboxServers();
    await client.listCrabboxServers();

    expect(credentials).toHaveBeenCalledTimes(2);
    expect(authorizations[0]).toContain("Credential=TASKKEY1/");
    expect(authorizations[1]).toContain("Credential=TASKKEY2/");
    expect(sessionTokens).toEqual(["task-token-1", "task-token-2"]);
  });

  it("retries an expected identity check after a transient failure", async () => {
    const client = new EC2SpotClient(expectedEnv(), region);
    const identity = vi
      .spyOn(client, "identity")
      .mockRejectedValueOnce(new Error("temporary STS failure"))
      .mockResolvedValue({
        account: expectedAccountID,
        arn: `arn:aws:sts::${expectedAccountID}:assumed-role/crabbox-controller/task`,
        userId: "task-id",
        region,
      });

    await expect(client.verifiedIdentity()).rejects.toThrow("temporary STS failure");
    await expect(client.verifiedIdentity()).resolves.toMatchObject({ account: expectedAccountID });
    expect(identity).toHaveBeenCalledTimes(2);
  });

  it("does not enable orphan sweeps from an unproven default credential chain", () => {
    expect(
      awsOrphanSweepCredentialsConfigured({ awsCredentialProvider: testAWSCredentialProvider }),
    ).toBe(false);
    expect(
      awsOrphanSweepCredentialsConfigured({
        awsCredentialProvider: testAWSCredentialProvider,
        CRABBOX_AWS_CREDENTIAL_SOURCE: "default-chain",
      }),
    ).toBe(true);
    expect(
      awsOrphanSweepCredentialsConfigured({
        awsCredentialProvider: testAWSCredentialProvider,
        CRABBOX_WORKSPACE_PROVIDER: "aws",
      }),
    ).toBe(true);
    for (const value of ["1", "true", "yes", "on"]) {
      expect(
        awsOrphanSweepCredentialsConfigured({
          awsCredentialProvider: testAWSCredentialProvider,
          CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: value,
        }),
      ).toBe(true);
    }
  });

  it("does not use spot history as an on-demand cost estimate", async () => {
    const fetchImpl = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", fetchImpl);
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
      {} as never,
    );

    await expect(
      provider.hourlyPriceUSD("t3a.small", privateLeaseConfig()),
    ).resolves.toBeUndefined();
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("keeps existing private workspace evidence observable after policy removal", () => {
    const env = privateWorkspaceEnv();
    const provider = new AWSProvider(env, region, {} as never);
    const lease = {
      network: { awsPrivate: true },
      awsSSMCommandID: "command-private123",
      awsSSMCommandStatus: "Success",
      awsSSMLogGroup: "/crabbox/private-workspaces",
    } as LeaseRecord;
    delete env.CRABBOX_WORKSPACE_AWS_PRIVATE;

    expect(() => provider.workspaceCapability(lease)).toThrow(
      "private AWS workspace recovery policy is unavailable",
    );
    expect(
      provider.workspaceCapability(lease, "observe")?.bootstrapEvidence(lease, "ready"),
    ).toMatchObject({
      transport: "ssm",
      status: "Success",
      commandId: "command-private123",
      logGroup: "/crabbox/private-workspaces",
    });
  });

  it("encodes a private launch without SSH or public networking", async () => {
    const config = privateLeaseConfig();
    const params = await awsRunInstancesParams({
      config,
      leaseID: "cbx_private000001",
      imageID: "ami-0123456789abcdef0",
      securityGroupID: "sg-workspace123",
      rootGB: 20,
      instanceProfile: "crabbox-private-workspace",
      subnetID: "subnet-private123",
      labels: {
        Name: "crabbox-private-workspace",
        access_mode: "ssm",
        crabbox: "true",
        crabbox_workspace: "true",
        created_by: "crabbox",
        lease: "cbx_private000001",
      },
    });

    expect(params["KeyName"]).toBeUndefined();
    expect(params["NetworkInterface.1.AssociatePublicIpAddress"]).toBe("false");
    expect(params["NetworkInterface.1.SubnetId"]).toBe("subnet-private123");
    expect(params["NetworkInterface.1.SecurityGroupId.1"]).toBe("sg-workspace123");
    expect(params["NetworkInterface.1.GroupSet.1"]).toBeUndefined();
    expect(params["IamInstanceProfile.Name"]).toBe("crabbox-private-workspace");
    expect(params["MetadataOptions.HttpEndpoint"]).toBe("enabled");
    expect(params["MetadataOptions.HttpTokens"]).toBe("required");
    expect(params["MetadataOptions.HttpPutResponseHopLimit"]).toBe("1");
    expect(params["MetadataOptions.InstanceMetadataTags"]).toBe("disabled");
    expect(params["BlockDeviceMapping.1.Ebs.VolumeType"]).toBe("gp3");
    expect(params["BlockDeviceMapping.1.Ebs.VolumeSize"]).toBe("20");
    expect(params["BlockDeviceMapping.1.Ebs.Encrypted"]).toBe("true");
    expect(params["BlockDeviceMapping.1.Ebs.DeleteOnTermination"]).toBe("true");
    expect(runInstanceTags(params, "instance")).toEqual({
      Name: "crabbox-private-workspace",
      access_mode: "ssm",
      crabbox: "true",
      crabbox_workspace: "true",
      created_by: "crabbox",
      lease: "cbx_private000001",
    });
    expect(runInstanceTags(params, "volume")).toEqual(runInstanceTags(params, "instance"));
  });

  it("validates the configured private security group without mutating ingress", async () => {
    const actions: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const params = new URLSearchParams(await request.clone().text());
        actions.push(params.get("Action") ?? "");
        return ec2XMLResponse(`<DescribeSecurityGroupsResponse><securityGroupInfo><item>
          <groupId>sg-workspace123</groupId><vpcId>vpc-private123</vpcId>
          <ipPermissions /><ipPermissionsEgress><item><ipProtocol>tcp</ipProtocol>
            <fromPort>443</fromPort><toPort>443</toPort>
            <ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges>
          </item></ipPermissionsEgress>
        </item></securityGroupInfo></DescribeSecurityGroupsResponse>`);
      }),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await expect(
      client.refreshSSHIngress(privateLeaseConfig(), { allowEmpty: true }),
    ).resolves.toBeUndefined();
    expect(actions).toEqual(["DescribeSecurityGroups"]);
  });

  it("fails closed on an unexpected AWS account before an EC2 request", async () => {
    const hosts: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        hosts.push(new URL(request.url).hostname);
        return stsIdentityResponse("999999999999");
      }),
    );
    const client = new EC2SpotClient(expectedEnv(), region);

    await expect(client.listCrabboxServers()).rejects.toThrow(
      `AWS account mismatch: expected ${expectedAccountID}, authenticated 999999999999`,
    );
    expect(hosts).toEqual([`sts.${region}.amazonaws.com`]);
  });

  it("fails closed when credentials are not for the exact task role", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => stsIdentityResponse(expectedAccountID)),
    );
    const client = new EC2SpotClient(
      {
        ...expectedEnv(),
        CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME: "expected-coordinator-task-role",
      },
      region,
    );

    await expect(client.listCrabboxServers()).rejects.toThrow("AWS task role mismatch");
  });

  it("preflights the exact allowlist, private network, security groups, and permissions", async () => {
    const policy = privatePolicy();
    const config = privateLeaseConfig();
    const ec2Actions: string[] = [];
    let runInstances: URLSearchParams | undefined;
    let instanceTypes: URLSearchParams | undefined;
    let ssmTarget = "";
    let ssmBody: Record<string, unknown> | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const host = new URL(request.url).hostname;
        if (host.startsWith("sts.")) return stsIdentityResponse(expectedAccountID);
        if (host.startsWith("ssm.")) {
          ssmTarget = request.headers.get("x-amz-target") ?? "";
          ssmBody = JSON.parse(await request.clone().text()) as Record<string, unknown>;
          return jsonResponse({ InstanceInformationList: [] });
        }
        const params = new URLSearchParams(await request.clone().text());
        const action = params.get("Action") ?? "";
        ec2Actions.push(action);
        if (action === "DescribeInstanceTypes") {
          instanceTypes = params;
          return ec2XMLResponse(`<DescribeInstanceTypesResponse><instanceTypeSet><item>
            <instanceType>t3a.small</instanceType>
            <processorInfo><supportedArchitectures><item>x86_64</item></supportedArchitectures></processorInfo>
            <vCpuInfo><defaultVCpus>2</defaultVCpus></vCpuInfo>
            <memoryInfo><sizeInMiB>2048</sizeInMiB></memoryInfo>
          </item></instanceTypeSet></DescribeInstanceTypesResponse>`);
        }
        if (action === "DescribeSubnets") {
          return ec2XMLResponse(`<DescribeSubnetsResponse><subnetSet><item>
            <subnetId>${policy.subnetID}</subnetId><state>available</state>
            <vpcId>vpc-private123</vpcId><mapPublicIpOnLaunch>false</mapPublicIpOnLaunch>
          </item></subnetSet></DescribeSubnetsResponse>`);
        }
        if (action === "DescribeRouteTables") {
          return ec2XMLResponse(`<DescribeRouteTablesResponse><routeTableSet><item>
            <routeTableId>rtb-private123</routeTableId><routeSet><item>
              <destinationCidrBlock>0.0.0.0/0</destinationCidrBlock>
              <natGatewayId>nat-private123</natGatewayId><state>active</state>
            </item></routeSet>
          </item></routeTableSet></DescribeRouteTablesResponse>`);
        }
        if (action === "DescribeSecurityGroups") {
          return ec2XMLResponse(`<DescribeSecurityGroupsResponse><securityGroupInfo>
            <item><groupId>${policy.securityGroupID}</groupId><vpcId>vpc-private123</vpcId>
              <ipPermissions /><ipPermissionsEgress><item><ipProtocol>tcp</ipProtocol>
                <fromPort>443</fromPort><toPort>443</toPort>
                <ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges>
              </item></ipPermissionsEgress>
            </item>
            <item><groupId>${policy.controllerSecurityGroupID}</groupId>
              <vpcId>vpc-private123</vpcId><ipPermissions /><ipPermissionsEgress />
            </item>
          </securityGroupInfo></DescribeSecurityGroupsResponse>`);
        }
        if (action === "RunInstances") {
          runInstances = params;
          return dryRunResponse(action);
        }
        throw new Error(`unexpected EC2 action ${action}`);
      }),
    );
    const client = new EC2SpotClient(expectedEnv(), region);

    await expect(client.privateWorkspacePreflight(config, policy)).resolves.toMatchObject({
      account: expectedAccountID,
      region,
    });

    expect(ec2Actions).toEqual([
      "DescribeInstanceTypes",
      "DescribeSubnets",
      "DescribeRouteTables",
      "DescribeSecurityGroups",
      "RunInstances",
    ]);
    expect(instanceTypes?.get("InstanceType.1")).toBe("t3a.small");
    expect(instanceTypes?.get("InstanceType.2")).toBeNull();
    expect(ssmTarget).toBe("AmazonSSM.DescribeInstanceInformation");
    expect(ssmBody).toEqual({ MaxResults: 5 });
    expect(runInstances?.get("DryRun")).toBe("true");
    expect(runInstances?.get("InstanceType")).toBe("t3a.small");
    expect(runInstances?.get("KeyName")).toBeNull();
    expect(runInstances?.get("NetworkInterface.1.AssociatePublicIpAddress")).toBe("false");
    expect(runInstances?.get("BlockDeviceMapping.1.Ebs.VolumeSize")).toBe("20");
  });

  it("rejects an allowlisted instance that exceeds the configured size cap", async () => {
    const policy = privatePolicy();
    const actions: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const host = new URL(request.url).hostname;
        if (host.startsWith("sts.")) return stsIdentityResponse(expectedAccountID);
        const params = new URLSearchParams(await request.clone().text());
        actions.push(params.get("Action") ?? "");
        return ec2XMLResponse(`<DescribeInstanceTypesResponse><instanceTypeSet><item>
          <instanceType>t3a.small</instanceType>
          <processorInfo><supportedArchitectures><item>x86_64</item></supportedArchitectures></processorInfo>
          <vCpuInfo><defaultVCpus>4</defaultVCpus></vCpuInfo>
          <memoryInfo><sizeInMiB>2048</sizeInMiB></memoryInfo>
        </item></instanceTypeSet></DescribeInstanceTypesResponse>`);
      }),
    );
    const client = new EC2SpotClient(expectedEnv(), region);

    await expect(client.privateWorkspacePreflight(privateLeaseConfig(), policy)).rejects.toThrow(
      "AWS private workspace instance type t3a.small exceeds 2 vCPUs",
    );
    expect(actions).toEqual(["DescribeInstanceTypes"]);
  });

  it("waits for SSM registration and returns command evidence", async () => {
    const targets: string[] = [];
    const bodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const target = request.headers.get("x-amz-target") ?? "";
        targets.push(target);
        bodies.push(JSON.parse(await request.clone().text()) as Record<string, unknown>);
        if (target === "AmazonSSM.DescribeInstanceInformation") {
          return jsonResponse({
            InstanceInformationList: [{ InstanceId: "i-private123", PingStatus: "Online" }],
          });
        }
        if (target === "AmazonSSM.SendCommand") {
          return jsonResponse({ Command: { CommandId: "command-private123" } });
        }
        if (target === "AmazonSSM.GetCommandInvocation") {
          return jsonResponse({ Status: "Success", StatusDetails: "Success" });
        }
        throw new Error(`unexpected SSM target ${target}`);
      }),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await client.waitForSSMOnline("i-private123");
    await expect(
      client.runSSMBootstrap(
        "i-private123",
        "cbx_private000001",
        "systemctl start crabbox-workspace",
        "/crabbox/private-workspaces",
      ),
    ).resolves.toEqual({ commandID: "command-private123", status: "Success" });

    expect(targets).toEqual([
      "AmazonSSM.DescribeInstanceInformation",
      "AmazonSSM.SendCommand",
      "AmazonSSM.GetCommandInvocation",
    ]);
    expect(bodies[1]).toMatchObject({
      CloudWatchOutputConfig: {
        CloudWatchLogGroupName: "/crabbox/private-workspaces",
        CloudWatchOutputEnabled: true,
      },
      DocumentName: "AWS-RunShellScript",
      InstanceIds: ["i-private123"],
      Parameters: {
        commands: [
          "/bin/bash -lc 'set -euo pipefail\ncloud-init status --wait\n/usr/local/bin/crabbox-ready\nsystemctl start crabbox-workspace'",
        ],
        executionTimeout: ["900"],
      },
    });
    expect(bodies[2]).toEqual({
      CommandId: "command-private123",
      InstanceId: "i-private123",
    });
  });

  it("keeps polling while a new instance is not registered with SSM", async () => {
    vi.useFakeTimers();
    let attempts = 0;
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    ) as unknown as {
      waitForSSMOnline(instanceID: string): Promise<void>;
      ssm(action: string, body: Record<string, unknown>): Promise<Record<string, unknown>>;
    };
    client.ssm = async (action, body) => {
      expect(action).toBe("DescribeInstanceInformation");
      expect(body).toEqual({
        Filters: [{ Key: "InstanceIds", Values: ["i-private123"] }],
        MaxResults: 5,
      });
      attempts += 1;
      if (attempts === 1) {
        throw new Error(
          "aws DescribeInstanceInformation: http 400: InvalidInstanceId: The managed node is not registered yet",
        );
      }
      return {
        InstanceInformationList: [{ InstanceId: "i-private123", PingStatus: "Online" }],
      };
    };

    const readiness = client.waitForSSMOnline("i-private123");
    await vi.advanceTimersByTimeAsync(5_000);

    await expect(readiness).resolves.toBeUndefined();
    expect(attempts).toBe(2);
  });

  it("uses a private instance address only when private readiness is explicit", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        ec2XMLResponse(`<DescribeInstancesResponse><reservationSet><item><instancesSet><item>
          <instanceId>i-private123</instanceId><privateIpAddress>10.0.2.15</privateIpAddress>
          <instanceState><name>running</name></instanceState>
          <metadataOptions><httpEndpoint>enabled</httpEndpoint><httpTokens>required</httpTokens>
            <httpPutResponseHopLimit>1</httpPutResponseHopLimit>
            <instanceMetadataTags>disabled</instanceMetadataTags></metadataOptions>
          <networkInterfaceSet><item><ipv6AddressesSet><item>
            <ipv6Address>2001:db8::1</ipv6Address>
          </item></ipv6AddressesSet></item></networkInterfaceSet>
          <rootDeviceName>/dev/sda1</rootDeviceName><blockDeviceMapping><item>
            <deviceName>/dev/sda1</deviceName><ebs><volumeId>vol-private123</volumeId>
              <deleteOnTermination>true</deleteOnTermination></ebs>
          </item></blockDeviceMapping>
        </item></instancesSet></item></reservationSet></DescribeInstancesResponse>`),
      ),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await expect(client.getServer("i-private123")).resolves.toMatchObject({
      host: "",
      privateHost: "10.0.2.15",
      awsMetadataHttpEndpoint: "enabled",
      awsMetadataHttpTokens: "required",
      awsMetadataHttpPutResponseHopLimit: 1,
      awsMetadataInstanceTags: "disabled",
      awsIPv6Addresses: ["2001:db8::1"],
      awsRootVolumeID: "vol-private123",
      awsRootDeleteOnTermination: true,
    });
    await expect(client.waitForServerIP("i-private123", true)).resolves.toMatchObject({
      host: "10.0.2.15",
      privateHost: "10.0.2.15",
    });
  });

  it("resumes a recovered instance with the exact IAM instance-profile ARN", async () => {
    const targets: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        if (new URL(request.url).hostname.startsWith("ec2.")) {
          return ec2XMLResponse(`<DescribeVolumesResponse><volumeSet><item>
            <volumeId>vol-private123</volumeId><volumeType>gp3</volumeType><size>20</size>
            <encrypted>true</encrypted><tagSet>
              <item><key>lease</key><value>cbx_private000001</value></item>
              <item><key>crabbox</key><value>true</value></item>
              <item><key>created_by</key><value>crabbox</value></item>
              <item><key>crabbox_workspace</key><value>true</value></item>
              <item><key>access_mode</key><value>ssm</value></item>
            </tagSet>
          </item></volumeSet></DescribeVolumesResponse>`);
        }
        const target = request.headers.get("x-amz-target") ?? "";
        targets.push(target);
        if (target === "AmazonSSM.DescribeInstanceInformation") {
          return jsonResponse({
            InstanceInformationList: [{ InstanceId: "i-private123", PingStatus: "Online" }],
          });
        }
        if (target === "AmazonSSM.SendCommand") {
          return jsonResponse({ Command: { CommandId: "command-private123" } });
        }
        if (target === "AmazonSSM.GetCommandInvocation") {
          return jsonResponse({ Status: "Success", StatusDetails: "Success" });
        }
        throw new Error(`unexpected target ${target}`);
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
      {} as never,
    );
    const server: ProviderMachine = {
      provider: "aws",
      id: 0,
      cloudID: "i-private123",
      name: "private-workspace",
      status: "running",
      serverType: "t3a.small",
      host: "",
      region,
      awsSubnetID: "subnet-private123",
      awsSecurityGroupIDs: ["sg-workspace123"],
      awsInstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/crabbox-private-workspace",
      awsMetadataHttpEndpoint: "enabled",
      awsMetadataHttpTokens: "required",
      awsMetadataHttpPutResponseHopLimit: 1,
      awsMetadataInstanceTags: "disabled",
      awsRootVolumeID: "vol-private123",
      awsRootDeleteOnTermination: true,
      labels: {},
    };

    await expect(
      provider.resumeRecoveredServer(
        privateLeaseConfig(),
        { id: "cbx_private000001" } as LeaseRecord,
        server,
      ),
    ).resolves.toMatchObject({
      cloudID: "i-private123",
      awsSSMCommandID: "command-private123",
      awsSSMCommandStatus: "Success",
    });
    expect(targets).toEqual([
      "AmazonSSM.DescribeInstanceInformation",
      "AmazonSSM.SendCommand",
      "AmazonSSM.GetCommandInvocation",
    ]);
  });

  it("refuses and retires a recovered instance outside the private policy", async () => {
    const actions: string[] = [];
    const ssmTargets: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        if (new URL(request.url).hostname.startsWith("ssm.")) {
          ssmTargets.push(request.headers.get("x-amz-target") ?? "");
          return jsonResponse({});
        }
        const params = new URLSearchParams(await request.clone().text());
        const action = params.get("Action") ?? "";
        actions.push(action);
        if (action === "TerminateInstances") {
          return ec2XMLResponse(`<TerminateInstancesResponse><instancesSet><item>
            <instanceId>i-private123</instanceId>
          </item></instancesSet></TerminateInstancesResponse>`);
        }
        return awsErrorResponse(
          "InvalidInstanceID.NotFound",
          "The instance ID 'i-private123' does not exist",
        );
      }),
    );
    const provider = new AWSProvider(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
      {} as never,
    );
    const server: ProviderMachine = {
      provider: "aws",
      id: 0,
      cloudID: "i-private123",
      name: "private-workspace",
      status: "running",
      serverType: "t3a.small",
      host: "",
      region,
      awsSubnetID: "subnet-private123",
      awsSecurityGroupIDs: ["sg-workspace123"],
      awsInstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/crabbox-private-workspace",
      awsMetadataHttpEndpoint: "enabled",
      awsMetadataHttpTokens: "required",
      awsMetadataHttpPutResponseHopLimit: 1,
      awsMetadataInstanceTags: "disabled",
      awsIPv6Addresses: ["2001:db8::1"],
      labels: {},
    };

    await expect(
      provider.resumeRecoveredServer(
        privateLeaseConfig(),
        { id: "cbx_private000001" } as LeaseRecord,
        server,
      ),
    ).rejects.toThrow("recovered AWS private workspace is outside deployment policy");
    expect(actions).toEqual(["TerminateInstances", "DescribeInstances"]);
    expect(ssmTargets).toEqual([]);
  });

  it("confirms termination when the instance disappears", async () => {
    const actions: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const params = new URLSearchParams(await request.clone().text());
        const action = params.get("Action") ?? "";
        actions.push(action);
        if (action === "TerminateInstances") {
          return ec2XMLResponse(`<TerminateInstancesResponse><instancesSet><item>
            <instanceId>i-private123</instanceId>
          </item></instancesSet></TerminateInstancesResponse>`);
        }
        return awsErrorResponse(
          "InvalidInstanceID.NotFound",
          "The instance ID 'i-private123' does not exist",
        );
      }),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await expect(client.terminateServerAndWait("i-private123")).resolves.toBeUndefined();
    expect(actions).toEqual(["TerminateInstances", "DescribeInstances"]);
  });

  it("checks termination once more after the final backoff", async () => {
    let describeCalls = 0;
    const delays: number[] = [];
    vi.stubGlobal("setTimeout", ((callback: () => void, delay?: number) => {
      delays.push(delay ?? 0);
      queueMicrotask(callback);
      return 0;
    }) as typeof setTimeout);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = requestFrom(input, init);
        const action = new URLSearchParams(await request.clone().text()).get("Action") ?? "";
        if (action === "TerminateInstances") {
          return ec2XMLResponse(`<TerminateInstancesResponse><instancesSet><item>
            <instanceId>i-private123</instanceId>
          </item></instancesSet></TerminateInstancesResponse>`);
        }
        describeCalls += 1;
        const state = describeCalls === 7 ? "terminated" : "shutting-down";
        return ec2XMLResponse(`<DescribeInstancesResponse><reservationSet><item>
          <instancesSet><item><instanceId>i-private123</instanceId>
            <instanceState><name>${state}</name></instanceState>
          </item></instancesSet>
        </item></reservationSet></DescribeInstancesResponse>`);
      }),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await expect(client.terminateServerAndWait("i-private123")).resolves.toBeUndefined();
    expect(describeCalls).toBe(7);
    expect(delays).toEqual([1_000, 2_000, 4_000, 8_000, 15_000, 30_000]);
  });

  it("treats an already absent instance as an idempotent cleanup success", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        awsErrorResponse(
          "InvalidInstanceID.NotFound",
          "The instance ID 'i-private123' does not exist",
        ),
      ),
    );
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      region,
    );

    await expect(client.terminateServerAndWait("i-private123")).resolves.toBeUndefined();
  });

  it("requires the full fail-closed private deployment policy", () => {
    const config = awsPrivateWorkspaceConfig({
      CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
      CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES: "t3a.small",
      CRABBOX_WORKSPACE_AWS_SUBNET_ID: "subnet-private123",
      CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID: "sg-workspace123",
      CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID: "sg-controller123",
      CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE: "crabbox-private-workspace",
      CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP: "/crabbox/private-workspaces",
      CRABBOX_AWS_EXPECTED_ACCOUNT_ID: expectedAccountID,
      CRABBOX_AWS_EXPECTED_REGION: region,
    });

    expect(config).toEqual(privatePolicy());
    expect(() =>
      awsPrivateWorkspaceConfig({
        CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
        CRABBOX_AWS_EXPECTED_ACCOUNT_ID: expectedAccountID,
        CRABBOX_AWS_EXPECTED_REGION: region,
      }),
    ).toThrow("CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES is required");
    expect(() =>
      awsPrivateWorkspaceConfig({
        CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
        CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES: "t3a.small",
        CRABBOX_WORKSPACE_AWS_ROOT_GB: "101",
        CRABBOX_WORKSPACE_AWS_SUBNET_ID: "subnet-private123",
        CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID: "sg-workspace123",
        CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID: "sg-controller123",
        CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE: "crabbox-private-workspace",
        CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP: "/crabbox/private-workspaces",
        CRABBOX_AWS_EXPECTED_ACCOUNT_ID: expectedAccountID,
        CRABBOX_AWS_EXPECTED_REGION: region,
      }),
    ).toThrow("CRABBOX_WORKSPACE_AWS_ROOT_GB must be an integer from 8 through 100");
  });
});

function privateLeaseConfig(): LeaseConfig {
  return leaseConfig({
    provider: "aws",
    target: "linux",
    class: "standard",
    serverType: "t3a.small",
    serverTypeExplicit: true,
    awsRegion: region,
    awsAMI: "ami-0123456789abcdef0",
    awsPrivate: true,
    awsRequireSSM: true,
    awsInstanceTypes: ["t3a.small"],
    awsSubnetID: "subnet-private123",
    awsSGID: "sg-workspace123",
    awsProfile: "crabbox-private-workspace",
    awsRootGB: 20,
    awsSSMBootstrapCommand: "systemctl start crabbox-workspace",
    awsSSMLogGroup: "/crabbox/private-workspaces",
    capacity: { market: "on-demand", fallback: "none", regions: [region], hints: false },
    providerKey: "crabbox-workspace-private",
  });
}

function privatePolicy(): AWSPrivateWorkspaceConfig {
  return {
    accountID: expectedAccountID,
    region,
    instanceTypes: ["t3a.small"],
    maxVCPUs: 2,
    maxMemoryMiB: 4096,
    rootGB: 20,
    subnetID: "subnet-private123",
    securityGroupID: "sg-workspace123",
    controllerSecurityGroupID: "sg-controller123",
    instanceProfile: "crabbox-private-workspace",
    market: "on-demand",
    ssmLogGroup: "/crabbox/private-workspaces",
  };
}

function expectedEnv(): Env {
  return {
    AWS_ACCESS_KEY_ID: "test",
    AWS_SECRET_ACCESS_KEY: "secret",
    CRABBOX_AWS_EXPECTED_ACCOUNT_ID: expectedAccountID,
    CRABBOX_AWS_EXPECTED_REGION: region,
  };
}

function privateWorkspaceEnv(): Env {
  return {
    ...expectedEnv(),
    CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
    CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES: "t3a.small",
    CRABBOX_WORKSPACE_AWS_SUBNET_ID: "subnet-private123",
    CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID: "sg-workspace123",
    CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID: "sg-controller123",
    CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE: "crabbox-private-workspace",
    CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP: "/crabbox/private-workspaces",
  };
}

async function testAWSCredentialProvider() {
  return {
    accessKeyId: "task-access-key",
    secretAccessKey: "task-secret-key",
  };
}

function requestFrom(input: RequestInfo | URL, init?: RequestInit): Request {
  return input instanceof Request ? input : new Request(input, init);
}

function runInstanceTags(params: Record<string, string>, resourceType: string) {
  const spec = [1, 2, 3].find(
    (index) => params[`TagSpecification.${index}.ResourceType`] === resourceType,
  );
  const tags: Record<string, string> = {};
  if (!spec) return tags;
  for (let index = 1; ; index += 1) {
    const key = params[`TagSpecification.${spec}.Tag.${index}.Key`];
    if (!key) return tags;
    tags[key] = params[`TagSpecification.${spec}.Tag.${index}.Value`] ?? "";
  }
}

function stsIdentityResponse(accountID: string): Response {
  return ec2XMLResponse(`<GetCallerIdentityResponse><GetCallerIdentityResult>
    <Account>${accountID}</Account>
    <Arn>arn:aws:sts::${accountID}:assumed-role/crabbox-controller/task</Arn>
    <UserId>task-id</UserId>
  </GetCallerIdentityResult></GetCallerIdentityResponse>`);
}

function dryRunResponse(action: string): Response {
  return awsErrorResponse("DryRunOperation", `Request would have succeeded: ${action}`, 412);
}

function awsErrorResponse(code: string, message: string, status = 400): Response {
  return ec2XMLResponse(
    `<Response><Errors><Error><Code>${code}</Code><Message>${message}</Message></Error></Errors></Response>`,
    status,
  );
}

function ec2XMLResponse(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "content-type": "application/xml" } });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/x-amz-json-1.1" },
  });
}
