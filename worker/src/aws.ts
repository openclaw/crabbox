import { AwsClient } from "aws4fetch";
import { XMLParser } from "fast-xml-parser";

import { awsRunInstancesUserData } from "./bootstrap";
import {
  awsPromotedAMIConfigKey,
  awsInstanceTypeCandidatesForTargetClass,
  sshPorts,
  validCIDRs,
  type LeaseConfig,
} from "./config";
import { osImageSpec } from "./os-image";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type {
  Env,
  ProviderFastSnapshotRestore,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
} from "./types";

const awsUbuntuOwner = "099720109477";
const ec2Version = "2016-11-15";
const stsVersion = "2011-06-15";
const awsSpotQuotaCode = "L-34B43A08";
const awsOnDemandQuotaCode = "L-1216C47A";
const awsSSHIngressDescription = "Crabbox SSH";
const awsMacHostQuotaSpecs: Record<string, { quotaCode: string; quotaName: string }> = {
  mac1: { quotaCode: "L-A8448DC5", quotaName: "Running Dedicated mac1 Hosts" },
  mac2: { quotaCode: "L-5D8DADF5", quotaName: "Running Dedicated mac2 Hosts" },
  "mac2-m1ultra": {
    quotaCode: "L-AE4D744C",
    quotaName: "Running Dedicated mac2-m1ultra Hosts",
  },
  "mac2-m2": { quotaCode: "L-B90B5B66", quotaName: "Running Dedicated mac2-m2 Hosts" },
  "mac2-m2pro": {
    quotaCode: "L-14F120D1",
    quotaName: "Running Dedicated mac2-m2pro Hosts",
  },
  "mac-m3ultra": {
    quotaCode: "L-7108A7B5",
    quotaName: "Running Dedicated mac-m3ultra Hosts",
  },
  "mac-m4": { quotaCode: "L-2CBA8B92", quotaName: "Running Dedicated mac-m4 Hosts" },
  "mac-m4max": {
    quotaCode: "L-D82CB68A",
    quotaName: "Running Dedicated mac-m4max Hosts",
  },
  "mac-m4pro": {
    quotaCode: "L-6919FC30",
    quotaName: "Running Dedicated mac-m4pro Hosts",
  },
};
const snapshotDeleteBackoffMs = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000];

export interface AWSMacHost {
  id: string;
  state: string;
  region: string;
  availabilityZone: string;
  instanceType: string;
  autoPlacement: string;
  availableCapacity?: number;
  availableVCpus?: number;
  allocationTime?: string;
  releaseTime?: string;
  tags: Record<string, string>;
}

export interface AWSMacHostOffering {
  region: string;
  availabilityZone: string;
  instanceType: string;
}

export interface AWSMacHostAllocationDryRun {
  region: string;
  availabilityZone: string;
  instanceType: string;
  ok: boolean;
  message: string;
}

export interface AWSReleaseHostFailure {
  resourceID: string;
  code: string;
  message: string;
}

export interface AWSReleaseHostsResult {
  successful: string[];
  unsuccessful: AWSReleaseHostFailure[];
}

export interface AWSServiceQuota {
  serviceCode?: string;
  quotaCode: string;
  quotaName: string;
  value?: number;
  adjustable?: boolean;
  globalQuota?: boolean;
  unit?: string;
}

export interface AWSCapacityReadinessCheck {
  status: "ok" | "skip" | "warning";
  check: "capacity";
  message: string;
  details: Record<string, string>;
}

export interface AWSIdentity {
  account: string;
  arn: string;
  userId: string;
  region: string;
  policyTarget?: AWSPolicyTarget;
}

export interface AWSPolicyTarget {
  type: "role" | "user";
  name: string;
  source: "iam-role" | "iam-user" | "assumed-role";
}

export function awsPolicyTargetFromArn(arn: string): AWSPolicyTarget | undefined {
  const parts = arn.split(":");
  if (parts.length < 6 || parts[0] !== "arn" || !parts[1]) {
    return undefined;
  }
  const service = parts[2];
  const resource = parts.slice(5).join(":");
  const segments = resource.split("/").filter(Boolean);
  const leafName = segments[segments.length - 1];
  if (service === "iam" && segments[0] === "user" && leafName) {
    return { type: "user", name: leafName, source: "iam-user" };
  }
  if (service === "iam" && segments[0] === "role" && leafName) {
    return { type: "role", name: leafName, source: "iam-role" };
  }
  const assumedRoleName = segments[1];
  if (service === "sts" && segments[0] === "assumed-role" && assumedRoleName) {
    return { type: "role", name: assumedRoleName, source: "assumed-role" };
  }
  return undefined;
}

export function createSecurityGroupParams(name: string, vpcID: string): Record<string, string> {
  return {
    GroupDescription: "Crabbox ephemeral test runners",
    GroupName: name,
    VpcId: vpcID,
    "TagSpecification.1.ResourceType": "security-group",
    "TagSpecification.1.Tag.1.Key": "Name",
    "TagSpecification.1.Tag.1.Value": name,
    "TagSpecification.1.Tag.2.Key": "crabbox",
    "TagSpecification.1.Tag.2.Value": "true",
    "TagSpecification.1.Tag.3.Key": "created_by",
    "TagSpecification.1.Tag.3.Value": "crabbox",
  };
}

type SSHIngressRule = {
  cidr: string;
  family: "ipv4" | "ipv6";
  port: string;
};

type DescribedSSHIngressRule = SSHIngressRule & { description: string };

interface AWSIngressOptions {
  reconcile?: "authoritative" | "additive";
}

const sshIngressRangeFamilies = [
  {
    cidrField: "cidrIp",
    descriptionParam: "IpPermissions.1.IpRanges.1.Description",
    family: "ipv4",
    param: "IpPermissions.1.IpRanges.1.CidrIp",
    rangesField: "ipRanges",
  },
  {
    cidrField: "cidrIpv6",
    descriptionParam: "IpPermissions.1.Ipv6Ranges.1.Description",
    family: "ipv6",
    param: "IpPermissions.1.Ipv6Ranges.1.CidrIpv6",
    rangesField: "ipv6Ranges",
  },
] as const;

export class EC2SpotClient {
  private readonly aws: AwsClient;
  private readonly serviceQuotas: AwsClient;
  private readonly stsClient: AwsClient;
  private readonly endpoint: string;
  private readonly serviceQuotasEndpoint: string;
  private readonly stsEndpoint: string;
  private readonly region: string;
  private readonly parser = new XMLParser({ ignoreAttributes: false });

  constructor(
    private readonly env: Env,
    region: string,
  ) {
    const accessKeyId = env.AWS_ACCESS_KEY_ID;
    const secretAccessKey = env.AWS_SECRET_ACCESS_KEY;
    if (!accessKeyId || !secretAccessKey) {
      throw new Error("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY secrets are required");
    }
    this.region = region || env.CRABBOX_AWS_REGION || "eu-west-1";
    this.endpoint = `https://ec2.${this.region}.amazonaws.com/`;
    this.serviceQuotasEndpoint = `https://servicequotas.${this.region}.amazonaws.com/`;
    this.stsEndpoint = `https://sts.${this.region}.amazonaws.com/`;
    const clientOptions: ConstructorParameters<typeof AwsClient>[0] = {
      accessKeyId,
      secretAccessKey,
      service: "ec2",
      region: this.region,
    };
    if (env.AWS_SESSION_TOKEN) {
      clientOptions.sessionToken = env.AWS_SESSION_TOKEN;
    }
    this.aws = new AwsClient(clientOptions);
    this.serviceQuotas = new AwsClient({ ...clientOptions, service: "servicequotas" });
    this.stsClient = new AwsClient({ ...clientOptions, service: "sts" });
  }

  async capacityReadinessChecks(config: LeaseConfig): Promise<AWSCapacityReadinessCheck[]> {
    if (config.target === "macos") {
      return [];
    }
    return await Promise.all(
      awsCapacityReadinessMarkets(config).map(async (market) => {
        const code = awsQuotaCodeForMarket(market);
        const quota = await this.appliedServiceQuota(code);
        return awsCapacityReadinessCheckForQuota(config, market, this.region, quota);
      }),
    );
  }

  async identity(): Promise<AWSIdentity> {
    const root = await this.sts("GetCallerIdentity", {});
    const result = record(root["GetCallerIdentityResult"] ?? root);
    const arn = asString(result["Arn"]);
    const identity: AWSIdentity = {
      account: asString(result["Account"]),
      arn,
      userId: asString(result["UserId"]),
      region: this.region,
    };
    const policyTarget = awsPolicyTargetFromArn(arn);
    if (policyTarget) {
      identity.policyTarget = policyTarget;
    }
    return identity;
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const root = await this.ec2("DescribeInstances", {
      "Filter.1.Name": "tag:crabbox",
      "Filter.1.Value.1": "true",
      "Filter.2.Name": "instance-state-name",
      "Filter.2.Value.1": "pending",
      "Filter.2.Value.2": "running",
      "Filter.2.Value.3": "stopping",
      "Filter.2.Value.4": "stopped",
    });
    return reservations(root).flatMap((reservation) =>
      items(record(record(reservation)["instancesSet"])["item"]).map((instance) =>
        this.withRegion(instanceToMachine(instance)),
      ),
    );
  }

