import type { LeaseConfig } from "./config";
import { redactDiagnosticSecrets } from "./http";
import { sshPublicKeyIdentity } from "./provider-key";
import { providerLabelValue } from "./provider-labels";
import {
  ProviderResourceUnresolvedError,
  type FrozenProvisioningPlan,
  type ProviderResumableProvisioning,
  type ProvisioningStep,
} from "./provider-provisioning";
import { leaseProviderName } from "./slug";
import type {
  Env,
  LeaseImageIdentity,
  LeaseRecord,
  ProviderMachine,
  TailscaleMetadata,
} from "./types";

const defaultAPIURL = "https://app.koyeb.com";
const defaultRegion = "was";
const defaultInstanceType = "large";
const managementRoute = "/koyeb-sandbox";
const publicKeyPath = "/run/crabbox-authorized-key.pub";
const bootstrapCommand = "/usr/local/bin/crabbox-koyeb-bootstrap";
const workRoot = "/workspace/crabbox";
const imageKind = "koyeb-sandbox-runner";
const pollInterval = 2_000;
const uuidPattern = /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/i;
const immutableImagePattern =
  /^(?=.{1,512}$)[a-z0-9][a-z0-9._:-]*(?:\/[a-z0-9][a-z0-9._-]*)+(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[a-f0-9]{64}$/;

type JSONRecord = Record<string, unknown>;
type KoyebTransport = "tailscale" | "koyeb-mesh";

interface KoyebConfiguration {
  apiURL: string;
  token: string;
  organizationID: string;
  appID: string;
  region: string;
  instanceType: string;
  image: string;
  registrySecret?: string;
  appName?: string;
}

interface KoyebService {
  id: string;
  name: string;
  type: string;
  organizationID: string;
  appID: string;
  status: string;
  activeDeploymentID?: string;
  latestDeploymentID?: string;
  lifeCycle: JSONRecord;
}

interface KoyebDeployment {
  id: string;
  organizationID: string;
  appID: string;
  serviceID: string;
  status: string;
  definition: JSONRecord;
  metadata: JSONRecord;
}

interface KoyebProvisioningPlan {
  version: 1;
  serviceName: string;
  runnerLeaseID: string;
  organizationID: string;
  appID: string;
  region: string;
  instanceType: string;
  image: string;
  registrySecret?: string;
  leaseID: string;
  slug: string;
  owner: string;
  org: string;
  generation: string;
  ttlSeconds: number;
  idleTimeoutSeconds: number;
  sshPublicKey: string;
  transport: KoyebTransport;
  privateHost?: string;
  tailscaleHostname: string;
  tailscaleTags: string[];
}

type KoyebAction = "dispatch" | "discover" | "observe" | "bootstrap" | "delete" | "confirm-delete";

interface KoyebContinuationState {
  version: 1;
  action: KoyebAction;
  serviceID?: string;
  deploymentID?: string;
  outcomeUncertain?: boolean;
}

interface OwnedDeployment {
  service: KoyebService;
  deployment: KoyebDeployment;
  sandboxSecret: string;
  publicURL?: string;
  routingKey?: string;
}

interface RunnerReady {
  transport: KoyebTransport;
  host: string;
  ipv4?: string;
  fqdn?: string;
  user: string;
  port: string;
  hostKey: string;
}

type Observation =
  | { kind: "missing" }
  | { kind: "pending"; service: KoyebService }
  | { kind: "conflict" }
  | { kind: "owned"; value: OwnedDeployment };

export class KoyebHTTPError extends Error {
  override name = "KoyebHTTPError";

  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly detail = "",
  ) {
    super(`koyeb ${method} ${path}: http ${status}${detail ? `: ${detail}` : ""}`);
  }
}

export class KoyebClient {
  readonly apiURL: string;
  readonly organizationID: string;
  readonly appID: string;
  readonly region: string;
  readonly instanceType: string;
  readonly image: string;
  readonly registrySecret?: string;
  readonly appName?: string;
  private readonly token: string;

  constructor(
    env: Env,
    private readonly fetcher: typeof fetch = fetch,
  ) {
    const missing = koyebConfigurationMissing(env);
    if (missing.length) {
      throw new Error(`Koyeb coordinator configuration invalid or missing: ${missing.join(", ")}`);
    }
    const configured = koyebConfiguration(env);
    this.apiURL = configured.apiURL;
    this.token = configured.token;
    this.organizationID = configured.organizationID;
    this.appID = configured.appID;
    this.region = configured.region;
    this.instanceType = configured.instanceType;
    this.image = configured.image;
    if (configured.registrySecret) this.registrySecret = configured.registrySecret;
    if (configured.appName) this.appName = configured.appName;
  }

  async providerScope(): Promise<string> {
    const digest = await sha256Hex(
      JSON.stringify([
        "crabbox-koyeb-context-v1",
        this.apiURL,
        this.organizationID,
        this.appID,
        this.token,
      ]),
    );
    return `koyeb:context:v1:${digest}`;
  }

  async listServices(name?: string): Promise<KoyebService[]> {
    const query = new URLSearchParams({ app_id: this.appID, limit: "100" });
    query.append("types", "SANDBOX");
    if (name) query.set("name", name);
    const value = asObject(await this.apiRequest("GET", `/v1/services?${query.toString()}`));
    if (value["has_next"] === true) throw new Error("koyeb service inventory is truncated");
    const services = value["services"];
    if (!Array.isArray(services)) throw new Error("koyeb service inventory is malformed");
    return services.map(koyebService);
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const services = await this.listServices();
    const machines = await Promise.all(
      services.map(async (service): Promise<ProviderMachine | undefined> => {
        if (
          !uuidPattern.test(service.id) ||
          service.type !== "SANDBOX" ||
          service.organizationID !== this.organizationID ||
          service.appID !== this.appID
        ) {
          return undefined;
        }
        const deploymentID = service.activeDeploymentID || service.latestDeploymentID;
        if (!deploymentID || !uuidPattern.test(deploymentID)) return undefined;
        const deployment = await this.getDeployment(deploymentID);
        return deployment ? inventoryMachine(this, service, deployment) : undefined;
      }),
    );
    return machines.filter((machine): machine is ProviderMachine => machine !== undefined);
  }

