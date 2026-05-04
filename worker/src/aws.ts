import { AwsClient } from "aws4fetch";
import { XMLParser } from "fast-xml-parser";

import { awsUserData } from "./bootstrap";
import {
  awsInstanceTypeCandidatesForTargetClass,
  sshPorts,
  validCIDRs,
  type LeaseConfig,
} from "./config";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type { Env, ProviderImage, ProviderMachine, ProvisioningAttempt } from "./types";

const awsUbuntuOwner = "099720109477";
const ec2Version = "2016-11-15";
const awsSpotQuotaCode = "L-34B43A08";
const awsOnDemandQuotaCode = "L-1216C47A";

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

export class EC2SpotClient {
  private readonly aws: AwsClient;
  private readonly serviceQuotas: AwsClient;
  private readonly endpoint: string;
  private readonly serviceQuotasEndpoint: string;
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
      items(record(record(reservation)["instancesSet"])["item"]).map(instanceToMachine),
    );
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{ server: ProviderMachine; serverType: string; attempts?: ProvisioningAttempt[] }> {
    await this.ensureSSHKey(config.providerKey, config.sshPublicKey);
    const imageID = await this.resolveAMI(config);
    const securityGroupID = await this.ensureSecurityGroup(config);
    const candidates = awsLaunchCandidates(config);
    const failures: string[] = [];
    const attempts: ProvisioningAttempt[] = [];
    const quotaCache = new Map<string, number | undefined>();
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
          attempts?: ProvisioningAttempt[];
        } = { server, serverType };
        if (attempts.length > 0) {
          result.attempts = attempts;
        }
        return result;
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        attempts.push({
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
            attempts?: ProvisioningAttempt[];
          } = { server, serverType };
          if (attempts.length > 0) {
            result.attempts = attempts;
          }
          return result;
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
          attempts.push({
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
  }

  async getServer(instanceID: string): Promise<ProviderMachine> {
    const root = await this.ec2("DescribeInstances", {
      "InstanceId.1": instanceID,
    });
    for (const reservation of reservations(root)) {
      for (const instance of items(record(record(reservation)["instancesSet"])["item"])) {
        return instanceToMachine(instance);
      }
    }
    throw new Error(`aws instance not found: ${instanceID}`);
  }

  async waitForServerIP(instanceID: string): Promise<ProviderMachine> {
    const deadline = Date.now() + 600_000;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling waits between EC2 reads.
      const server = await this.getServer(instanceID);
      if (server.host) {
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
    return { id: imageID, name, state: "pending", region: this.region };
  }

  async getImage(imageID: string): Promise<ProviderImage> {
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
      region: this.region,
    };
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
    const params: Record<string, string> = {
      ClientToken: leaseID,
      ImageId: imageID,
      InstanceType: config.serverType,
      KeyName: config.providerKey,
      MaxCount: "1",
      MinCount: "1",
      UserData: btoa(awsUserData(config)),
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
      const hostID = config.awsMacHostID || this.env.CRABBOX_AWS_MAC_HOST_ID || "";
      if (!hostID) {
        throw new Error("aws target=macos requires CRABBOX_AWS_MAC_HOST_ID");
      }
      params["Placement.HostId"] = hostID;
      params["Placement.Tenancy"] = "host";
    }
    addRunInstancesTagSpecifications(params, { ...labels, Name: name }, config.capacityMarket);
    const root = await this.ec2("RunInstances", params);
    const instance = items(record(root["instancesSet"])["item"])[0];
    if (!instance) {
      throw new Error("aws returned no instances");
    }
    return instanceToMachine(instance);
  }

  private async resolveAMI(config: LeaseConfig): Promise<string> {
    if (config.awsAMI || this.env.CRABBOX_AWS_AMI) {
      return config.awsAMI || this.env.CRABBOX_AWS_AMI || "";
    }
    if (config.target === "windows") {
      return this.resolveLatestAmazonAMI("Windows_Server-2022-English-Full-Base-*", "x86_64");
    }
    if (config.target === "macos") {
      if (config.serverType.startsWith("mac1.")) {
        return this.resolveLatestAmazonAMI("amzn-ec2-macos-14.*", "x86_64_mac");
      }
      return this.resolveLatestAmazonAMI("amzn-ec2-macos-14.*-arm64", "arm64_mac");
    }
    const root = await this.ec2("DescribeImages", {
      "Owner.1": awsUbuntuOwner,
      "Filter.1.Name": "architecture",
      "Filter.1.Value.1": "x86_64",
      "Filter.2.Name": "name",
      "Filter.2.Value.1": "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",
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
      throw new Error(`no Ubuntu 24.04 x86_64 AMI found in ${this.region}`);
    }
    return imageID;
  }

  private async resolveLatestAmazonAMI(name: string, architecture: string): Promise<string> {
    const root = await this.ec2("DescribeImages", {
      "Owner.1": "amazon",
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
      throw new Error(
        `no AWS AMI found in ${this.region} for name=${name} architecture=${architecture}`,
      );
    }
    return imageID;
  }

  private async ensureSecurityGroup(config: LeaseConfig): Promise<string> {
    if (config.awsSGID || this.env.CRABBOX_AWS_SECURITY_GROUP_ID) {
      return config.awsSGID || this.env.CRABBOX_AWS_SECURITY_GROUP_ID || "";
    }
    const vpcID = await this.securityGroupVPC(config);
    const name = "crabbox-runners";
    const existing = await this.ec2("DescribeSecurityGroups", {
      "Filter.1.Name": "group-name",
      "Filter.1.Value.1": name,
      "Filter.2.Name": "vpc-id",
      "Filter.2.Value.1": vpcID,
    });
    const group = items(record(existing["securityGroupInfo"])["item"])[0];
    let groupID = asString(record(group)["groupId"]);
    if (!groupID) {
      const created = await this.ec2("CreateSecurityGroup", createSecurityGroupParams(name, vpcID));
      groupID = asString(record(created)["groupId"]);
    }
    if (!groupID) {
      throw new Error("aws security group id is empty");
    }
    const cidrs = awsSSHCIDRs(config, this.env);
    for (const port of sshPorts(config)) {
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
          if (!message.includes("InvalidPermission.Duplicate")) {
            throw error;
          }
        });
      }
    }
    return groupID;
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
    if (cidr.includes(":")) {
      params["IpPermissions.1.Ipv6Ranges.1.CidrIpv6"] = cidr;
      params["IpPermissions.1.Ipv6Ranges.1.Description"] = "Crabbox SSH";
    } else {
      params["IpPermissions.1.IpRanges.1.CidrIp"] = cidr;
      params["IpPermissions.1.IpRanges.1.Description"] = "Crabbox SSH";
    }
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
      throw new Error(`aws ${action}: http ${response.status}: ${trimBody(text)}`);
    }
    const parsed = this.parser.parse(text) as unknown;
    const parsedRecord = record(parsed);
    const root = parsedRecord[`${action}Response`] ?? parsedRecord["Response"] ?? parsedRecord;
    return record(root);
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
    host: asString(instance["ipAddress"]),
    labels: tags,
  };
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