  async refreshSSHIngress(config: LeaseConfig, options: AWSIngressOptions = {}): Promise<void> {
    await this.ensureSecurityGroup(config, options);
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    options: AWSIngressOptions = {},
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    await this.ensureSSHKey(config.providerKey, config.sshPublicKey);
    let transientImageID = "";
    try {
      const defaultImageID = config.awsSnapshot
        ? await (async () => {
            transientImageID = await this.registerSnapshotImage(config.awsSnapshot, leaseID);
            return this.waitForImageAvailable(transientImageID);
          })()
        : config.target === "macos"
          ? ""
          : await this.resolveAMI(config);
      const securityGroupID = await this.ensureSecurityGroup(config, options);
      const candidates = awsLaunchCandidates(config);
      const failures: string[] = [];
      const attempts: ProvisioningAttempt[] = [];
      const quotaCache = new Map<string, number | undefined>();
      const imageCache = new Map<string, string>();
      const pinnedMacOSImageID =
        config.target === "macos" ? config.awsAMI || this.env.CRABBOX_AWS_AMI || "" : "";
      const resolveCandidateImageID = async (candidateConfig: LeaseConfig): Promise<string> => {
        if (defaultImageID) {
          return defaultImageID;
        }
        if (candidateConfig.target !== "macos") {
          return this.resolveAMI(candidateConfig);
        }
        const promotedImageID =
          config.awsPromotedAMIs[
            awsPromotedAMIConfigKey(this.region, candidateConfig.serverType)
          ] ?? "";
        if (promotedImageID) {
          return promotedImageID;
        }
        if (pinnedMacOSImageID && candidateConfig.serverType === config.serverType) {
          return pinnedMacOSImageID;
        }
        const query = awsMacOSAMIQuery(candidateConfig.serverType);
        const cacheKey = `${query.name}\0${query.architecture}`;
        const cached = imageCache.get(cacheKey);
        if (cached) {
          return cached;
        }
        const imageID = pinnedMacOSImageID
          ? await this.resolveLatestAmazonAMI(query.name, query.architecture)
          : await this.resolveAMI(candidateConfig);
        imageCache.set(cacheKey, imageID);
        return imageID;
      };
      for (const serverType of candidates) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- quota preflight follows sequential fallback order.
        const preflight = await this.quotaPreflightAttempt(
          serverType,
          config.capacityMarket,
          quotaCache,
        );
        if (preflight) {
          attempts.push(preflight);
          failures.push(`${serverType}: ${preflight.message}`);
          continue;
        }
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- instance-type fallback may need an architecture-specific AMI.
          const imageID = await resolveCandidateImageID({ ...config, serverType });
          // oxlint-disable-next-line eslint/no-await-in-loop -- instance-type fallback must stay sequential.
          const server = await this.createServer(
            { ...config, serverType },
            leaseID,
            slug,
            owner,
            imageID,
            securityGroupID,
          );
          const result: {
            server: ProviderMachine;
            serverType: string;
            market?: string;
            attempts?: ProvisioningAttempt[];
          } = { server, serverType, market: config.capacityMarket };
          if (attempts.length > 0) {
            result.attempts = attempts;
          }
          return result;
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
          attempts.push({
            region: this.region,
            serverType,
            market: config.capacityMarket,
            category: awsProvisioningErrorCategory(message) || "fatal",
            message: conciseAWSProvisioningMessage(message),
          });
          failures.push(`${serverType}: ${message}`);
          if (!isRetryableAWSProvisioningError(message)) {
            break;
          }
        }
      }
      if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
        for (const serverType of candidates) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback must stay sequential.
          const preflight = await this.quotaPreflightAttempt(serverType, "on-demand", quotaCache);
          if (preflight) {
            attempts.push(preflight);
            failures.push(`on-demand ${serverType}: ${preflight.message}`);
            continue;
          }
          try {
            // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback may need an architecture-specific AMI.
            const imageID = await resolveCandidateImageID({
              ...config,
              capacityMarket: "on-demand",
              serverType,
            });
            // oxlint-disable-next-line eslint/no-await-in-loop -- on-demand fallback must stay sequential.
            const server = await this.createServer(
              { ...config, capacityMarket: "on-demand", serverType },
              leaseID,
              slug,
              owner,
              imageID,
              securityGroupID,
            );
            const result: {
              server: ProviderMachine;
              serverType: string;
              market?: string;
              attempts?: ProvisioningAttempt[];
            } = { server, serverType, market: "on-demand" };
            if (attempts.length > 0) {
              result.attempts = attempts;
            }
            return result;
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            attempts.push({
              region: this.region,
              serverType,
              market: "on-demand",
              category: awsProvisioningErrorCategory(message) || "fatal",
              message: conciseAWSProvisioningMessage(message),
            });
            failures.push(`on-demand ${serverType}: ${message}`);
            if (!isRetryableAWSProvisioningError(message)) {
              break;
            }
          }
        }
      }
      if (config.serverTypeExplicit) {
        throw new Error(
          `requested exact AWS instance type ${config.serverType} failed; remove --type to allow class fallback: ${failures.join("; ")}`,
        );
      }
      throw new Error(failures.join("; "));
    } finally {
      if (transientImageID) {
        await this.ec2("DeregisterImage", { ImageId: transientImageID }).catch(() => undefined);
      }
    }
  }

  async getServer(instanceID: string): Promise<ProviderMachine> {
    const root = await this.ec2("DescribeInstances", {
      "InstanceId.1": instanceID,
    });
    for (const reservation of reservations(root)) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"])) {
        return this.withRegion(instanceToMachine(instance));
      }
    }
    throw new Error(`aws instance not found: ${instanceID}`);
  }

  async waitForServerIP(instanceID: string): Promise<ProviderMachine> {
    const deadline = Date.now() + 600_000;
    while (Date.now() < deadline) {
      let server: ProviderMachine | undefined;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- polling waits between EC2 reads.
        server = await this.getServer(instanceID);
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!isAWSInstanceNotFoundError(message)) {
          throw error;
        }
      }
      if (server?.host) {
        return server;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await sleep(5_000);
    }
    throw new Error(`timed out waiting for AWS instance public IP: ${instanceID}`);
  }

  async hourlySpotPriceUSD(instanceType: string): Promise<number | undefined> {
    const root = await this.ec2("DescribeSpotPriceHistory", {
      "InstanceType.1": instanceType,
      MaxResults: "1",
      "ProductDescription.1": "Linux/UNIX",
      StartTime: new Date().toISOString(),
    });
    const item = items(record(root["spotPriceHistorySet"])["item"])[0];
    return positiveFloat(asString(record(item)["spotPrice"]));
  }

  async deleteServer(instanceID: string): Promise<void> {
    await this.ec2("TerminateInstances", { "InstanceId.1": instanceID });
  }

  async createDiskSnapshot(instanceID: string, name: string): Promise<ProviderImage> {
    const source = await this.instanceRootVolumeMetadata(instanceID);
    const root = await this.ec2("CreateSnapshot", {
      VolumeId: source.volumeID,
      Description: `Crabbox checkpoint from ${instanceID}`,
      "TagSpecification.1.ResourceType": "snapshot",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "Name",
      "TagSpecification.1.Tag.3.Value": name,
      "TagSpecification.1.Tag.4.Key": "crabbox_root_device_name",
      "TagSpecification.1.Tag.4.Value": source.rootDeviceName,
      "TagSpecification.1.Tag.5.Key": "crabbox_architecture",
      "TagSpecification.1.Tag.5.Value": source.architecture,
    });
    const snapshotID = asString(root["snapshotId"]);
    if (!snapshotID) {
      throw new Error("aws returned no snapshot id");
    }
    return {
      id: snapshotID,
      name,
      state: asString(root["status"]) || "pending",
      provider: "aws",
      kind: "aws-ebs-snapshot",
      region: this.region,
      resourceID: snapshotID,
      snapshots: [snapshotID],
    };
  }

  async createImage(instanceID: string, name: string, noReboot: boolean): Promise<ProviderImage> {
    const params: Record<string, string> = {
      InstanceId: instanceID,
      Name: name,
      NoReboot: noReboot ? "true" : "false",
      "TagSpecification.1.ResourceType": "image",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "Name",
      "TagSpecification.1.Tag.3.Value": name,
    };
    const root = await this.ec2("CreateImage", params);
    const imageID = asString(root["imageId"]);
    if (!imageID) {
      throw new Error("aws returned no image id");
    }
    return {
      id: imageID,
      name,
      state: "pending",
      provider: "aws",
      kind: "aws-ami",
      region: this.region,
      resourceID: imageID,
    };
  }

  async getImage(imageID: string): Promise<ProviderImage> {
    if (imageID.startsWith("snap-")) {
      return await this.getSnapshot(imageID);
    }
    const root = await this.ec2("DescribeImages", {
      "ImageId.1": imageID,
    });
    const image = record(items(record(root["imagesSet"])["item"])[0]);
    const id = asString(image["imageId"]);
    if (!id) {
      throw new Error(`aws image not found: ${imageID}`);
    }
    return {
      id,
      name: asString(image["name"]),
      state: asString(image["imageState"]),
      provider: "aws",
      kind: "aws-ami",
      region: this.region,
      resourceID: id,
      architecture: asString(image["architecture"]),
      snapshots: imageSnapshotIDs(image),
    };
  }

  async deleteImage(imageID: string): Promise<void> {
    if (imageID.startsWith("snap-")) {
      await this.deleteSnapshotWithRetry(imageID);
      return;
    }
    const image = await this.getImage(imageID);
    await this.ec2("DeregisterImage", { ImageId: imageID }).catch((error: unknown) => {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidAMIID.NotFound")) {
        throw error;
      }
    });
    for (const snapshotID of image.snapshots ?? []) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EBS snapshot deletes are independent cleanup calls.
      await this.deleteSnapshotWithRetry(snapshotID);
    }
  }

  async enableFastSnapshotRestore(
    snapshotIDs: string[],
    availabilityZones: string[],
  ): Promise<ProviderFastSnapshotRestore[]> {
    const snapshots = uniqueStrings(snapshotIDs);
    const zones = uniqueStrings(availabilityZones);
    if (snapshots.length === 0 || zones.length === 0) {
      return [];
    }
    const params: Record<string, string> = {};
    snapshots.forEach((snapshotID, index) => {
      params[`SourceSnapshotId.${index + 1}`] = snapshotID;
    });
    zones.forEach((zone, index) => {
      params[`AvailabilityZone.${index + 1}`] = zone;
    });
    const root = await this.ec2("EnableFastSnapshotRestores", params);
    const successes = fastSnapshotRestoreItems(root, "enableFastSnapshotRestoreSuccessSet");
    const errors = fastSnapshotRestoreItems(root, "enableFastSnapshotRestoreErrorSet");
    if (errors.length > 0) {
      const message = errors
        .map((error) =>
          [
            error.snapshotID,
            error.availabilityZone,
            error.stateTransitionReason || error.state || "failed",
          ]
            .filter(Boolean)
            .join(":"),
        )
        .join(", ");
      throw new Error(`aws fast snapshot restore failed: ${message}`);
    }
    return successes.length > 0
      ? successes
      : snapshots.flatMap((snapshotID) =>
          zones.map((availabilityZone) => ({
            snapshotID,
            availabilityZone,
            state: "enabling",
          })),
        );
  }

  async fastSnapshotRestoreStatus(
    snapshotIDs: string[],
    availabilityZones: string[] = [],
  ): Promise<ProviderFastSnapshotRestore[]> {
    const snapshots = uniqueStrings(snapshotIDs);
    const zones = uniqueStrings(availabilityZones);
    if (snapshots.length === 0) {
      return [];
    }
    const params: Record<string, string> = {
      "Filter.1.Name": "snapshot-id",
    };
    snapshots.forEach((snapshotID, index) => {
      params[`Filter.1.Value.${index + 1}`] = snapshotID;
    });
    if (zones.length > 0) {
      params["Filter.2.Name"] = "availability-zone";
      zones.forEach((zone, index) => {
        params[`Filter.2.Value.${index + 1}`] = zone;
      });
    }
    const statuses: ProviderFastSnapshotRestore[] = [];
    let nextToken = "";
    for (let page = 0; page < 100; page++) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 pagination depends on the previous token.
      const root = await this.ec2("DescribeFastSnapshotRestores", {
        ...params,
        ...(nextToken ? { NextToken: nextToken } : {}),
      });
      statuses.push(...fastSnapshotRestoreItems(root, "fastSnapshotRestoreSet"));
      nextToken = asString(root["nextToken"]);
      if (!nextToken) {
        return statuses;
      }
    }
    throw new Error("aws fast snapshot restore status pagination exceeded 100 pages");
  }

  async listMacHosts(serverType = "", state = ""): Promise<AWSMacHost[]> {
    const params: Record<string, string> = {};
    let filter = 1;
    if (serverType) {
      params[`Filter.${filter}.Name`] = "instance-type";
      params[`Filter.${filter}.Value.1`] = serverType;
      filter++;
    }
    if (state) {
      params[`Filter.${filter}.Name`] = "state";
      params[`Filter.${filter}.Value.1`] = state;
    }
    const root = await this.ec2("DescribeHosts", params);
    return items(record(root["hostSet"])["item"])
      .map((host) => this.macHostFromDescribeHost(host))
      .filter((host) => host.instanceType.startsWith("mac"));
  }

  async listMacHostOfferings(serverType = "mac2.metal"): Promise<AWSMacHostOffering[]> {
    const root = await this.ec2("DescribeInstanceTypeOfferings", {
      LocationType: "availability-zone",
      "Filter.1.Name": "instance-type",
      "Filter.1.Value.1": serverType,
    });
    return awsMacHostOfferingsFromDescribeInstanceTypeOfferings(root, this.region);
  }

  async allocateMacHost(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Promise<AWSMacHost[]> {
    const params = this.allocateMacHostParams(serverType, availabilityZone, clientToken);
    const root = await this.ec2("AllocateHosts", params);
    const hostIDs = awsHostIDsFromSet(root["hostIdSet"]);
    if (hostIDs.length === 0) {
      return [];
    }
    const fallbackHosts = this.macHostsFromAllocatedIDs(hostIDs, availabilityZone, serverType);
    let hosts: AWSMacHost[];
    try {
      hosts = await this.describeMacHostsByID(hostIDs);
    } catch {
      return fallbackHosts;
    }
    return hosts.length > 0 ? hosts : fallbackHosts;
  }

  async dryRunAllocateMacHost(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Promise<AWSMacHostAllocationDryRun> {
    try {
      await this.ec2("AllocateHosts", {
        ...this.allocateMacHostParams(serverType, availabilityZone, clientToken),
        DryRun: "true",
      });
      return {
        region: this.region,
        availabilityZone,
        instanceType: serverType,
        ok: true,
        message: "dry run accepted",
      };
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return {
        region: this.region,
        availabilityZone,
        instanceType: serverType,
        ok: message.includes("DryRunOperation"),
        message: conciseAWSMacHostDryRunMessage(message),
      };
    }
  }

  async listMacHostQuotas(serverType = "mac2.metal"): Promise<AWSServiceQuota[]> {
    const family = awsMacHostFamily(serverType);
    const direct = await this.macHostQuotasByFamily(family);
    if (direct) {
      return direct;
    }
    const all = await this.listEC2ServiceQuotas();
    const macHostQuotas = all.filter((quota) => {
      const name = quota.quotaName.toLowerCase();
      return name.includes("running dedicated") && name.includes("mac") && name.includes("hosts");
    });
    const exactName = `running dedicated ${family} hosts`;
    const exact = macHostQuotas.filter((quota) => quota.quotaName.toLowerCase() === exactName);
    return (exact.length > 0 ? exact : macHostQuotas).toSorted((left, right) => {
      const leftName = left.quotaName.toLowerCase();
      const rightName = right.quotaName.toLowerCase();
      const leftFamily = family && leftName.includes(family) ? 0 : 1;
      const rightFamily = family && rightName.includes(family) ? 0 : 1;
      return leftFamily - rightFamily || left.quotaName.localeCompare(right.quotaName);
    });
  }

  private async macHostQuotasByFamily(family: string): Promise<AWSServiceQuota[] | undefined> {
    const spec = awsMacHostQuotaSpecs[family];
    if (!spec) {
      return undefined;
    }
    const quota = await this.getEC2ServiceQuota(spec.quotaCode);
    if (!quota) {
      return [];
    }
    return [
      {
        ...quota,
        quotaCode: quota.quotaCode || spec.quotaCode,
        quotaName: quota.quotaName || spec.quotaName,
        serviceCode: quota.serviceCode || "ec2",
      },
    ];
  }

  async releaseMacHost(hostID: string): Promise<string[]> {
    const root = await this.ec2("ReleaseHosts", { "HostId.1": hostID });
    const result = awsReleaseHostsResult(root);
    if (result.unsuccessful.length > 0) {
      const details = result.unsuccessful.map((failure) =>
        [failure.resourceID || hostID, failure.code, failure.message].filter(Boolean).join(": "),
      );
      throw new Error(`aws ReleaseHosts failed: ${details.join("; ")}`);
    }
    if (!result.successful.includes(hostID)) {
      throw new Error(`aws ReleaseHosts did not confirm release for ${hostID}`);
    }
    return result.successful;
  }

  private allocateMacHostParams(
    serverType: string,
    availabilityZone: string,
    clientToken: string,
  ): Record<string, string> {
    const params: Record<string, string> = {
      AutoPlacement: "off",
      ClientToken: clientToken,
      InstanceType: serverType,
      Quantity: "1",
      "TagSpecification.1.ResourceType": "dedicated-host",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
    };
    if (availabilityZone) {
      params["AvailabilityZone"] = availabilityZone;
    }
    return params;
  }

  private async getSnapshot(snapshotID: string): Promise<ProviderImage> {
    const root = await this.ec2("DescribeSnapshots", {
      "SnapshotId.1": snapshotID,
    });
    const snapshot = record(items(record(root["snapshotSet"])["item"])[0]);
    const id = asString(snapshot["snapshotId"]);
    if (!id) {
      throw new Error(`aws snapshot not found: ${snapshotID}`);
    }
    return {
      id,
      name: tagValue(snapshot, "Name") || id,
      state: asString(snapshot["status"]),
      provider: "aws",
      kind: "aws-ebs-snapshot",
      region: this.region,
      resourceID: id,
      snapshots: [id],
    };
  }

  private async instanceRootVolumeMetadata(instanceID: string): Promise<{
    volumeID: string;
    rootDeviceName: string;
    architecture: string;
  }> {
    const root = await this.ec2("DescribeInstances", {
      "InstanceId.1": instanceID,
    });
    for (const reservation of reservations(root)) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"])) {
        const inst = record(instance);
        const rootDevice = asString(inst["rootDeviceName"]) || "/dev/sda1";
        for (const mapping of items(record(inst["blockDeviceMapping"])["item"])) {
          const map = record(mapping);
          if (asString(map["deviceName"]) !== rootDevice) continue;
          const volumeID = asString(record(map["ebs"])["volumeId"]);
          if (volumeID) {
            return {
              volumeID,
              rootDeviceName: rootDevice,
              architecture: asString(inst["architecture"]) || "x86_64",
            };
          }
        }
      }
    }
    throw new Error(`aws root volume not found for instance ${instanceID}`);
  }

  private async registerSnapshotImage(snapshotID: string, leaseID: string): Promise<string> {
    const name = `crabbox-${leaseID.replaceAll("_", "-")}-${Date.now()}`;
    const metadata = await this.snapshotBootMetadata(snapshotID);
    const root = await this.ec2("RegisterImage", {
      Name: name,
      Architecture: metadata.architecture,
      RootDeviceName: metadata.rootDeviceName,
      VirtualizationType: "hvm",
      EnaSupport: "true",
      "BlockDeviceMapping.1.DeviceName": metadata.rootDeviceName,
      "BlockDeviceMapping.1.Ebs.SnapshotId": snapshotID,
      "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
      "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
      "TagSpecification.1.ResourceType": "image",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
      "TagSpecification.1.Tag.3.Key": "transient_checkpoint_snapshot",
      "TagSpecification.1.Tag.3.Value": snapshotID,
    });
    const imageID = asString(root["imageId"]);
    if (!imageID) {
      throw new Error("aws returned no transient image id");
    }
    return imageID;
  }

  private async snapshotBootMetadata(snapshotID: string): Promise<{
    rootDeviceName: string;
    architecture: string;
  }> {
    const root = await this.ec2("DescribeSnapshots", {
      "SnapshotId.1": snapshotID,
    });
    const snapshot = record(items(record(root["snapshotSet"])["item"])[0]);
    return {
      rootDeviceName: tagValue(snapshot, "crabbox_root_device_name") || "/dev/sda1",
      architecture: tagValue(snapshot, "crabbox_architecture") || "x86_64",
    };
  }

  private async waitForImageAvailable(imageID: string): Promise<string> {
    const deadline = Date.now() + 600_000;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- EC2 AMI availability is eventually consistent.
      const image = await this.getImage(imageID);
      if (image.state === "available") return imageID;
      if (image.state === "failed" || image.state === "invalid") {
        throw new Error(`aws transient image ${imageID} is ${image.state}`);
      }
      if (Date.now() > deadline) {
        throw new Error(`timed out waiting for AWS transient image ${imageID}`);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling interval.
      await sleep(5_000);
    }
  }

  private async deleteSnapshotWithRetry(snapshotID: string): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt <= snapshotDeleteBackoffMs.length; attempt++) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- retry preserves snapshot IDs after AMI deregistration.
        await this.ec2("DeleteSnapshot", { SnapshotId: snapshotID });
        return;
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (message.includes("InvalidSnapshot.NotFound")) {
          return;
        }
        if (!isRetryableSnapshotDeleteError(message)) {
          throw error;
        }
        lastError = error;
        const backoffMs = snapshotDeleteBackoffMs[attempt];
        if (backoffMs === undefined) {
          break;
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- AWS can keep just-deregistered AMI snapshots locked briefly.
        await sleep(backoffMs);
      }
    }
    throw lastError;
  }

  async deleteSSHKey(name: string): Promise<void> {
    await this.ec2("DeleteKeyPair", { KeyName: name }).catch((error: unknown) => {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidKeyPair.NotFound")) {
        throw error;
      }
    });
  }

  async setTags(instanceID: string, labels: Record<string, string>): Promise<void> {
    const params: Record<string, string> = { "ResourceId.1": instanceID };
    addTags(params, "Tag", labels);
    await this.ec2("CreateTags", params);
  }

  private async ensureSSHKey(name: string, publicKey: string): Promise<void> {
    try {
      await this.ec2("DescribeKeyPairs", { "KeyName.1": name });
      return;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("InvalidKeyPair.NotFound")) {
        throw error;
      }
    }
    await this.ec2("ImportKeyPair", {
      KeyName: name,
      PublicKeyMaterial: btoa(publicKey),
      "TagSpecification.1.ResourceType": "key-pair",
      "TagSpecification.1.Tag.1.Key": "crabbox",
      "TagSpecification.1.Tag.1.Value": "true",
      "TagSpecification.1.Tag.2.Key": "created_by",
      "TagSpecification.1.Tag.2.Value": "crabbox",
    });
  }

  private async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    imageID: string,
    securityGroupID: string,
  ): Promise<ProviderMachine> {
    const now = new Date();
    const name = leaseProviderName(leaseID, slug);
    const labels = leaseProviderLabels(config, leaseID, slug, owner, "aws", now, {
      market: config.capacityMarket,
    });
    const rootGB = config.awsRootGB || positiveInt(this.env.CRABBOX_AWS_ROOT_GB) || 400;
    const instanceProfile = config.awsProfile || this.env.CRABBOX_AWS_INSTANCE_PROFILE || "";
    const subnetID = config.awsSubnetID || this.env.CRABBOX_AWS_SUBNET_ID || "";
    const configuredMacHostID =
      config.hostID ||
      config.awsMacHostID ||
      this.env.CRABBOX_HOST_ID ||
      this.env.CRABBOX_AWS_MAC_HOST_ID ||
      "";
    const macHostAvailabilityZone =
      config.target === "macos"
        ? await this.macHostAvailabilityZoneForLaunch(config, subnetID)
        : "";
    let lastMacHostID = "";
    const run = async (macHostID: string): Promise<ProviderMachine> => {
      const params: Record<string, string> = {
        ClientToken: leaseID,
        ImageId: imageID,
        InstanceType: config.serverType,
        KeyName: config.providerKey,
        MaxCount: "1",
        MinCount: "1",
        UserData: await awsRunInstancesUserData(config),
        "BlockDeviceMapping.1.DeviceName": "/dev/sda1",
        "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
        "BlockDeviceMapping.1.Ebs.Encrypted": "true",
        "BlockDeviceMapping.1.Ebs.VolumeSize": String(Math.max(1, rootGB)),
        "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
      };
      if (config.capacityMarket !== "on-demand") {
        params["InstanceMarketOptions.MarketType"] = "spot";
        params["InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior"] = "terminate";
        params["InstanceMarketOptions.SpotOptions.SpotInstanceType"] = "one-time";
      }
      if (instanceProfile) {
        params["IamInstanceProfile.Name"] = instanceProfile;
      }
      if (subnetID) {
        params["NetworkInterface.1.AssociatePublicIpAddress"] = "true";
        params["NetworkInterface.1.DeleteOnTermination"] = "true";
        params["NetworkInterface.1.DeviceIndex"] = "0";
        params["NetworkInterface.1.GroupSet.1"] = securityGroupID;
        params["NetworkInterface.1.SubnetId"] = subnetID;
      } else {
        params["SecurityGroupId.1"] = securityGroupID;
      }
      applyAWSRunInstanceTargetOptions(params, config);
      if (config.target === "macos") {
        const hostID =
          macHostID ||
          (await this.discoverMacHostID(config.serverType, "", macHostAvailabilityZone));
        lastMacHostID = hostID;
        params["Placement.HostId"] = hostID;
        params["Placement.Tenancy"] = "host";
      } else if (!subnetID) {
        const availabilityZone = awsAvailabilityZoneForRegion(config, this.env, this.region);
        if (availabilityZone) {
          params["Placement.AvailabilityZone"] = availabilityZone;
        }
      }
      addRunInstancesTagSpecifications(params, { ...labels, Name: name }, config.capacityMarket);
      const root = await this.ec2("RunInstances", params);
      const instance = items(record(root["instancesSet"])["item"])[0];
      if (!instance) {
        throw new Error("aws returned no instances");
      }
      return this.withRegion(instanceToMachine(instance));
    };
    try {
      return await run(configuredMacHostID);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (
        config.target === "macos" &&
        ((configuredMacHostID && isAWSInvalidHostIDError(message)) ||
          (!configuredMacHostID && isAWSInsufficientCapacityOnHostError(message)))
      ) {
        const discovered = await this.discoverMacHostID(
          config.serverType,
          configuredMacHostID || lastMacHostID,
          macHostAvailabilityZone,
        );
        return run(discovered);
      }
      if (config.target !== "macos" || !configuredMacHostID || !isAWSInvalidHostIDError(message)) {
        throw error;
      }
      const discovered = await this.discoverMacHostID(
        config.serverType,
        configuredMacHostID,
        macHostAvailabilityZone,
      );
      return run(discovered);
    }
  }

  private async resolveAMI(config: LeaseConfig): Promise<string> {
    if (config.awsAMI || this.env.CRABBOX_AWS_AMI) {
      return config.awsAMI || this.env.CRABBOX_AWS_AMI || "";
    }
    if (config.target === "windows") {
      return this.resolveLatestAmazonAMI("Windows_Server-2022-English-Full-Base-*", "x86_64");
    }
    if (config.target === "macos") {
      const query = awsMacOSAMIQuery(config.serverType);
      return this.resolveLatestAmazonAMI(query.name, query.architecture);
    }
    const os = osImageSpec(config.os);
    const architecture = config.architecture === "arm64" ? "arm64" : "x86_64";
    const name = config.architecture === "arm64" ? os.awsArm64Name : os.awsName;
    return this.resolveLatestAMI(
      awsUbuntuOwner,
      name,
      architecture,
      `no ${os.awsLabel} ${architecture} AMI found in ${this.region}`,
    );
  }

  private async resolveLatestAmazonAMI(name: string, architecture: string): Promise<string> {
    return this.resolveLatestAMI(
      "amazon",
      name,
      architecture,
      `no AWS AMI found in ${this.region} for name=${name} architecture=${architecture}`,
    );
  }

  private async resolveLatestAMI(
    owner: string,
    name: string,
    architecture: string,
    emptyMessage: string,
  ): Promise<string> {
    const root = await this.ec2("DescribeImages", {
      "Owner.1": owner,
      "Filter.1.Name": "architecture",
      "Filter.1.Value.1": architecture,
      "Filter.2.Name": "name",
      "Filter.2.Value.1": name,
      "Filter.3.Name": "root-device-type",
      "Filter.3.Value.1": "ebs",
      "Filter.4.Name": "virtualization-type",
      "Filter.4.Value.1": "hvm",
    });
    const images = items(record(root["imagesSet"])["item"]).toSorted((left, right) =>
      asString(record(right)["creationDate"]).localeCompare(asString(record(left)["creationDate"])),
    );
    const imageID = asString(record(images[0])["imageId"]);
    if (!imageID) {
      throw new Error(emptyMessage);
    }
    return imageID;
  }

  private async ensureSecurityGroup(
    config: LeaseConfig,
    options: AWSIngressOptions = {},
  ): Promise<string> {
    const configuredGroupID = config.awsSGID || this.env.CRABBOX_AWS_SECURITY_GROUP_ID || "";
    let group: unknown;
    let groupID = configuredGroupID;
    if (groupID) {
      const existing = await this.ec2("DescribeSecurityGroups", {
        "GroupId.1": groupID,
      });
      group = items(record(existing["securityGroupInfo"])["item"])[0];
    } else {
      const vpcID = await this.securityGroupVPC(config);
      const name = "crabbox-runners";
      const existing = await this.ec2("DescribeSecurityGroups", {
        "Filter.1.Name": "group-name",
        "Filter.1.Value.1": name,
        "Filter.2.Name": "vpc-id",
        "Filter.2.Value.1": vpcID,
      });
      group = items(record(existing["securityGroupInfo"])["item"])[0];
      groupID = asString(record(group)["groupId"]);
      if (!groupID) {
        const created = await this.ec2(
          "CreateSecurityGroup",
          createSecurityGroupParams(name, vpcID),
        );
        groupID = asString(record(created)["groupId"]);
      }
    }
    if (!groupID) {
      throw new Error("aws security group id is empty");
    }
    const cidrs = awsSSHCIDRs(config, this.env);
    const ports = sshPorts(config);
    if (options.reconcile !== "additive") {
      await this.pruneStaleSSHIngress(groupID, group, ports, cidrs);
    }
    let compactedAfterRuleLimit = false;
    for (const port of ports) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- cleanup is per port.
      await this.revokeWorldTCP(groupID, port).catch((error: unknown) => {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidPermission.NotFound")) {
          throw error;
        }
      });
      for (const cidr of cidrs) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- duplicate ingress handling is per CIDR.
        await this.allowTCP(groupID, port, cidr).catch((error: unknown) => {
          const message = error instanceof Error ? error.message : String(error);
          if (message.includes("InvalidPermission.Duplicate")) {
            return;
          }
          if (
            options.reconcile !== "additive" &&
            !compactedAfterRuleLimit &&
            isAWSSecurityGroupRuleLimitError(message)
          ) {
            compactedAfterRuleLimit = true;
            return this.compactSSHIngressForRuleLimit(groupID, ports, cidrs).then((compacted) => {
              if (!compacted) {
                throw error;
              }
              return this.allowTCP(groupID, port, cidr).catch((retryError: unknown) => {
                const retryMessage =
                  retryError instanceof Error ? retryError.message : String(retryError);
                if (!retryMessage.includes("InvalidPermission.Duplicate")) {
                  throw retryError;
                }
              });
            });
          }
          throw error;
        });
      }
    }
    return groupID;
  }

  private async discoverMacHostID(
    serverType: string,
    excludedHostID = "",
    availabilityZone = "",
  ): Promise<string> {
    const params: Record<string, string> = {
      "Filter.1.Name": "instance-type",
      "Filter.1.Value.1": serverType,
      "Filter.2.Name": "state",
      "Filter.2.Value.1": "available",
      "Filter.3.Name": "tag:crabbox",
      "Filter.3.Value.1": "true",
    };
    if (availabilityZone) {
      params["Filter.4.Name"] = "availability-zone";
      params["Filter.4.Value.1"] = availabilityZone;
    }
    const root = await this.ec2("DescribeHosts", params);
    const hostID = awsMacHostIDFromDescribeHosts(root, excludedHostID, serverType);
    if (!hostID) {
      const zoneHint = availabilityZone ? ` in ${availabilityZone}` : "";
      throw new Error(
        `no available EC2 Mac Dedicated Host found in ${this.region}${zoneHint} for ${serverType}; allocate a host or set CRABBOX_HOST_ID`,
      );
    }
    return hostID;
  }

  private async macHostAvailabilityZoneForLaunch(
    config: LeaseConfig,
    subnetID: string,
  ): Promise<string> {
    if (subnetID) {
      return this.subnetAvailabilityZone(subnetID);
    }
    return awsAvailabilityZoneForRegion(config, this.env, this.region);
  }

  private async subnetAvailabilityZone(subnetID: string): Promise<string> {
    const root = await this.ec2("DescribeSubnets", { "SubnetId.1": subnetID });
    const subnet = record(items(record(root["subnetSet"])["item"])[0]);
    const availabilityZone = asString(subnet["availabilityZone"]);
    if (!availabilityZone) {
      throw new Error(`AWS subnet not found: ${subnetID}`);
    }
    return availabilityZone;
  }

  private async describeMacHostsByID(hostIDs: string[]): Promise<AWSMacHost[]> {
    const params: Record<string, string> = {};
    hostIDs.forEach((hostID, index) => {
      params[`HostId.${index + 1}`] = hostID;
    });
    const root = await this.ec2("DescribeHosts", params);
    return items(record(root["hostSet"])["item"]).map((host) => this.macHostFromDescribeHost(host));
  }

  private macHostsFromAllocatedIDs(
    hostIDs: string[],
    availabilityZone: string,
    serverType: string,
  ): AWSMacHost[] {
    return hostIDs.map((id) => ({
      id,
      state: "available",
      region: this.region,
      availabilityZone,
      instanceType: serverType,
      autoPlacement: "off",
      tags: {},
    }));
  }

  private macHostFromDescribeHost(input: unknown): AWSMacHost {
    const host = record(input);
    const properties = record(host["hostProperties"]);
    const instanceType = asString(properties["instanceType"] ?? host["instanceType"]);
    const macHost: AWSMacHost = {
      id: asString(host["hostId"]),
      state: asString(host["hostState"] ?? host["state"]),
      region: this.region,
      availabilityZone: asString(host["availabilityZone"]),
      instanceType,
      autoPlacement: asString(host["autoPlacement"]),
      tags: tagMap(host["tagSet"]),
    };
    const availableCapacity = awsMacHostAvailableCapacity(host, instanceType);
    if (availableCapacity !== undefined) {
      macHost.availableCapacity = availableCapacity;
    }
    const availableVCpus = finiteNumber(record(host["availableCapacity"])["availableVCpus"]);
    if (availableVCpus !== undefined) {
      macHost.availableVCpus = availableVCpus;
    }
    const allocationTime = asString(host["allocationTime"]);
    if (allocationTime) {
      macHost.allocationTime = allocationTime;
    }
    const releaseTime = asString(host["releaseTime"]);
    if (releaseTime) {
      macHost.releaseTime = releaseTime;
    }
    return macHost;
  }

  private async pruneStaleSSHIngress(
    groupID: string,
    group: unknown,
    ports: string[],
    cidrs: string[],
  ): Promise<void> {
    for (const rule of staleCrabboxSSHIngressRules(group, ports, cidrs)) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- revoke calls are per exact rule.
      await this.revokeTCP(groupID, rule).catch((error: unknown) => {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidPermission.NotFound")) {
          throw error;
        }
      });
    }
  }

  private async compactSSHIngressForRuleLimit(
    groupID: string,
    ports: string[],
    cidrs: string[],
  ): Promise<boolean> {
    const existing = await this.ec2("DescribeSecurityGroups", {
      "GroupId.1": groupID,
    });
    const group = items(record(existing["securityGroupInfo"])["item"])[0];
    const rules = reclaimableCrabboxSSHIngressRules(group, ports, cidrs);
    for (const rule of rules) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- revoke calls are per exact rule.
      await this.revokeTCP(groupID, rule).catch((error: unknown) => {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.includes("InvalidPermission.NotFound")) {
          throw error;
        }
      });
    }
    return rules.length > 0;
  }

  private async securityGroupVPC(config: LeaseConfig): Promise<string> {
    const subnetID = config.awsSubnetID || this.env.CRABBOX_AWS_SUBNET_ID || "";
    if (!subnetID) {
      const root = await this.ec2("DescribeVpcs", {
        "Filter.1.Name": "is-default",
        "Filter.1.Value.1": "true",
      });
      const vpcID = asString(record(items(record(root["vpcSet"])["item"])[0])["vpcId"]);
      if (!vpcID) {
        throw new Error("no default VPC found; set awsSubnetID and awsSGID");
      }
      return vpcID;
    }
    const root = await this.ec2("DescribeSubnets", { "SubnetId.1": subnetID });
    const vpcID = asString(record(items(record(root["subnetSet"])["item"])[0])["vpcId"]);
    if (!vpcID) {
      throw new Error(`AWS subnet not found: ${subnetID}`);
    }
    return vpcID;
  }

  private async allowTCP(groupID: string, port: string, cidr: string): Promise<void> {
    if (!/^[1-9][0-9]{0,4}$/.test(port) || Number(port) > 65_535) {
      throw new Error(`invalid SSH port: ${port}`);
    }
    const params: Record<string, string> = {
      GroupId: groupID,
      "IpPermissions.1.FromPort": port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.ToPort": port,
    };
    assignSSHIngressRange(params, cidr, true);
    await this.ec2("AuthorizeSecurityGroupIngress", params);
  }

  private async revokeWorldTCP(groupID: string, port: string): Promise<void> {
    await this.ec2("RevokeSecurityGroupIngress", {
      GroupId: groupID,
      "IpPermissions.1.FromPort": port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.IpRanges.1.CidrIp": "0.0.0.0/0",
      "IpPermissions.1.ToPort": port,
    });
  }

  private async revokeTCP(groupID: string, rule: SSHIngressRule): Promise<void> {
    const params: Record<string, string> = {
      GroupId: groupID,
      "IpPermissions.1.FromPort": rule.port,
      "IpPermissions.1.IpProtocol": "tcp",
      "IpPermissions.1.ToPort": rule.port,
    };
    assignSSHIngressRule(params, rule);
    await this.ec2("RevokeSecurityGroupIngress", params);
  }

  private async ec2(
    action: string,
    params: Record<string, string>,
  ): Promise<Record<string, unknown>> {
    const body = new URLSearchParams({ Action: action, Version: ec2Version, ...params });
    const response = await this.aws.fetch(this.endpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: body.toString(),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(this.awsQueryErrorMessage(action, response.status, text));
    }
    const parsed = this.parser.parse(text) as unknown;
    const parsedRecord = record(parsed);
    const root = parsedRecord[`${action}Response`] ?? parsedRecord["Response"] ?? parsedRecord;
    return record(root);
  }

  private async sts(
    action: string,
    params: Record<string, string>,
  ): Promise<Record<string, unknown>> {
    const body = new URLSearchParams({ Action: action, Version: stsVersion, ...params });
    const response = await this.stsClient.fetch(this.stsEndpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded; charset=utf-8" },
      body: body.toString(),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(this.awsQueryErrorMessage(action, response.status, text));
    }
    const parsed = this.parser.parse(text) as unknown;
    const parsedRecord = record(parsed);
    const root = parsedRecord[`${action}Response`] ?? parsedRecord["Response"] ?? parsedRecord;
    return record(root);
  }

  private awsQueryErrorMessage(action: string, status: number, text: string): string {
    let detail = "";
    try {
      const parsed = this.parser.parse(text) as unknown;
      const parsedRecord = record(parsed);
      const root = record(
        parsedRecord["ErrorResponse"] ?? parsedRecord["Response"] ?? parsedRecord,
      );
      const error = record(
        items(
          root["Error"] ??
            root["error"] ??
            record(root["Errors"])["Error"] ??
            record(root["Errors"])["error"],
        )[0],
      );
      const code = asString(error["Code"] ?? error["code"]);
      const message = asString(error["Message"] ?? error["message"]);
      detail = code && message ? `${code}: ${message}` : code || message;
    } catch {
      detail = "";
    }
    return `aws ${action}: http ${status}: ${detail || trimBody(text).replace(/\s+/g, " ")}`;
  }

  private async quotaPreflightAttempt(
    serverType: string,
    market: LeaseConfig["capacityMarket"],
    quotaCache: Map<string, number | undefined>,
  ): Promise<ProvisioningAttempt | undefined> {
    const code = awsQuotaCodeForMarket(market);
    let quota = quotaCache.get(code);
    if (!quotaCache.has(code)) {
      quota = await this.appliedServiceQuota(code);
      quotaCache.set(code, quota);
    }
    return awsQuotaPreflightAttempt(serverType, market, this.region, quota);
  }

  private async appliedServiceQuota(quotaCode: string): Promise<number | undefined> {
    try {
      const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
        method: "POST",
        headers: {
          "content-type": "application/x-amz-json-1.1",
          "x-amz-target": "ServiceQuotasV20190624.GetServiceQuota",
        },
        body: JSON.stringify({ ServiceCode: "ec2", QuotaCode: quotaCode }),
      });
      if (!response.ok) {
        return undefined;
      }
      const parsed = record(await response.json());
      const quota = record(parsed["Quota"]);
      return positiveNumber(quota["Value"]);
    } catch {
      return undefined;
    }
  }

  private async listEC2ServiceQuotas(): Promise<AWSServiceQuota[]> {
    const quotas: AWSServiceQuota[] = [];
    let nextToken = "";
    do {
      const body: Record<string, string | number> = { ServiceCode: "ec2", MaxResults: 100 };
      if (nextToken) {
        body["NextToken"] = nextToken;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- Service Quotas pagination is token-ordered.
      const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
        method: "POST",
        headers: {
          "content-type": "application/x-amz-json-1.1",
          "x-amz-target": "ServiceQuotasV20190624.ListServiceQuotas",
        },
        body: JSON.stringify(body),
      });
      // oxlint-disable-next-line eslint/no-await-in-loop -- response body belongs to the current page.
      const text = await response.text();
      if (!response.ok) {
        throw new Error(`aws ListServiceQuotas: http ${response.status}: ${trimBody(text)}`);
      }
      const parsed = record(JSON.parse(text));
      for (const quota of items(parsed["Quotas"]).map(record)) {
        const out = awsServiceQuotaFromRecord(quota);
        if (out) {
          quotas.push(out);
        }
      }
      nextToken = asString(parsed["NextToken"]);
    } while (nextToken);
    return quotas;
  }

  private async getEC2ServiceQuota(quotaCode: string): Promise<AWSServiceQuota | undefined> {
    const response = await this.serviceQuotas.fetch(this.serviceQuotasEndpoint, {
      method: "POST",
      headers: {
        "content-type": "application/x-amz-json-1.1",
        "x-amz-target": "ServiceQuotasV20190624.GetServiceQuota",
      },
      body: JSON.stringify({ ServiceCode: "ec2", QuotaCode: quotaCode }),
    });
    const text = await response.text();
    if (!response.ok) {
      if (text.includes("NoSuchResourceException")) {
        return undefined;
      }
      throw new Error(`aws GetServiceQuota: http ${response.status}: ${trimBody(text)}`);
    }
    const quota = awsServiceQuotaFromRecord(record(JSON.parse(text))["Quota"]);
    if (!quota) {
      throw new Error(`aws GetServiceQuota: missing quota ${quotaCode}`);
    }
    return quota;
  }

  private withRegion(server: ProviderMachine): ProviderMachine {
    return { ...server, region: this.region };
  }
}