  async getService(id: string): Promise<KoyebService | undefined> {
    requireUUID(id, "service id");
    const result = await this.apiRequest("GET", `/v1/services/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    if (result === undefined) return undefined;
    return koyebService(asObject(result)["service"]);
  }

  async getDeployment(id: string): Promise<KoyebDeployment | undefined> {
    requireUUID(id, "deployment id");
    const result = await this.apiRequest("GET", `/v1/deployments/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    if (result === undefined) return undefined;
    return koyebDeployment(asObject(result)["deployment"]);
  }

  async createService(body: JSONRecord): Promise<KoyebService> {
    const result = asObject(await this.apiRequest("POST", "/v1/services", { body }));
    return koyebService(result["service"]);
  }

  async deleteService(id: string): Promise<boolean> {
    requireUUID(id, "service id");
    const result = await this.apiRequest("DELETE", `/v1/services/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    return result !== undefined;
  }

  privateHost(serviceName: string): string | undefined {
    return this.appName ? `${serviceName}.${this.appName}.internal` : undefined;
  }

  async managementHealth(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
  ): Promise<void> {
    await this.managementRequest(baseURL, routingKey, secret, "/health", "GET");
  }

  async managementWriteFile(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    path: string,
    content: string,
  ): Promise<void> {
    await this.managementRequest(baseURL, routingKey, secret, "/write_file", "POST", {
      path,
      content,
    });
  }

  async managementRun(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    body: { cmd: string; cwd?: string; env?: Record<string, string> },
    protectedValues: readonly string[] = [],
  ): Promise<{ stdout: string; stderr: string; code: number }> {
    const value = asObject(
      await this.managementRequest(baseURL, routingKey, secret, "/run", "POST", body, [
        ...protectedValues,
      ]),
    );
    const stdout = stringValue(value["stdout"]);
    const stderr = stringValue(value["stderr"]);
    const code = value["code"];
    if (!Number.isSafeInteger(code)) throw new Error("koyeb sandbox run response is malformed");
    return { stdout, stderr, code: Number(code) };
  }

  async managementBindPort(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    port: string,
  ): Promise<void> {
    let value: JSONRecord;
    try {
      value = asObject(
        await this.managementRequest(baseURL, routingKey, secret, "/bind_port", "POST", { port }),
      );
    } catch (error) {
      if (error instanceof KoyebHTTPError && error.status === 409) {
        try {
          const conflict = asObject(JSON.parse(error.detail));
          if (conflict["success"] === false && conflict["current_port"] === port) return;
        } catch {
          // Preserve the original authenticated-management error below.
        }
      }
      throw error;
    }
    if (value["success"] !== true || value["port"] !== port) {
      throw new Error("koyeb sandbox bind-port response is malformed");
    }
  }

  async ownedServiceForLease(lease: LeaseRecord): Promise<OwnedDeployment | undefined> {
    const scope = await this.providerScope();
    if (
      lease.provider !== "koyeb" ||
      !uuidPattern.test(lease.cloudID) ||
      lease.providerScope !== scope ||
      !lease.createAttemptGeneration
    ) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb lease cleanup identity is incomplete or belongs to another context",
      );
    }
    const service = await this.getService(lease.cloudID);
    if (!service) return undefined;
    const plan = planForLeaseCleanup(this, lease);
    const observation = await observeService(this, plan, service);
    if (observation.kind === "missing") return undefined;
    if (observation.kind !== "owned") {
      throw new ProviderResourceUnresolvedError("Koyeb lease cleanup ownership is not proven");
    }
    return observation.value;
  }

  async deleteOwnedService(lease: LeaseRecord): Promise<void> {
    const owned = await this.ownedServiceForLease(lease);
    if (!owned) return;
    await this.deleteService(owned.service.id);
    const observed = await this.getService(owned.service.id);
    if (observed && observed.status !== "DELETED") {
      throw new Error("Koyeb service deletion is not yet confirmed");
    }
  }

  private async apiRequest(
    method: string,
    path: string,
    options: { body?: unknown; allowNotFound?: boolean } = {},
  ): Promise<unknown | undefined> {
    const headers = new Headers({ authorization: `Bearer ${this.token}` });
    if (options.body !== undefined) headers.set("content-type", "application/json");
    const request = new Request(`${this.apiURL}${path}`, {
      method,
      headers,
      redirect: "error",
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
    });
    return this.request(request, method, path, [this.token], options.allowNotFound === true);
  }

  private async managementRequest(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    path: string,
    method: string,
    body?: unknown,
    protectedValues: readonly string[] = [],
  ): Promise<unknown> {
    const base = managementBaseURL(baseURL, routingKey !== undefined);
    if (routingKey !== undefined && !validRoutingKey(routingKey)) {
      throw new Error("koyeb sandbox routing key is malformed");
    }
    if (!/^[A-Za-z0-9_-]{32,128}$/.test(secret)) {
      throw new Error("koyeb sandbox credential is malformed");
    }
    const headers = new Headers({ authorization: `Bearer ${secret}` });
    if (routingKey !== undefined) headers.set("x-routing-key", routingKey);
    if (body !== undefined) headers.set("content-type", "application/json");
    const request = new Request(`${base}${path}`, {
      method,
      headers,
      redirect: "error",
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    return (await this.request(
      request,
      method,
      `${managementRoute}${path}`,
      [this.token, secret, ...(routingKey ? [routingKey] : []), ...protectedValues],
      false,
    ))!;
  }

  private async request(
    request: Request,
    method: string,
    path: string,
    secrets: readonly string[],
    allowNotFound: boolean,
  ): Promise<unknown | undefined> {
    let response: Response;
    try {
      response = await this.fetcher(request);
    } catch (error) {
      throw new KoyebHTTPError(
        method,
        path,
        0,
        redactDiagnosticSecrets(error instanceof Error ? error.message : String(error), secrets),
      );
    }
    const body = await response.text();
    if (allowNotFound && response.status === 404) return undefined;
    if (!response.ok) {
      throw new KoyebHTTPError(
        method,
        path,
        response.status,
        redactDiagnosticSecrets(body.slice(0, 2048), secrets),
      );
    }
    if (!body.trim()) return {};
    try {
      return JSON.parse(body) as unknown;
    } catch {
      throw new KoyebHTTPError(method, path, response.status, "invalid JSON response");
    }
  }
}

function retryableManagementFailure(error: unknown): error is KoyebHTTPError {
  return (
    error instanceof KoyebHTTPError &&
    (error.status === 0 ||
      error.status === 408 ||
      error.status === 425 ||
      error.status === 429 ||
      error.status >= 500)
  );
}

function managementFailureCategory(error: KoyebHTTPError): string {
  if (error.status === 0) return "transport_unavailable";
  if (error.status === 408) return "request_timeout";
  if (error.status === 425) return "service_not_ready";
  if (error.status === 429) return "rate_limited";
  return "server_unavailable";
}

function managementBlockedReason(error: unknown): string {
  if (!(error instanceof KoyebHTTPError)) return "koyeb_management_response_invalid";
  if (error.status === 401 || error.status === 403) return "koyeb_management_unauthorized";
  if (error.status === 404) return "koyeb_management_route_missing";
  return "koyeb_management_rejected";
}

export class KoyebResumableProvisioning implements ProviderResumableProvisioning {
  readonly version = 1 as const;
  private readonly client: KoyebClient;

  constructor(env: Env, fetcher: typeof fetch = fetch) {
    this.client = new KoyebClient(env, fetcher);
  }

  supports(config: LeaseConfig): boolean {
    return (
      config.provider === "koyeb" &&
      config.target === "linux" &&
      config.architecture === "amd64" &&
      (config.tailscale || Boolean(this.client.privateHost("crabbox"))) &&
      !config.tailscaleExitNode &&
      config.serverType === this.client.instanceType &&
      config.workRoot === workRoot
    );
  }

  async prepare(
    config: LeaseConfig,
    lease: LeaseRecord,
  ): Promise<{
    plan: FrozenProvisioningPlan;
    material: { adminPassword: string; bootstrap: string; providerSecret: string };
    step: ProvisioningStep;
  }> {
    if (!this.supports(config))
      throw new Error("Koyeb durable provisioning configuration unsupported");
    if (!lease.createAttemptGeneration) {
      throw new Error("Koyeb provisioning requires an immutable attempt generation");
    }
    if (config.tailscale && (!config.tailscaleAuthKey || config.tailscaleAuthKey.length > 4096)) {
      throw new Error("Koyeb provisioning requires a bounded one-off Tailscale auth key");
    }
    const scope = await this.client.providerScope();
    const serviceName = leaseProviderName(lease.id, lease.slug);
    const transport: KoyebTransport = config.tailscale ? "tailscale" : "koyeb-mesh";
    const privateHost =
      transport === "koyeb-mesh" ? this.client.privateHost(serviceName) : undefined;
    if (transport === "koyeb-mesh" && !privateHost) {
      throw new Error("Koyeb private-mesh identity is unavailable");
    }
    const data: KoyebProvisioningPlan = {
      version: 1,
      serviceName,
      runnerLeaseID: serviceName,
      organizationID: this.client.organizationID,
      appID: this.client.appID,
      region: this.client.region,
      instanceType: this.client.instanceType,
      image: this.client.image,
      ...(this.client.registrySecret ? { registrySecret: this.client.registrySecret } : {}),
      leaseID: lease.id,
      slug: lease.slug ?? "",
      owner: lease.providerOwner || lease.owner,
      org: lease.org,
      generation: lease.createAttemptGeneration,
      ttlSeconds: lease.ttlSeconds,
      idleTimeoutSeconds: lease.idleTimeoutSeconds ?? lease.ttlSeconds,
      sshPublicKey: config.sshPublicKey,
      transport,
      ...(privateHost ? { privateHost } : {}),
      tailscaleHostname: config.tailscaleHostname,
      tailscaleTags: [...config.tailscaleTags],
    };
    return {
      plan: {
        version: 1,
        provider: "koyeb",
        scope,
        resources: [{ cloudID: serviceName, region: this.client.region, scope }],
        data,
      },
      material: {
        adminPassword: randomSecret(),
        bootstrap: transport === "tailscale" ? config.tailscaleAuthKey : "",
        providerSecret: randomSecret(),
      },
      step: {
        phase: "prepared",
        attempt: 0,
        state: { version: 1, action: "dispatch" } satisfies KoyebContinuationState,
        nextWake: Date.now(),
      },
    };
  }

  async advance(
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
  ): Promise<ProvisioningStep> {
    const plan = await validatedPlan(this.client, input.plan, input.lease);
    const state = continuationState(input.step.state);
    if (
      input.canceled ||
      input.step.phase === "cleanup" ||
      input.step.phase === "settling" ||
      input.step.phase === "retained"
    ) {
      return this.cleanup(input, plan, state);
    }
    if (
      !input.material.providerSecret ||
      (plan.transport === "tailscale" && !input.material.bootstrap)
    ) {
      throw new Error("Koyeb forward provisioning material is unavailable");
    }
    const output = (phase: ProvisioningStep["phase"], delay = pollInterval): ProvisioningStep => ({
      phase,
      attempt: 0,
      state,
      nextWake: Date.now() + delay,
    });

    if (state.action === "dispatch" || state.action === "discover") {
      const discovery = await discoverService(this.client, plan, input.material.providerSecret);
      if (discovery.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      if (discovery.kind === "owned" || discovery.kind === "pending") {
        state.action = "observe";
        state.serviceID = discovery.service.id;
        delete state.outcomeUncertain;
        return output("provisioning", 1);
      }
      if (state.action === "discover" || input.recovering) {
        state.action = "discover";
        state.outcomeUncertain = true;
        return { ...output("blocked"), blockedReason: "dispatch_outcome_unresolved" };
      }
      try {
        const created = await this.client.createService(
          createServiceRequest(plan, input.material.providerSecret),
        );
        if (!serviceMatchesPlan(created, plan) || !uuidPattern.test(created.id)) {
          state.action = "discover";
          state.outcomeUncertain = true;
          return { ...output("blocked"), blockedReason: "dispatch_identity_unresolved" };
        }
        state.action = "observe";
        state.serviceID = created.id;
        return output("provisioning", 1);
      } catch {
        state.action = "discover";
        state.outcomeUncertain = true;
        return output("provisioning");
      }
    }

    if (!state.serviceID || !uuidPattern.test(state.serviceID)) {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (state.action === "observe") {
      const observation = await observeByID(
        this.client,
        plan,
        state.serviceID,
        input.material.providerSecret,
      );
      if (observation.kind === "missing") {
        return { ...output("blocked"), blockedReason: "observed_resource_missing" };
      }
      if (observation.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      if (observation.kind === "pending") return output("provisioning");
      if (serviceTerminal(observation.value.service.status)) {
        return { ...output("blocked"), blockedReason: "service_terminal" };
      }
      if (deploymentTerminal(observation.value.deployment.status)) {
        return { ...output("blocked"), blockedReason: "deployment_terminal" };
      }
      if (
        observation.value.service.status !== "HEALTHY" ||
        observation.value.deployment.status !== "HEALTHY" ||
        !managementTarget(observation.value, plan)
      ) {
        return output("provisioning");
      }
      state.action = "bootstrap";
      state.deploymentID = observation.value.deployment.id;
      return output("provisioning", 1);
    }

    if (state.action !== "bootstrap" || !state.deploymentID) {
      return { ...output("blocked"), blockedReason: "continuation_state_invalid" };
    }
    const observation = await observeByID(
      this.client,
      plan,
      state.serviceID,
      input.material.providerSecret,
    );
    if (observation.kind !== "owned") {
      return {
        ...output(observation.kind === "pending" ? "provisioning" : "blocked"),
        ...(observation.kind === "pending"
          ? {}
          : { blockedReason: "bootstrap_identity_unavailable" }),
      };
    }
    const owned = observation.value;
    const management = managementTarget(owned, plan);
    if (
      owned.deployment.id !== state.deploymentID ||
      owned.service.status !== "HEALTHY" ||
      owned.deployment.status !== "HEALTHY" ||
      !management
    ) {
      return { ...output("blocked"), blockedReason: "bootstrap_identity_unavailable" };
    }
    let run: { stdout: string; stderr: string; code: number };
    try {
      await this.client.managementHealth(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
      );
      await this.client.managementWriteFile(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
        publicKeyPath,
        plan.sshPublicKey,
      );
      const bootstrapEnvironment: Record<string, string> = {
        CRABBOX_KOYEB_LEASE_ID: plan.runnerLeaseID,
        CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE: publicKeyPath,
        ...(plan.transport === "tailscale"
          ? {
              CRABBOX_KOYEB_TAILSCALE_AUTH_KEY: input.material.bootstrap,
              CRABBOX_KOYEB_TAILSCALE_HOSTNAME: plan.tailscaleHostname,
              CRABBOX_KOYEB_TAILSCALE_TAGS: plan.tailscaleTags.join(","),
            }
          : {
              CRABBOX_KOYEB_NETWORK: "koyeb-mesh",
              CRABBOX_KOYEB_PRIVATE_HOST: plan.privateHost!,
            }),
      };
      run = await this.client.managementRun(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
        {
          cmd: bootstrapCommand,
          cwd: workRoot,
          env: bootstrapEnvironment,
        },
        plan.transport === "tailscale" ? [input.material.bootstrap] : [],
      );
    } catch (error) {
      if (retryableManagementFailure(error)) {
        console.warn("koyeb sandbox bootstrap deferred", {
          phase: "bootstrap",
          category: managementFailureCategory(error),
        });
        return output("provisioning");
      }
      const blockedReason = managementBlockedReason(error);
      console.error("koyeb sandbox bootstrap blocked", { phase: "bootstrap", blockedReason });
      return { ...output("blocked"), blockedReason };
    }
    if (run.code !== 0) {
      return { ...output("blocked"), blockedReason: "runner_bootstrap_failed" };
    }
    const ready = runnerReady(run.stdout, plan);
    if (!ready) {
      return { ...output("blocked"), blockedReason: "runner_readiness_invalid" };
    }
    if (plan.transport === "koyeb-mesh") {
      try {
        await this.client.managementBindPort(
          management.baseURL,
          management.routingKey,
          input.material.providerSecret,
          "22",
        );
      } catch (error) {
        if (retryableManagementFailure(error)) {
          console.warn("koyeb sandbox bootstrap deferred", {
            phase: "bind-port",
            category: managementFailureCategory(error),
          });
          return output("provisioning");
        }
        const blockedReason = managementBlockedReason(error);
        console.error("koyeb sandbox bootstrap blocked", { phase: "bind-port", blockedReason });
        return { ...output("blocked"), blockedReason };
      }
    }
    const tailscale: TailscaleMetadata | undefined =
      plan.transport === "tailscale"
        ? {
            enabled: true,
            hostname: plan.tailscaleHostname,
            ipv4: ready.ipv4!,
            ...(ready.fqdn ? { fqdn: ready.fqdn } : {}),
            tags: [...plan.tailscaleTags],
            state: "ready",
          }
        : undefined;
    const labels =
      plan.transport === "tailscale"
        ? {
            ...ownershipLabels(plan),
            tailscale: "true",
            tailscale_state: "ready",
            tailscale_hostname: plan.tailscaleHostname,
            tailscale_ipv4: ready.ipv4!,
            ...(ready.fqdn ? { tailscale_fqdn: ready.fqdn } : {}),
            tailscale_tags: plan.tailscaleTags.join(","),
          }
        : { ...ownershipLabels(plan), koyeb_network: "mesh" };
    const result = output("ready-to-publish", 1);
    result.publication = {
      server: machineForService(owned.service, plan, ready.host, labels),
      serverType: plan.instanceType,
      market: "on-demand",
      access: {
        sshUser: ready.user,
        sshPort: plan.transport === "koyeb-mesh" ? "3031" : ready.port,
        sshFallbackPorts: [],
        workRoot,
        sshHostKey: ready.hostKey,
        ...(tailscale ? { tailscale } : {}),
      },
      image: imageIdentity(plan),
    };
    return result;
  }

  private async cleanup(
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
    plan: KoyebProvisioningPlan,
    state: KoyebContinuationState,
  ): Promise<ProvisioningStep> {
    const output = (phase: ProvisioningStep["phase"], delay = pollInterval): ProvisioningStep => ({
      phase,
      attempt: 0,
      state,
      nextWake: Date.now() + delay,
    });
    if (state.action === "dispatch" && !input.recovering) return output("terminal", 1);
    if (state.action === "dispatch" || state.action === "discover") {
      const discovery = await discoverService(this.client, plan);
      if (discovery.kind === "missing") {
        state.action = "discover";
        state.outcomeUncertain = true;
        return { ...output("blocked"), blockedReason: "dispatch_outcome_unresolved" };
      }
      if (discovery.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      state.serviceID = discovery.service.id;
      state.action = "observe";
      return output("cleanup", 1);
    }
    if (!state.serviceID || !uuidPattern.test(state.serviceID)) {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (state.action === "confirm-delete") {
      const service = await this.client.getService(state.serviceID);
      if (!service || service.status === "DELETED") return output("terminal", 1);
      if (!serviceMatchesPlan(service, plan)) {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      return output("cleanup");
    }
    if (state.action === "delete") {
      const observation = await observeByID(this.client, plan, state.serviceID);
      if (observation.kind === "missing") return output("terminal", 1);
      if (observation.kind !== "owned") {
        return {
          ...output(observation.kind === "pending" ? "cleanup" : "blocked"),
          ...(observation.kind === "pending"
            ? {}
            : { blockedReason: "identity_resolution_required" }),
        };
      }
      try {
        await this.client.deleteService(state.serviceID);
      } finally {
        state.action = "confirm-delete";
      }
      return output("cleanup", 1);
    }
    const observation = await observeByID(this.client, plan, state.serviceID);
    if (observation.kind === "missing") return output("terminal", 1);
    if (observation.kind === "conflict") {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (observation.kind === "pending") return output("cleanup");
    if (input.retain) return output("retained", 1);
    state.action = "delete";
    state.deploymentID = observation.value.deployment.id;
    return output("cleanup", 1);
  }
}

export function koyebConfigurationMissing(env: Env): string[] {
  const missing: string[] = [];
  if (!env.KOYEB_API_TOKEN?.trim()) missing.push("KOYEB_API_TOKEN");
  if (!uuidPattern.test(env.CRABBOX_KOYEB_ORGANIZATION_ID?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_ORGANIZATION_ID");
  }
  if (!uuidPattern.test(env.CRABBOX_KOYEB_APP_ID?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_APP_ID");
  }
  if (!immutableImagePattern.test(env.CRABBOX_KOYEB_IMAGE?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_IMAGE");
  }
  const registrySecret = env.CRABBOX_KOYEB_REGISTRY_SECRET?.trim();
  if (registrySecret && !validKoyebSecretName(registrySecret)) {
    missing.push("CRABBOX_KOYEB_REGISTRY_SECRET");
  }
  try {
    canonicalAPIURL(env.CRABBOX_KOYEB_API_URL);
  } catch {
    missing.push("CRABBOX_KOYEB_API_URL");
  }
  if (!validKoyebName(env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion)) {
    missing.push("CRABBOX_KOYEB_REGION");
  }
  if (!validKoyebName(env.CRABBOX_KOYEB_INSTANCE_TYPE?.trim() || defaultInstanceType)) {
    missing.push("CRABBOX_KOYEB_INSTANCE_TYPE");
  }
  return [...new Set(missing)];
}

export function koyebPrivateMeshAvailable(env: Env): boolean {
  const appName = env.KOYEB_APP_NAME?.trim() ?? "";
  const appID = env.KOYEB_APP_ID?.trim() ?? "";
  const organizationID = env.KOYEB_ORGANIZATION_ID?.trim() ?? "";
  const region = env.KOYEB_REGION?.trim() ?? "";
  return (
    validKoyebName(appName) &&
    appID === env.CRABBOX_KOYEB_APP_ID?.trim() &&
    organizationID === env.CRABBOX_KOYEB_ORGANIZATION_ID?.trim() &&
    region === (env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion)
  );
}

function koyebConfiguration(env: Env): KoyebConfiguration {
  const registrySecret = env.CRABBOX_KOYEB_REGISTRY_SECRET?.trim();
  const appName = koyebPrivateMeshAvailable(env) ? env.KOYEB_APP_NAME!.trim() : undefined;
  return {
    apiURL: canonicalAPIURL(env.CRABBOX_KOYEB_API_URL),
    token: env.KOYEB_API_TOKEN!.trim(),
    organizationID: env.CRABBOX_KOYEB_ORGANIZATION_ID!.trim(),
    appID: env.CRABBOX_KOYEB_APP_ID!.trim(),
    region: env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion,
    instanceType: env.CRABBOX_KOYEB_INSTANCE_TYPE?.trim() || defaultInstanceType,
    image: env.CRABBOX_KOYEB_IMAGE!.trim(),
    ...(registrySecret ? { registrySecret } : {}),
    ...(appName ? { appName } : {}),
  };
}

function canonicalAPIURL(value: string | undefined): string {
  const url = new URL(value?.trim() || defaultAPIURL);
  if (
    url.protocol !== "https:" ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    (url.pathname !== "/" && url.pathname !== "")
  ) {
    throw new Error("invalid Koyeb API URL");
  }
  return url.origin;
}

function validKoyebName(value: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,62}$/.test(value);
}

function validKoyebSecretName(value: string): boolean {
  return value.length >= 2 && validKoyebName(value);
}

async function validatedPlan(
  client: KoyebClient,
  frozen: FrozenProvisioningPlan,
  lease: LeaseRecord,
): Promise<KoyebProvisioningPlan> {
  const storedData = asObject(frozen.data) as Partial<KoyebProvisioningPlan>;
  const data: Partial<KoyebProvisioningPlan> = {
    ...storedData,
    // Plans written before native mesh support implicitly used Tailscale.
    transport: storedData.transport ?? "tailscale",
  };
  const scope = await client.providerScope();
  const resource = frozen.resources[0];
  const privateHost = client.privateHost(data.serviceName ?? "");
  if (
    frozen.version !== 1 ||
    frozen.provider !== "koyeb" ||
    frozen.scope !== scope ||
    frozen.resources.length !== 1 ||
    !resource ||
    resource.scope !== scope ||
    data.version !== 1 ||
    !validKoyebName(data.serviceName ?? "") ||
    data.runnerLeaseID !== data.serviceName ||
    resource.cloudID !== data.serviceName ||
    resource.region !== data.region ||
    data.organizationID !== client.organizationID ||
    data.appID !== client.appID ||
    !validKoyebName(data.region ?? "") ||
    !validKoyebName(data.instanceType ?? "") ||
    !immutableImagePattern.test(data.image ?? "") ||
    (data.registrySecret !== undefined && !validKoyebSecretName(data.registrySecret)) ||
    lease.provider !== "koyeb" ||
    lease.providerScope !== scope ||
    data.leaseID !== lease.id ||
    data.slug !== (lease.slug ?? "") ||
    data.owner !== (lease.providerOwner || lease.owner) ||
    data.org !== lease.org ||
    data.generation !== lease.createAttemptGeneration ||
    !Number.isSafeInteger(data.ttlSeconds) ||
    (data.ttlSeconds ?? 0) <= 0 ||
    (data.ttlSeconds ?? 0) > 86_400 ||
    !Number.isSafeInteger(data.idleTimeoutSeconds) ||
    (data.idleTimeoutSeconds ?? 0) <= 0 ||
    (data.idleTimeoutSeconds ?? 0) > 86_400 ||
    typeof data.sshPublicKey !== "string" ||
    !data.sshPublicKey.trim() ||
    !["tailscale", "koyeb-mesh"].includes(data.transport ?? "") ||
    (data.transport === "koyeb-mesh" && (!privateHost || data.privateHost !== privateHost)) ||
    (data.transport === "tailscale" && data.privateHost !== undefined) ||
    (data.transport === "tailscale" && !validKoyebName(data.tailscaleHostname ?? "")) ||
    typeof data.tailscaleHostname !== "string" ||
    !Array.isArray(data.tailscaleTags) ||
    (data.transport === "tailscale" && data.tailscaleTags.length === 0) ||
    data.tailscaleTags.some((tag) => !/^tag:[a-z0-9][a-z0-9-]{0,62}$/.test(tag))
  ) {
    throw new Error("Koyeb provisioning plan is invalid or belongs to another context");
  }
  return data as KoyebProvisioningPlan;
}

function continuationState(value: unknown): KoyebContinuationState {
  const state = asObject(value) as Partial<KoyebContinuationState>;
  if (
    state.version !== 1 ||
    !["dispatch", "discover", "observe", "bootstrap", "delete", "confirm-delete"].includes(
      state.action ?? "",
    ) ||
    (state.serviceID !== undefined && !uuidPattern.test(state.serviceID)) ||
    (state.deploymentID !== undefined && !uuidPattern.test(state.deploymentID))
  ) {
    throw new Error("Koyeb continuation state is invalid");
  }
  return structuredClone(state as KoyebContinuationState);
}

function createServiceRequest(plan: KoyebProvisioningPlan, sandboxSecret: string): JSONRecord {
  const docker: JSONRecord = {
    image: plan.image,
    ...(plan.registrySecret ? { image_registry_secret: plan.registrySecret } : {}),
  };
  return {
    app_id: plan.appID,
    life_cycle: {
      delete_after_create: plan.ttlSeconds,
      delete_after_sleep: plan.idleTimeoutSeconds,
    },
    definition: {
      name: plan.serviceName,
      type: "SANDBOX",
      docker,
      env: [
        { key: "SANDBOX_SECRET", value: sandboxSecret },
        ...Object.entries(ownershipEnvironment(plan)).map(([key, value]) => ({ key, value })),
      ],
      regions: [plan.region],
      instance_types: [{ type: plan.instanceType }],
      ...(plan.transport === "koyeb-mesh" ? { mesh: "DEPLOYMENT_MESH_ENABLED" } : {}),
      // Koyeb's Sandbox runtime owns 3030 for management and 3031 for its TCP
      // proxy. Exposing target port 22 directly makes Koyeb select it as PORT,
      // moving management off 3030 before Crabbox can bootstrap the runner.
      ports:
        plan.transport === "koyeb-mesh"
          ? [
              { port: 3030, protocol: "http" },
              { port: 3031, protocol: "tcp" },
            ]
          : [{ port: 3030, protocol: "http" }],
      routes:
        plan.transport === "koyeb-mesh"
          ? []
          : [
              {
                port: 3030,
                path: `${managementRoute}/`,
                security_policies: { api_keys: [sandboxSecret] },
              },
            ],
      scalings: [
        {
          min: 1,
          max: 1,
          targets: [],
        },
      ],
    },
  };
}

async function discoverService(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  expectedSandboxSecret?: string,
): Promise<
  | { kind: "missing" }
  | { kind: "conflict" }
  | { kind: "pending"; service: KoyebService }
  | { kind: "owned"; service: KoyebService; value: OwnedDeployment }
> {
  const matches = (await client.listServices(plan.serviceName)).filter(
    (service) => service.name === plan.serviceName,
  );
  if (matches.length === 0) return { kind: "missing" };
  if (matches.length !== 1) return { kind: "conflict" };
  const service = matches[0]!;
  const observation = await observeService(client, plan, service, expectedSandboxSecret);
  return observation.kind === "owned"
    ? { kind: "owned", service, value: observation.value }
    : observation.kind === "pending"
      ? { kind: "pending", service }
      : { kind: observation.kind };
}

async function observeByID(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  serviceID: string,
  expectedSandboxSecret?: string,
): Promise<Observation> {
  const service = await client.getService(serviceID);
  return service
    ? observeService(client, plan, service, expectedSandboxSecret)
    : { kind: "missing" };
}

async function observeService(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  service: KoyebService,
  expectedSandboxSecret?: string,
): Promise<Observation> {
  if (!serviceMatchesPlan(service, plan)) return { kind: "conflict" };
  if (
    service.activeDeploymentID &&
    service.latestDeploymentID &&
    service.activeDeploymentID !== service.latestDeploymentID
  ) {
    // A service-level delete also removes the latest deployment. Do not trust
    // the active deployment as proof of ownership while Koyeb reports another
    // deployment generation that has not been validated against this lease.
    return { kind: "pending", service };
  }
  const deploymentID = service.activeDeploymentID || service.latestDeploymentID;
  if (!deploymentID) return { kind: "pending", service };
  const deployment = await client.getDeployment(deploymentID);
  if (!deployment) return { kind: "pending", service };
  const sandboxSecret = deploymentSandboxSecret(deployment);
  if (
    !deploymentMatchesPlan(deployment, service, plan, expectedSandboxSecret) ||
    !sandboxSecret ||
    (expectedSandboxSecret !== undefined && sandboxSecret !== expectedSandboxSecret)
  ) {
    return { kind: "conflict" };
  }
  const sandbox = asObject(asObject(deployment.metadata)["sandbox"]);
  const publicURL = optionalString(sandbox["public_url"]);
  const routingKey = optionalString(sandbox["routing_key"]);
  return {
    kind: "owned",
    value: {
      service,
      deployment,
      sandboxSecret,
      ...(publicURL ? { publicURL } : {}),
      ...(routingKey ? { routingKey } : {}),
    },
  };
}

function managementTarget(
  owned: OwnedDeployment,
  plan: KoyebProvisioningPlan,
): { baseURL: string; routingKey?: string } | undefined {
  if (plan.transport === "koyeb-mesh") {
    return plan.privateHost ? { baseURL: `http://${plan.privateHost}:3030` } : undefined;
  }
  return owned.publicURL && owned.routingKey
    ? { baseURL: owned.publicURL, routingKey: owned.routingKey }
    : undefined;
}

function serviceMatchesPlan(service: KoyebService, plan: KoyebProvisioningPlan): boolean {
  return (
    uuidPattern.test(service.id) &&
    service.name === plan.serviceName &&
    service.type === "SANDBOX" &&
    service.organizationID === plan.organizationID &&
    service.appID === plan.appID &&
    lifeCycleMatchesPlan(service.lifeCycle, plan)
  );
}

function lifeCycleMatchesPlan(lifeCycle: JSONRecord, plan: KoyebProvisioningPlan): boolean {
  return (
    hasOnlyKeys(lifeCycle, ["delete_after_create", "delete_after_sleep"]) &&
    lifeCycle["delete_after_create"] === plan.ttlSeconds &&
    lifeCycle["delete_after_sleep"] === plan.idleTimeoutSeconds
  );
}

function deploymentMatchesPlan(
  deployment: KoyebDeployment,
  service: KoyebService,
  plan: KoyebProvisioningPlan,
  expectedSandboxSecret?: string,
): boolean {
  const definition = deployment.definition;
  const docker = asObject(definition["docker"]);
  const regions = definition["regions"];
  const instanceTypes = definition["instance_types"];
  const scalings = definition["scalings"];
  const ports = definition["ports"];
  const routes = definition["routes"];
  const proxyPorts = definition["proxy_ports"];
  const sandboxSecret = deploymentSandboxSecret(deployment);
  return (
    uuidPattern.test(deployment.id) &&
    deployment.organizationID === plan.organizationID &&
    deployment.appID === plan.appID &&
    deployment.serviceID === service.id &&
    definition["name"] === plan.serviceName &&
    definition["type"] === "SANDBOX" &&
    meshMatchesPlan(definition["mesh"], plan) &&
    dockerMatchesPlan(docker, plan) &&
    exactStringArray(regions, [plan.region]) &&
    instanceTypesMatchPlan(instanceTypes, plan) &&
    scalingsMatchPlan(scalings, plan) &&
    portsMatchPlan(ports, plan) &&
    routesMatchPlan(routes, sandboxSecret, plan) &&
    optionalEmptyArray(proxyPorts) &&
    Boolean(sandboxSecret) &&
    (expectedSandboxSecret === undefined || sandboxSecret === expectedSandboxSecret) &&
    environmentMatchesPlan(definition["env"], plan, sandboxSecret)
  );
}

function dockerMatchesPlan(docker: JSONRecord, plan: KoyebProvisioningPlan): boolean {
  return (
    hasOnlyKeys(docker, [
      "image",
      "command",
      "args",
      "image_registry_secret",
      "entrypoint",
      "privileged",
    ]) &&
    docker["image"] === plan.image &&
    (plan.registrySecret
      ? docker["image_registry_secret"] === plan.registrySecret
      : optionalEmptyString(docker["image_registry_secret"])) &&
    optionalEmptyString(docker["command"]) &&
    optionalEmptyArray(docker["args"]) &&
    optionalEmptyArray(docker["entrypoint"]) &&
    optionalFalse(docker["privileged"])
  );
}

function instanceTypesMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value) || value.length !== 1) return false;
  const instanceType = asObject(value[0]);
  return (
    hasOnlyKeys(instanceType, ["type", "scopes"]) &&
    instanceType["type"] === plan.instanceType &&
    optionalRegionScopes(instanceType["scopes"], plan.region)
  );
}

function scalingsMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value) || value.length !== 1) return false;
  const scaling = asObject(value[0]);
  const targets = scaling["targets"];
  if (
    !hasOnlyKeys(scaling, ["scopes", "min", "max", "targets"]) ||
    !optionalRegionScopes(scaling["scopes"], plan.region) ||
    scaling["min"] !== 1 ||
    scaling["max"] !== 1 ||
    !Array.isArray(targets) ||
    targets.length !== 0
  ) {
    return false;
  }
  return true;
}

function meshMatchesPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  return plan.transport === "koyeb-mesh"
    ? value === "DEPLOYMENT_MESH_ENABLED"
    : nullish(value) || value === "DEPLOYMENT_MESH_AUTO";
}

function portsMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value)) return false;
  const actual = value
    .map((item) => asObject(item))
    .map((port) => {
      if (!hasOnlyKeys(port, ["port", "protocol"])) return "";
      return `${String(port["port"])}:${String(port["protocol"])}`;
    })
    .toSorted();
  const expected =
    plan.transport === "koyeb-mesh"
      ? [
          ["3030:http", "3031:tcp"],
          // Plans created before the Sandbox proxy contract exposed SSH
          // directly. Koyeb canonicalized that payload to this order. Keep
          // recognizing it so failed legacy leases remain safely reclaimable.
          ["22:tcp", "3030:http"],
        ]
      : [["3030:http"]];
  return expected.some(
    (ports) =>
      actual.length === ports.length && ports.every((port, index) => actual[index] === port),
  );
}