function asString(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return "";
}

export function awsLaunchCandidates(
  config: Pick<
    LeaseConfig,
    "serverType" | "serverTypeExplicit" | "class" | "target" | "windowsMode"
  >,
): string[] {
  if (config.serverTypeExplicit) {
    return [config.serverType];
  }
  if (config.target === "macos") {
    return uniqueStrings([
      config.serverType,
      ...awsInstanceTypeCandidatesForTargetClass(config.target, config.class),
    ]);
  }
  const policyFallback =
    config.target === "windows"
      ? config.windowsMode === "wsl2"
        ? "m8i.large"
        : "t3.large"
      : "t3.small";
  return uniqueStrings([
    config.serverType,
    ...awsInstanceTypeCandidatesForTargetClass(config.target, config.class, config.windowsMode),
    policyFallback,
  ]);
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
    serverType,
    market,
    category: "quota",
    message: `quota ${quotaCode} in ${region} is ${quotaValue} vCPUs; ${serverType} needs ${needed} vCPUs`,
  };
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
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

export function awsProvisioningErrorCategory(message: string): string {
  if (message.includes("InsufficientInstanceCapacity")) {
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
  return "";
}

function isRetryableAWSProvisioningError(message: string): boolean {
  return awsProvisioningErrorCategory(message) !== "";
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

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