function awsSSHCIDRs(config: LeaseConfig, env: Env): string[] {
  const configured = [...config.awsSSHCIDRs, ...(env.CRABBOX_AWS_SSH_CIDRS ?? "").split(",")];
  const cidrs = validCIDRs(configured);
  if (cidrs.length === 0) {
    throw new Error(
      "AWS SSH source CIDR is required; set CRABBOX_AWS_SSH_CIDRS or use Cloudflare request IP forwarding",
    );
  }
  return cidrs;
}

function reservations(root: Record<string, unknown>): Record<string, unknown>[] {
  return items(record(root["reservationSet"])["item"]).map(record);
}

function instanceToMachine(input: unknown): ProviderMachine {
  const instance = record(input);
  const tags = tagMap(instance["tagSet"]);
  const cloudID = asString(instance["instanceId"]);
  return {
    provider: "aws",
    id: 0,
    cloudID,
    name: tags["Name"] || cloudID,
    status: asString(record(instance["instanceState"])["name"]),
    serverType: asString(instance["instanceType"]),
    hostID: asString(record(instance["placement"])["hostId"]),
    host: asString(instance["ipAddress"]),
    labels: tags,
  };
}

export function awsMacHostIDFromDescribeHosts(
  root: Record<string, unknown>,
  excludedHostID = "",
  serverType = "",
): string {
  const selectedHost = items(record(root["hostSet"])["item"])
    .map(record)
    .find((host) => {
      const hostID = asString(host["hostId"]);
      return (
        hostID &&
        hostID !== excludedHostID &&
        asString(host["hostState"] ?? host["state"]) === "available" &&
        awsMacHostHasLaunchCapacity(host, serverType)
      );
    });
  return asString(selectedHost?.["hostId"]);
}