function routesMatchPlan(
  value: unknown,
  sandboxSecret: string,
  plan: KoyebProvisioningPlan,
): boolean {
  if (plan.transport === "koyeb-mesh") return Array.isArray(value) && value.length === 0;
  if (!Array.isArray(value) || value.length !== 1) return false;
  const route = asObject(value[0]);
  return (
    hasOnlyKeys(route, ["port", "path", "security_policies"]) &&
    route["port"] === 3030 &&
    route["path"] === `${managementRoute}/` &&
    sandboxRouteSecurityPolicies(route["security_policies"], sandboxSecret)
  );
}

function environmentMatchesPlan(
  value: unknown,
  plan: KoyebProvisioningPlan,
  sandboxSecret: string,
): boolean {
  if (!Array.isArray(value)) return false;
  const expected = { SANDBOX_SECRET: sandboxSecret, ...ownershipEnvironment(plan) };
  if (value.length !== Object.keys(expected).length) return false;
  const seen = new Set<string>();
  for (const item of value) {
    const entry = asObject(item);
    if (
      !hasOnlyKeys(entry, ["scopes", "key", "value", "secret"]) ||
      typeof entry["key"] !== "string" ||
      typeof entry["value"] !== "string" ||
      !optionalEmptyString(entry["secret"]) ||
      !optionalRegionScopes(entry["scopes"], plan.region) ||
      seen.has(entry["key"]) ||
      expected[entry["key"] as keyof typeof expected] !== entry["value"]
    ) {
      return false;
    }
    seen.add(entry["key"]);
  }
  return seen.size === Object.keys(expected).length;
}