function awsMacHostHasLaunchCapacity(host: Record<string, unknown>, serverType: string): boolean {
  const availableCapacity = awsMacHostAvailableCapacity(host, serverType);
  if (availableCapacity !== undefined) {
    return availableCapacity > 0;
  }
  const availableVCpus = finiteNumber(record(host["availableCapacity"])["availableVCpus"]);
  return availableVCpus === undefined || availableVCpus > 0;
}

function awsMacHostAvailableCapacity(
  host: Record<string, unknown>,
  serverType: string,
): number | undefined {
  if (!serverType) {
    return undefined;
  }
  const capacity = record(host["availableCapacity"]);
  const itemsSet = record(capacity["availableInstanceCapacity"]);
  for (const item of items(itemsSet["item"]).map(record)) {
    if (asString(item["instanceType"]) !== serverType) {
      continue;
    }
    return finiteNumber(item["availableCapacity"]);
  }
  return undefined;
}

export function awsHostIDsFromSet(input: unknown): string[] {
  return items(record(input)["item"])
    .map((item) => asString(record(item)["hostId"]) || asString(item))
    .filter(Boolean);
}

export function awsReleaseHostsResult(root: Record<string, unknown>): AWSReleaseHostsResult {
  const unsuccessful = items(record(root["unsuccessful"])["item"])
    .map((item) => {
      const failure = record(item);
      const error = record(failure["error"]);
      return {
        resourceID: asString(failure["resourceId"]),
        code: asString(error["code"]),
        message: asString(error["message"]),
      };
    })
    .filter((failure) => failure.resourceID || failure.code || failure.message);
  return {
    successful: awsHostIDsFromSet(root["successful"]),
    unsuccessful,
  };
}

export function awsMacHostOfferingsFromDescribeInstanceTypeOfferings(
  root: Record<string, unknown>,
  region: string,
): AWSMacHostOffering[] {
  const seen = new Set<string>();
  return items(record(root["instanceTypeOfferingSet"])["item"])
    .map(record)
    .map((item) => ({
      region,
      availabilityZone: asString(item["location"]),
      instanceType: asString(item["instanceType"]),
    }))
    .filter((offering) => {
      if (
        !offering.availabilityZone.startsWith(region) ||
        !offering.instanceType.startsWith("mac") ||
        !offering.instanceType.endsWith(".metal")
      ) {
        return false;
      }
      const key = `${offering.availabilityZone}\0${offering.instanceType}`;
      if (seen.has(key)) {
        return false;
      }
      seen.add(key);
      return true;
    })
    .toSorted((left, right) => {
      const azCompare = left.availabilityZone.localeCompare(right.availabilityZone);
      return azCompare || left.instanceType.localeCompare(right.instanceType);
    });
}

function tagMap(input: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  for (const item of items(record(input)["item"])) {
    const tag = record(item);
    const key = asString(tag["key"]);
    if (key) {
      out[key] = asString(tag["value"]);
    }
  }
  return out;
}

function addTags(
  params: Record<string, string>,
  prefix: string,
  labels: Record<string, string>,
): void {
  Object.entries(labels)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .forEach(([key, value], index) => {
      const tag = index + 1;
      params[`${prefix}.${tag}.Key`] = key;
      params[`${prefix}.${tag}.Value`] = value;
    });
}