function sandboxRouteSecurityPolicies(value: unknown, sandboxSecret: string): boolean {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const policies = value as JSONRecord;
  return (
    hasOnlyKeys(policies, ["basic_auths", "api_keys"]) &&
    optionalEmptyArray(policies["basic_auths"]) &&
    exactStringArray(policies["api_keys"], [sandboxSecret])
  );
}

function exactStringArray(value: unknown, expected: string[]): boolean {
  return (
    Array.isArray(value) &&
    value.length === expected.length &&
    value.every((item, index) => item === expected[index])
  );
}

function optionalRegionScopes(value: unknown, region: string): boolean {
  return (
    nullish(value) ||
    (Array.isArray(value) &&
      (value.length === 0 || (value.length === 1 && value[0] === `region:${region}`)))
  );
}

function optionalEmptyArray(value: unknown): boolean {
  return nullish(value) || (Array.isArray(value) && value.length === 0);
}

function optionalEmptyString(value: unknown): boolean {
  return nullish(value) || value === "";
}

function optionalFalse(value: unknown): boolean {
  return nullish(value) || value === false;
}

function nullish(value: unknown): boolean {
  return value === undefined || value === null;
}

function hasOnlyKeys(value: JSONRecord, allowed: string[]): boolean {
  const allowedKeys = new Set(allowed);
  return Object.keys(value).every((key) => allowedKeys.has(key));
}

function ownershipEnvironment(plan: KoyebProvisioningPlan): Record<string, string> {
  return {
    CRABBOX_MANAGED_BY: "crabbox",
    CRABBOX_PROVIDER: "koyeb",
    CRABBOX_LEASE_ID: plan.leaseID,
    CRABBOX_LEASE_SLUG: providerLabelValue(plan.slug),
    CRABBOX_LEASE_OWNER: providerLabelValue(plan.owner),
    CRABBOX_LEASE_ORG: providerLabelValue(plan.org),
    CRABBOX_PROVISIONING_GENERATION: plan.generation,
  };
}

function ownershipLabels(plan: KoyebProvisioningPlan): Record<string, string> {
  return {
    crabbox: "true",
    created_by: "crabbox",
    lease: plan.leaseID,
    slug: providerLabelValue(plan.slug),
    owner: providerLabelValue(plan.owner),
    provider: "koyeb",
    server_type: providerLabelValue(plan.instanceType),
  };
}

function imageIdentity(plan: KoyebProvisioningPlan): LeaseImageIdentity {
  return {
    id: plan.image,
    source: "explicit",
    provider: "koyeb",
    kind: imageKind,
    region: plan.region,
    ...(plan.registrySecret ? { sourceID: plan.registrySecret } : {}),
  };
}