export function addRunInstancesTagSpecifications(
  params: Record<string, string>,
  labels: Record<string, string>,
  market: string,
): void {
  params["TagSpecification.1.ResourceType"] = "instance";
  params["TagSpecification.2.ResourceType"] = "volume";
  addTags(params, "TagSpecification.1.Tag", labels);
  addTags(params, "TagSpecification.2.Tag", labels);
  if (market !== "on-demand") {
    params["TagSpecification.3.ResourceType"] = "spot-instances-request";
    addTags(params, "TagSpecification.3.Tag", labels);
  }
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function items(value: unknown): unknown[] {
  if (Array.isArray(value)) {
    return value;
  }
  return value === undefined ? [] : [value];
}

function imageSnapshotIDs(image: Record<string, unknown>): string[] {
  const mappings = items(record(image["blockDeviceMapping"])["item"]);
  const snapshots = mappings
    .map((mapping) => asString(record(record(mapping)["ebs"])["snapshotId"]))
    .filter((snapshotID) => snapshotID !== "");
  return [...new Set(snapshots)];
}

function fastSnapshotRestoreItems(
  root: Record<string, unknown>,
  field: string,
): ProviderFastSnapshotRestore[] {
  return items(record(root[field])["item"]).map((entry) => {
    const item = record(entry);
    const error = record(item["error"]);
    const state = asString(item["state"] ?? error["code"]);
    const stateTransitionReason = asString(item["stateTransitionReason"] ?? error["message"]);
    return {
      snapshotID: asString(item["snapshotId"] ?? item["sourceSnapshotId"]),
      availabilityZone: asString(item["availabilityZone"]),
      ...(state ? { state } : {}),
      ...(stateTransitionReason ? { stateTransitionReason } : {}),
    };
  });
}

function tagValue(resource: Record<string, unknown>, key: string): string {
  for (const tag of items(record(resource["tagSet"])["item"])) {
    const item = record(tag);
    if (asString(item["key"]) === key) return asString(item["value"]);
  }
  return "";
}

function isRetryableSnapshotDeleteError(message: string): boolean {
  return (
    message.includes("InvalidSnapshot.InUse") ||
    message.includes("RequestLimitExceeded") ||
    message.includes("Throttl") ||
    message.includes("ServiceUnavailable") ||
    message.includes("InternalError") ||
    message.includes("http 5") ||
    message.toLowerCase().includes("currently in use")
  );
}

function asString(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return "";
}

function assignSSHIngressRange(
  params: Record<string, string>,
  cidr: string,
  describe: boolean,
): void {
  const family = sshIngressRangeFamily(ingressFamily(cidr));
  params[family.param] = cidr;
  if (describe) {
    params[family.descriptionParam] = awsSSHIngressDescription;
  }
}

function assignSSHIngressRule(params: Record<string, string>, rule: SSHIngressRule): void {
  params[sshIngressRangeFamily(rule.family).param] = rule.cidr;
}

function ingressFamily(cidr: string): SSHIngressRule["family"] {
  return cidr.includes(":") ? "ipv6" : "ipv4";
}

function sshIngressRangeFamily(family: SSHIngressRule["family"]) {
  return family === "ipv6" ? sshIngressRangeFamilies[1] : sshIngressRangeFamilies[0];
}

export function crabboxSSHIngressRules(group: unknown, ports: string[]): SSHIngressRule[] {
  return sshIngressRules(group, ports)
    .filter((rule) => rule.description === awsSSHIngressDescription)
    .map(stripSSHIngressRuleDescription);
}

export function reclaimableCrabboxSSHIngressRules(
  group: unknown,
  ports: string[],
  cidrs: string[],
): SSHIngressRule[] {
  const desired = new Set(cidrs.map((cidr) => cidr.trim()).filter(Boolean));
  const includeLegacyUnlabeled = isCrabboxManagedSecurityGroup(group);
  return sshIngressRules(group, ports)
    .filter((rule) => {
      if (desired.has(rule.cidr)) {
        return false;
      }
      return (
        rule.description === awsSSHIngressDescription ||
        (includeLegacyUnlabeled && rule.description === "")
      );
    })
    .map(stripSSHIngressRuleDescription);
}

function sshIngressRules(group: unknown, ports: string[]): DescribedSSHIngressRule[] {
  const wantedPorts = new Set(ports);
  const rules: DescribedSSHIngressRule[] = [];
  for (const permission of items(record(record(group)["ipPermissions"])["item"])) {
    const entry = record(permission);
    const protocol = asString(entry["ipProtocol"]);
    const fromPort = asString(entry["fromPort"]);
    const toPort = asString(entry["toPort"]);
    if (protocol !== "tcp" || fromPort !== toPort || !wantedPorts.has(fromPort)) {
      continue;
    }
    for (const family of sshIngressRangeFamilies) {
      for (const range of items(record(entry[family.rangesField])["item"])) {
        const value = record(range);
        rules.push({
          cidr: asString(value[family.cidrField]),
          description: asString(value["description"]),
          family: family.family,
          port: fromPort,
        });
      }
    }
  }
  return rules.filter((rule) => rule.cidr);
}

function stripSSHIngressRuleDescription(rule: DescribedSSHIngressRule): SSHIngressRule {
  return {
    cidr: rule.cidr,
    family: rule.family,
    port: rule.port,
  };
}

export function staleCrabboxSSHIngressRules(
  group: unknown,
  ports: string[],
  cidrs: string[],
): SSHIngressRule[] {
  const desired = new Set(cidrs.map((cidr) => cidr.trim()).filter(Boolean));
  return crabboxSSHIngressRules(group, ports).filter((rule) => !desired.has(rule.cidr));
}

function isCrabboxManagedSecurityGroup(group: unknown): boolean {
  const entry = record(group);
  const tags = tagMap(entry["tagSet"]);
  return (
    asString(entry["groupName"]) === "crabbox-runners" ||
    tags["crabbox"] === "true" ||
    tags["created_by"] === "crabbox"
  );
}

function isAWSSecurityGroupRuleLimitError(message: string): boolean {
  return message.includes("RulesPerSecurityGroupLimitExceeded");
}

function isAWSInsufficientCapacityOnHostError(message: string): boolean {
  return message.includes("InsufficientCapacityOnHost");
}

export function awsLaunchCandidates(
  config: Pick<
    LeaseConfig,
    "serverType" | "serverTypeExplicit" | "class" | "target" | "windowsMode" | "architecture"
  >,
): string[] {
  if (config.serverTypeExplicit) {
    return [config.serverType];
  }
  if (config.target === "macos") {
    return uniqueStrings([
      config.serverType,
      ...awsInstanceTypeCandidatesForTargetClass(
        config.target,
        config.class,
        config.windowsMode,
        config.architecture,
      ),
    ]);
  }
  const policyFallback =
    config.target === "windows"
      ? config.windowsMode === "wsl2"
        ? "m8i.large"
        : "t3.large"
      : config.architecture === "arm64"
        ? "t4g.small"
        : "t3.small";
  return uniqueStrings([
    config.serverType,
    ...awsInstanceTypeCandidatesForTargetClass(
      config.target,
      config.class,
      config.windowsMode,
      config.architecture,
    ),
    policyFallback,
  ]);
}

export function awsRegionCandidates(
  config: Pick<LeaseConfig, "awsRegion" | "capacityRegions"> &
    Partial<Pick<LeaseConfig, "target" | "hostID" | "awsMacHostID" | "awsAMI" | "awsSnapshot">>,
  env: Pick<Env, "CRABBOX_AWS_REGION" | "CRABBOX_CAPACITY_REGIONS">,
  preferredRegion = "eu-west-1",
): string[] {
  if (
    config.target === "macos" &&
    (config.hostID || config.awsMacHostID || config.awsAMI || config.awsSnapshot)
  ) {
    return uniqueStrings([config.awsRegion || preferredRegion || env.CRABBOX_AWS_REGION || ""]);
  }
  return uniqueStrings([
    preferredRegion,
    config.awsRegion,
    env.CRABBOX_AWS_REGION ?? "",
    ...splitCommaList(env.CRABBOX_CAPACITY_REGIONS ?? ""),
    ...config.capacityRegions,
  ]);
}

export function awsAvailabilityZoneForRegion(
  config: Pick<LeaseConfig, "capacityAvailabilityZones">,
  env: Pick<Env, "CRABBOX_CAPACITY_AVAILABILITY_ZONES">,
  region: string,
): string {
  return (
    uniqueStrings([
      ...config.capacityAvailabilityZones,
      ...splitCommaList(env.CRABBOX_CAPACITY_AVAILABILITY_ZONES ?? ""),
    ]).find((zone) => zone.startsWith(region)) ?? ""
  );
}

export function applyAWSRunInstanceTargetOptions(
  params: Record<string, string>,
  config: Pick<LeaseConfig, "target" | "windowsMode">,
): void {
  if (config.target === "windows" && config.windowsMode === "wsl2") {
    params["CpuOptions.NestedVirtualization"] = "enabled";
  }
}

export function awsQuotaCodeForMarket(market: string): string {
  return market === "on-demand" ? awsOnDemandQuotaCode : awsSpotQuotaCode;
}

function awsMacHostFamily(serverType: string): string {
  return serverType.split(".")[0]?.toLowerCase() ?? "";
}

function awsMacOSAMIQuery(serverType: string): {
  name: string;
  architecture: "arm64_mac" | "x86_64_mac";
} {
  if (serverType.startsWith("mac1.")) {
    return { name: "amzn-ec2-macos-14.*", architecture: "x86_64_mac" };
  }
  if (serverType.startsWith("mac-m")) {
    return { name: "amzn-ec2-macos-15.*-arm64", architecture: "arm64_mac" };
  }
  return { name: "amzn-ec2-macos-14.*-arm64", architecture: "arm64_mac" };
}

function awsServiceQuotaFromRecord(value: unknown): AWSServiceQuota | undefined {
  const quota = record(value);
  const quotaCode = asString(quota["QuotaCode"]);
  const quotaName = asString(quota["QuotaName"]);
  if (!quotaCode || !quotaName) {
    return undefined;
  }
  const out: AWSServiceQuota = { quotaCode, quotaName };
  const serviceCode = asString(quota["ServiceCode"]);
  const valueNumber = finiteNumber(quota["Value"]);
  const unit = asString(quota["Unit"]);
  if (serviceCode) {
    out.serviceCode = serviceCode;
  }
  if (valueNumber !== undefined) {
    out.value = valueNumber;
  }
  if (typeof quota["Adjustable"] === "boolean") {
    out.adjustable = quota["Adjustable"];
  }
  if (typeof quota["GlobalQuota"] === "boolean") {
    out.globalQuota = quota["GlobalQuota"];
  }
  if (unit) {
    out.unit = unit;
  }
  return out;
}

export function awsInstanceTypeVCPUs(serverType: string): number | undefined {
  const match = /\.([0-9]+)xlarge$/.exec(serverType);
  if (match?.[1]) {
    return Number.parseInt(match[1], 10) * 4;
  }
  if (serverType.endsWith(".xlarge")) {
    return 4;
  }
  if (/\.(nano|micro|small|medium|large)$/.test(serverType)) {
    return 2;
  }
  return undefined;
}

export function awsQuotaPreflightAttempt(
  serverType: string,
  market: string,
  region: string,
  quotaValue: number | undefined,
): ProvisioningAttempt | undefined {
  const needed = awsInstanceTypeVCPUs(serverType);
  if (!needed || quotaValue === undefined || quotaValue >= needed) {
    return undefined;
  }
  const quotaCode = awsQuotaCodeForMarket(market);
  return {
    region,
    serverType,
    market,
    category: "quota",
    message: `quota ${quotaCode} in ${region} is ${quotaValue} vCPUs; ${serverType} needs ${needed} vCPUs`,
  };
}

type AWSCapacityReadinessConfig = Pick<
  LeaseConfig,
  | "target"
  | "windowsMode"
  | "architecture"
  | "class"
  | "serverType"
  | "capacityMarket"
  | "capacityFallback"
>;

export function awsCapacityReadinessCheckForQuota(
  config: AWSCapacityReadinessConfig,
  market: string,
  region: string,
  quotaValue: number | undefined,
): AWSCapacityReadinessCheck {
  const quotaCode = awsQuotaCodeForMarket(market);
  const needed = awsInstanceTypeVCPUs(config.serverType);
  const details: Record<string, string> = {
    provider: "aws",
    market,
    region,
    quota_code: quotaCode,
    default_class: config.class,
    default_type: config.serverType,
    default_needed_vcpus: String(needed ?? 0),
  };
  if (quotaValue === undefined) {
    details["hint"] = "servicequotas_unavailable_or_forbidden";
    return {
      status: "skip",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=unknown", details),
      details,
    };
  }
  details["limit_vcpus"] = String(Math.trunc(quotaValue));
  if (!needed) {
    details["hint"] = "unknown_instance_vcpus";
    return {
      status: "skip",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=unknown", details),
      details,
    };
  }
  if (quotaValue < needed) {
    const recommendation = awsRecommendedClassForQuota(config, Math.trunc(quotaValue));
    if (recommendation) {
      details["recommended_class"] = recommendation.machineClass;
      details["recommended_type"] = recommendation.serverType;
    }
    details["hint"] = "lower_class_or_request_quota";
    return {
      status: "warning",
      check: "capacity",
      message: awsCapacityReadinessMessage("provider=aws capacity=quota_pressure", details),
      details,
    };
  }
  details["hint"] = "quota_satisfies_default_class";
  return {
    status: "ok",
    check: "capacity",
    message: awsCapacityReadinessMessage("provider=aws capacity=ready", details),
    details,
  };
}

function awsCapacityReadinessMarkets(config: AWSCapacityReadinessConfig): string[] {
  const markets = [config.capacityMarket || "spot"];
  if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
    markets.push("on-demand");
  }
  return uniqueStrings(markets);
}