function deploymentEnvironment(deployment: KoyebDeployment): Record<string, string> {
  const values: Record<string, string> = {};
  const environment = deployment.definition["env"];
  if (!Array.isArray(environment)) return values;
  for (const item of environment) {
    const entry = asObject(item);
    if (typeof entry["key"] === "string" && typeof entry["value"] === "string") {
      values[entry["key"]] = entry["value"];
    }
  }
  return values;
}

function deploymentSandboxSecret(deployment: KoyebDeployment): string {
  const value = deploymentEnvironment(deployment)["SANDBOX_SECRET"] ?? "";
  return /^[A-Za-z0-9_-]{32,128}$/.test(value) ? value : "";
}

function runnerReady(stdout: string, plan: KoyebProvisioningPlan): RunnerReady | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(stdout.trim());
  } catch {
    return undefined;
  }
  const value = asObject(parsed);
  const ssh = asObject(value["ssh"]);
  const host = stringValue(ssh["host"]);
  const hostKey = sshPublicKeyIdentity(stringValue(ssh["hostKey"]));
  if (plan.transport === "koyeb-mesh") {
    const network = asObject(value["network"]);
    if (
      value["schema"] !== "crabbox-koyeb-sandbox-runner/v2" ||
      value["leaseId"] !== plan.runnerLeaseID ||
      ssh["user"] !== "crabbox" ||
      ssh["port"] !== 22 ||
      host !== plan.privateHost ||
      network["transport"] !== "koyeb-mesh" ||
      network["privateHost"] !== plan.privateHost ||
      !/^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$/.test(hostKey)
    ) {
      return undefined;
    }
    return {
      transport: "koyeb-mesh",
      host,
      user: "crabbox",
      port: "22",
      hostKey,
    };
  }
  const tailscale = asObject(value["tailscale"]);
  const ipv4 = stringValue(tailscale["ipv4"]);
  const dnsName = optionalString(tailscale["dnsName"]);
  if (
    value["schema"] !== "crabbox-koyeb-sandbox-runner/v1" ||
    value["leaseId"] !== plan.runnerLeaseID ||
    ssh["user"] !== "crabbox" ||
    ssh["port"] !== 22 ||
    host !== ipv4 ||
    !validTailscaleIPv4(ipv4) ||
    (dnsName !== undefined && !validDNSName(dnsName)) ||
    !/^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$/.test(hostKey)
  ) {
    return undefined;
  }
  return {
    transport: "tailscale",
    host: dnsName || ipv4,
    ipv4,
    ...(dnsName ? { fqdn: dnsName } : {}),
    user: "crabbox",
    port: "22",
    hostKey,
  };
}

function validTailscaleIPv4(value: string): boolean {
  const parts = value.split(".").map(Number);
  if (
    parts.length !== 4 ||
    parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)
  ) {
    return false;
  }
  return parts[0] === 100 && parts[1]! >= 64 && parts[1]! <= 127;
}

function validDNSName(value: string): boolean {
  return (
    value.length <= 253 &&
    value === value.toLowerCase() &&
    value.split(".").every((label) => validKoyebName(label))
  );
}

function machineForService(
  service: KoyebService,
  plan: KoyebProvisioningPlan,
  host: string,
  labels: Record<string, string> = ownershipLabels(plan),
): ProviderMachine {
  return {
    provider: "koyeb",
    id: 0,
    cloudID: service.id,
    region: plan.region,
    name: service.name,
    status: service.status.toLowerCase(),
    serverType: plan.instanceType,
    host,
    labels,
  };
}

function planForLeaseCleanup(client: KoyebClient, lease: LeaseRecord): KoyebProvisioningPlan {
  const image = lease.image;
  const region = lease.region;
  if (
    !image ||
    image.provider !== "koyeb" ||
    image.source !== "explicit" ||
    image.kind !== imageKind ||
    !immutableImagePattern.test(image.id) ||
    !region ||
    image.region !== region ||
    (image.sourceID !== undefined && !validKoyebSecretName(image.sourceID))
  ) {
    throw new ProviderResourceUnresolvedError("Koyeb lease cleanup image identity is incomplete");
  }
  const serviceName = leaseProviderName(lease.id, lease.slug);
  const expectedPrivateHost = client.privateHost(serviceName);
  const transport: KoyebTransport =
    !lease.tailscale?.enabled && lease.host === expectedPrivateHost ? "koyeb-mesh" : "tailscale";
  const privateHost = transport === "koyeb-mesh" ? client.privateHost(serviceName) : undefined;
  if (transport === "koyeb-mesh" && !privateHost) {
    throw new ProviderResourceUnresolvedError("Koyeb lease cleanup mesh identity is incomplete");
  }
  return {
    version: 1,
    serviceName,
    runnerLeaseID: serviceName,
    organizationID: client.organizationID,
    appID: client.appID,
    region,
    instanceType: lease.serverType,
    image: image.id,
    ...(image.sourceID ? { registrySecret: image.sourceID } : {}),
    leaseID: lease.id,
    slug: lease.slug ?? "",
    owner: lease.providerOwner || lease.owner,
    org: lease.org,
    generation: lease.createAttemptGeneration!,
    ttlSeconds: lease.ttlSeconds,
    idleTimeoutSeconds: lease.idleTimeoutSeconds ?? lease.ttlSeconds,
    sshPublicKey: "cleanup-only",
    transport,
    ...(privateHost ? { privateHost } : {}),
    tailscaleHostname: lease.tailscale?.hostname || leaseProviderName(lease.id, lease.slug),
    tailscaleTags: lease.tailscale?.tags?.length ? [...lease.tailscale.tags] : ["tag:crabbox"],
  };
}

function inventoryMachine(
  client: KoyebClient,
  service: KoyebService,
  deployment: KoyebDeployment,
): ProviderMachine | undefined {
  const definition = deployment.definition;
  const environment = deploymentEnvironment(deployment);
  const leaseID = environment["CRABBOX_LEASE_ID"] ?? "";
  const slug = environment["CRABBOX_LEASE_SLUG"] ?? "";
  const owner = environment["CRABBOX_LEASE_OWNER"] ?? "";
  const generation = environment["CRABBOX_PROVISIONING_GENERATION"] ?? "";
  const org = environment["CRABBOX_LEASE_ORG"] ?? "";
  const docker = asObject(definition["docker"]);
  const regions = definition["regions"];
  const instanceTypes = definition["instance_types"];
  const region = Array.isArray(regions) ? stringValue(regions[0]) : "";
  const instanceType = Array.isArray(instanceTypes)
    ? stringValue(asObject(instanceTypes[0])["type"])
    : "";
  const image = stringValue(docker["image"]);
  const registrySecret = optionalString(docker["image_registry_secret"]);
  const transport: KoyebTransport =
    definition["mesh"] === "DEPLOYMENT_MESH_ENABLED" ? "koyeb-mesh" : "tailscale";
  const privateHost = transport === "koyeb-mesh" ? client.privateHost(service.name) : undefined;
  const deleteAfterCreate = service.lifeCycle["delete_after_create"];
  const deleteAfterSleep = service.lifeCycle["delete_after_sleep"];
  if (
    !/^cbx_[a-f0-9]{12}$/.test(leaseID) ||
    !validKoyebName(slug) ||
    !owner ||
    !org ||
    !generation ||
    !validKoyebName(region) ||
    !validKoyebName(instanceType) ||
    !immutableImagePattern.test(image) ||
    !Number.isSafeInteger(deleteAfterCreate) ||
    !Number.isSafeInteger(deleteAfterSleep) ||
    (registrySecret !== undefined && !validKoyebSecretName(registrySecret)) ||
    (transport === "koyeb-mesh" && !privateHost)
  ) {
    return undefined;
  }
  const plan: KoyebProvisioningPlan = {
    version: 1,
    serviceName: leaseProviderName(leaseID, slug),
    runnerLeaseID: leaseProviderName(leaseID, slug),
    organizationID: service.organizationID,
    appID: service.appID,
    region,
    instanceType,
    image,
    ...(registrySecret ? { registrySecret } : {}),
    leaseID,
    slug,
    owner,
    org,
    generation,
    ttlSeconds: deleteAfterCreate as number,
    idleTimeoutSeconds: deleteAfterSleep as number,
    sshPublicKey: "inventory-only",
    transport,
    ...(privateHost ? { privateHost } : {}),
    tailscaleHostname: service.name,
    tailscaleTags: ["tag:crabbox"],
  };
  if (!serviceMatchesPlan(service, plan) || !deploymentMatchesPlan(deployment, service, plan)) {
    return undefined;
  }
  return machineForService(service, plan, "");
}

function koyebService(value: unknown): KoyebService {
  const service = asObject(value);
  const parsed: KoyebService = {
    id: stringValue(service["id"]),
    name: stringValue(service["name"]),
    type: stringValue(service["type"]),
    organizationID: stringValue(service["organization_id"]),
    appID: stringValue(service["app_id"]),
    status: stringValue(service["status"]),
    lifeCycle: asObject(service["life_cycle"]),
  };
  const activeDeploymentID = optionalString(service["active_deployment_id"]);
  const latestDeploymentID = optionalString(service["latest_deployment_id"]);
  if (activeDeploymentID) parsed.activeDeploymentID = activeDeploymentID;
  if (latestDeploymentID) parsed.latestDeploymentID = latestDeploymentID;
  return parsed;
}

function koyebDeployment(value: unknown): KoyebDeployment {
  const deployment = asObject(value);
  return {
    id: stringValue(deployment["id"]),
    organizationID: stringValue(deployment["organization_id"]),
    appID: stringValue(deployment["app_id"]),
    serviceID: stringValue(deployment["service_id"]),
    status: stringValue(deployment["status"]),
    definition: asObject(deployment["definition"]),
    metadata: asObject(deployment["metadata"]),
  };
}

function asObject(value: unknown): JSONRecord {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as JSONRecord) : {};
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function optionalString(value: unknown): string | undefined {
  const string = stringValue(value).trim();
  return string || undefined;
}

function requireUUID(value: string, name: string): void {
  if (!uuidPattern.test(value)) throw new Error(`invalid Koyeb ${name}`);
}

function managementBaseURL(value: string, publicEdge: boolean): string {
  const url = new URL(value.trim());
  const validPublic = publicEdge && url.protocol === "https:";
  const validPrivate =
    !publicEdge &&
    url.protocol === "http:" &&
    url.port === "3030" &&
    (url.pathname === "/" || url.pathname === "") &&
    url.hostname.endsWith(".internal") &&
    validDNSName(url.hostname);
  if ((!validPublic && !validPrivate) || url.username || url.password || url.search || url.hash) {
    throw new Error("koyeb sandbox management URL is malformed");
  }
  // The public Sandbox endpoint is mounted below /koyeb-sandbox. A private
  // mesh address reaches port 3030 directly, where the API is rooted at /
  // (/health, /run, and the other operation paths).
  if (publicEdge) url.pathname = `${url.pathname.replace(/\/+$/, "")}${managementRoute}`;
  return url.toString().replace(/\/+$/, "");
}

function validRoutingKey(value: string): boolean {
  return value.length > 0 && value.length <= 512 && !/[\r\n]/.test(value);
}

function serviceTerminal(status: string): boolean {
  return ["UNHEALTHY", "DELETING", "DELETED", "PAUSED"].includes(status);
}

function deploymentTerminal(status: string): boolean {
  return ["CANCELED", "UNHEALTHY", "STOPPED", "ERROR"].includes(status);
}

function randomSecret(): string {
  return btoa(
    Array.from(crypto.getRandomValues(new Uint8Array(32)), (byte) =>
      String.fromCharCode(byte),
    ).join(""),
  )
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll("=", "");
}

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