function awsCapacityReadinessMessage(prefix: string, details: Record<string, string>): string {
  const suffix = Object.entries(details)
    .filter(([key, value]) => key !== "provider" && value)
    .toSorted(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => `${key}=${value.replaceAll(" ", "_")}`)
    .join(" ");
  return suffix ? `${prefix} ${suffix}` : prefix;
}

function awsRecommendedClassForQuota(
  config: Pick<LeaseConfig, "target" | "windowsMode" | "architecture">,
  limitVCPUs: number,
): { machineClass: string; serverType: string } | undefined {
  if (limitVCPUs <= 0) {
    return undefined;
  }
  for (const machineClass of ["beast", "large", "fast", "standard"]) {
    const [serverType] = awsInstanceTypeCandidatesForTargetClass(
      config.target,
      machineClass,
      config.windowsMode,
      config.architecture,
    );
    if (serverType && (awsInstanceTypeVCPUs(serverType) ?? 0) <= limitVCPUs) {
      return { machineClass, serverType };
    }
  }
  for (const serverType of awsInstanceTypeCandidatesForTargetClass(
    config.target,
    "standard",
    config.windowsMode,
    config.architecture,
  )) {
    if ((awsInstanceTypeVCPUs(serverType) ?? 0) <= limitVCPUs) {
      return { machineClass: "standard", serverType };
    }
  }
  return undefined;
}

function uniqueStrings(values: string[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const value of values) {
    const normalized = value.trim();
    if (normalized && !seen.has(normalized)) {
      seen.add(normalized);
      out.push(normalized);
    }
  }
  return out;
}

function splitCommaList(value: string): string[] {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function positiveInt(value: string | undefined): number {
  if (!value) {
    return 0;
  }
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

function positiveFloat(value: string): number | undefined {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function positiveNumber(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : Number.parseFloat(String(value));
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function finiteNumber(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : Number.parseFloat(String(value));
  return Number.isFinite(parsed) ? parsed : undefined;
}

const opaqueAWSHTTP400XMLDeclaration = '<?xml version="1.0" encoding="UTF-8"?>';

export function awsProvisioningErrorCategory(message: string): string {
  if (message.includes("no available EC2 Mac Dedicated Host")) {
    return "capacity";
  }
  if (message.includes("no AWS AMI found")) {
    return "region";
  }
  if (
    message.includes("InsufficientInstanceCapacity") ||
    isAWSInsufficientCapacityOnHostError(message)
  ) {
    return "capacity";
  }
  if (message.includes("MaxSpotInstanceCountExceeded") || message.includes("VcpuLimitExceeded")) {
    return "quota";
  }
  if (message.includes("Unsupported") || message.includes("InvalidParameterValue")) {
    return "unsupported";
  }
  if (
    message.includes("InvalidParameterCombination") &&
    (message.includes("Free Tier") ||
      message.includes("eligible") ||
      message.includes("InstanceType") ||
      message.includes("instance type"))
  ) {
    return "policy";
  }
  if (isOpaqueAWSHTTP400XMLOnlyError(message)) {
    return "capacity";
  }
  return "";
}

function isOpaqueAWSHTTP400XMLOnlyError(message: string): boolean {
  const entries = message
    .split(";")
    .map((entry) => entry.trim())
    .filter(Boolean);
  return entries.length > 0 && entries.every(isOpaqueAWSHTTP400XMLEntry);
}

function isOpaqueAWSHTTP400XMLEntry(entry: string): boolean {
  const awsIndex = entry.indexOf("aws ");
  if (awsIndex < 0) {
    return false;
  }
  if (awsIndex > 0 && !entry.slice(0, awsIndex).trimEnd().endsWith(":")) {
    return false;
  }

  const payload = entry.slice(awsIndex);
  const separator = ": http 400: ";
  const separatorIndex = payload.indexOf(separator);
  if (separatorIndex <= "aws ".length) {
    return false;
  }
  const operation = payload.slice("aws ".length, separatorIndex);
  if (!isAWSOperationName(operation)) {
    return false;
  }
  return payload.slice(separatorIndex + separator.length).trim() === opaqueAWSHTTP400XMLDeclaration;
}

function isAWSOperationName(value: string): boolean {
  if (!value) {
    return false;
  }
  for (const char of value) {
    const code = char.charCodeAt(0);
    const isUpper = code >= 65 && code <= 90;
    const isLower = code >= 97 && code <= 122;
    if (!isUpper && !isLower) {
      return false;
    }
  }
  return true;
}

export function isRetryableAWSProvisioningError(message: string): boolean {
  return awsProvisioningErrorCategory(message) !== "";
}

export function isAWSInstanceNotFoundError(message: string): boolean {
  return message.includes("InvalidInstanceID.NotFound");
}

export function isAWSInvalidHostIDError(message: string): boolean {
  return message.includes("InvalidHostID.NotFound");
}

export function isAWSInstanceCleanedAfterReadinessFailure(
  waitMessage: string,
  cleanupMessage: string,
): boolean {
  if (cleanupMessage === "") {
    return true;
  }
  return isAWSInstanceNotFoundError(waitMessage) && isAWSInstanceNotFoundError(cleanupMessage);
}

function trimBody(text: string): string {
  return text.length > 500 ? `${text.slice(0, 500)}...` : text;
}

function conciseAWSProvisioningMessage(message: string): string {
  const code = /<Code>([^<]+)<\/Code>/.exec(message)?.[1] ?? "";
  const detail = /<Message>([^<]+)<\/Message>/.exec(message)?.[1] ?? "";
  if (code && detail) {
    return `${code}: ${detail}`;
  }
  return trimBody(message).replace(/\s+/g, " ");
}

function conciseAWSMacHostDryRunMessage(message: string): string {
  const code = /<Code>([^<]+)<\/Code>/.exec(message)?.[1] ?? "";
  if (code === "DryRunOperation" || message.includes("DryRunOperation")) {
    return "DryRunOperation: request would have succeeded";
  }
  if (code === "UnauthorizedOperation" || message.includes("UnauthorizedOperation")) {
    return "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags";
  }
  if (code) {
    return code;
  }
  if (message.includes("Encoded authorization failure") || message.includes("arn:aws:iam::")) {
    return "AWS authorization failure: details omitted";
  }
  return trimBody(message).replace(/\s+/g, " ");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
