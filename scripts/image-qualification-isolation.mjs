// Protected qualification admission. This record is operator-reviewed configuration,
// never candidate input. Account administrators remain trusted custodians.
import crypto from "node:crypto";

export const authorityName = "crabbox-aws-qualification-authority";
export const controllerName = "crabbox-image-qualification-controller";
export const canonical = (value) =>
  Array.isArray(value)
    ? `[${value.map(canonical).join(",")}]`
    : value && typeof value === "object"
      ? `{${Object.keys(value)
          .sort()
          .map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`)
          .join(",")}}`
      : JSON.stringify(value);
export const digest = (value) => crypto.createHash("sha256").update(value).digest("hex");
const fail = (message) => {
  throw new Error(`qualification isolation: ${message}`);
};
const same = (a, b) => canonical(a) === canonical(b);
const hex = /^[0-9a-f]{32}$/;

export function bindingShape(bindings) {
  if (!Array.isArray(bindings)) fail("bindings missing");
  // Preserve every non-secret coordinate, including namespace, environment and
  // dispatch-namespace fields. Unknown fields must not silently disappear.
  return bindings
    .map(({ text, ...binding }) => ({
      ...binding,
      ...(binding.type === "plain_text" ? { text } : {}),
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

export function readAdmission(env = process.env) {
  const raw = env.QUALIFICATION_ISOLATION_JSON;
  if (!raw || raw.length > 32_768) fail("admitted resource record missing or oversized");
  const record = JSON.parse(raw);
  const sha256 = digest(canonical(record));
  if (sha256 !== env.QUALIFICATION_ISOLATION_SHA256) fail("admission digest mismatch");
  const { authority, aws, production } = record;
  if (
    record.version !== 1 ||
    !hex.test(record.accountId ?? "") ||
    record.accountId !== env.CLOUDFLARE_ACCOUNT_ID ||
    authority?.name !== authorityName ||
    !/^[0-9a-f-]{36}$/.test(authority.version ?? "") ||
    !/^[0-9a-f]{40}$/.test(authority.sourceSha ?? "") ||
    !/^[0-9a-f]{64}$/.test(authority.policyHash ?? "") ||
    !/^[0-9a-f]{64}$/.test(authority.bindingDigest ?? "") ||
    !/^[0-9]{12}$/.test(aws?.accountId ?? "") ||
    !new RegExp(`^arn:aws:iam::${aws.accountId}:role/[A-Za-z0-9+=,.@_/-]+$`).test(
      aws.roleArn ?? "",
    ) ||
    !/^[a-z]{2}-[a-z]+-[0-9]+$/.test(aws.region ?? "") ||
    !/^subnet-[0-9a-f]+$/.test(aws.subnetId ?? "") ||
    !/^sg-[0-9a-f]+$/.test(aws.securityGroupId ?? "") ||
    !/^ami-[0-9a-f]+$/.test(aws.baseAmiId ?? "") ||
    !Number.isInteger(aws.rootGB) ||
    aws.rootGB < 8 ||
    aws.rootGB > 400 ||
    !Array.isArray(authority.namespaces) ||
    authority.namespaces.length !== 2
  )
    fail("invalid admitted identities");
  const expectedClasses = ["AWSQualificationRegistry", "AWSQualificationRun"];
  if (
    !same(authority.namespaces.map((n) => n.class).sort(), expectedClasses) ||
    authority.namespaces.some((n) => !hex.test(n.id ?? "") || n.script !== authorityName) ||
    new Set(authority.namespaces.map((n) => n.id)).size !== 2
  )
    fail("authority namespace ownership invalid");
  for (const key of ["workers", "namespaceIds", "roleArns", "subnetIds", "securityGroupIds"]) {
    if (
      !Array.isArray(production?.[key]) ||
      production[key].length === 0 ||
      production[key].some((value) => typeof value !== "string" || !value)
    )
      fail("production exclusion inventory missing");
  }
  if (
    production.workers.some(
      (name) =>
        name === authorityName ||
        name === controllerName ||
        /^crabbox-image-qualification-(?:relay-)?[0-9]+-[0-9]+$/.test(name),
    ) ||
    authority.namespaces.some((n) => production.namespaceIds.includes(n.id)) ||
    production.roleArns.includes(aws.roleArn) ||
    production.subnetIds.includes(aws.subnetId) ||
    production.securityGroupIds.includes(aws.securityGroupId)
  )
    fail("qualification and production identities overlap");
  for (const [key, value] of Object.entries({
    QUALIFICATION_AUTHORITY_SHA: authority.sourceSha,
    QUALIFICATION_AUTHORITY_VERSION: authority.logicalVersion,
    QUALIFICATION_EXPECTED_POLICY_HASH: authority.policyHash,
    QUALIFICATION_AWS_REGION: aws.region,
    QUALIFICATION_SUBNET_ID: aws.subnetId,
    QUALIFICATION_SECURITY_GROUP_ID: aws.securityGroupId,
    QUALIFICATION_BASE_AMI_ID: aws.baseAmiId,
    QUALIFICATION_ROOT_GB: String(aws.rootGB),
  }))
    if (env[key] !== undefined && env[key] !== value) fail(`${key} differs from admission`);
  if (typeof authority.logicalVersion !== "string" || !authority.logicalVersion)
    fail("authority logical version missing");
  return { ...record, sha256 };
}

export async function namespaces(cf) {
  const result = await cf.list("/workers/durable_objects/namespaces");
  if (
    result.some((n) => !hex.test(n.id ?? "") || !n.script || !n.class) ||
    new Set(result.map((n) => n.id)).size !== result.length
  )
    fail("namespace inventory incomplete or ambiguous");
  return result;
}

export async function ownedNamespaces(cf, worker, bindings) {
  const all = await namespaces(cf);
  const own = all.filter((n) => n.script === worker);
  const bound = bindings.filter((b) => b.type === "durable_object_namespace");
  for (const binding of bound) {
    if (
      !own.some((n) => n.id === binding.namespace_id && n.class === binding.class_name) ||
      (binding.script_name !== undefined && binding.script_name !== worker) ||
      binding.environment !== undefined ||
      cf.admission.production.namespaceIds.includes(binding.namespace_id)
    )
      fail(`external namespace on ${worker}`);
  }
  if (
    own.length !== bound.length ||
    new Set(bound.map((b) => b.namespace_id)).size !== bound.length
  )
    fail(`unexpected namespace residue on ${worker}`);
  return own;
}

export async function verifyAuthority(cf) {
  const admission = cf.admission;
  const settings = await cf.settings(authorityName);
  if (
    (await cf.latestVersion(authorityName)) !== admission.authority.version ||
    digest(canonical(bindingShape(settings.bindings))) !== admission.authority.bindingDigest ||
    settings.bindings.some(
      (b) =>
        !["plain_text", "secret_text", "durable_object_namespace", "version_metadata"].includes(
          b.type,
        ),
    )
  )
    fail("authority version or bindings drifted");
  const own = await ownedNamespaces(cf, authorityName, settings.bindings);
  if (
    !same(
      own
        .map(({ id, script, class: klass }) => ({ id, script, class: klass }))
        .sort((a, b) => a.id.localeCompare(b.id)),
      [...admission.authority.namespaces].sort((a, b) => a.id.localeCompare(b.id)),
    )
  )
    fail("authority namespaces drifted");
  const values = Object.fromEntries(
    settings.bindings.filter((b) => b.type === "plain_text").map((b) => [b.name, b.text]),
  );
  for (const [key, value] of Object.entries({
    ACCOUNT_ID: admission.aws.accountId,
    ROLE_ARN: admission.aws.roleArn,
    REGION: admission.aws.region,
    SUBNET_ID: admission.aws.subnetId,
    SECURITY_GROUP_ID: admission.aws.securityGroupId,
    BASE_AMI_ID: admission.aws.baseAmiId,
    ROOT_GB: String(admission.aws.rootGB),
    AUTHORITY_SHA: admission.authority.sourceSha,
    AUTHORITY_VERSION: admission.authority.logicalVersion,
  }))
    if (values[`CRABBOX_AWS_QUALIFICATION_${key}`] !== value)
      fail(`authority ${key} differs from admission`);
  const [subdomain, routes, schedules, domains] = await Promise.all([
    cf.request(`/workers/scripts/${authorityName}/subdomain`),
    cf.request(
      `/workers/services/${authorityName}/environments/production/routes?show_zonename=true`,
    ),
    cf.request(`/workers/scripts/${authorityName}/schedules`),
    cf.list(`/workers/domains/records?service=${authorityName}`),
  ]);
  if (
    subdomain?.enabled !== false ||
    subdomain?.previews_enabled !== false ||
    !same(routes, []) ||
    !same(schedules, []) ||
    domains.length !== 0 ||
    (await cf.latestVersion(authorityName)) !== admission.authority.version
  )
    fail("authority is public or changed during verification");
}

export async function requireAbsent(cf, worker) {
  if (
    (await cf.request(`/workers/scripts/${worker}/settings`, {}, { allow404: true })) !==
      undefined ||
    (await namespaces(cf)).some((n) => n.script === worker)
  )
    fail(`name collision at ${worker}`);
}

export function ownershipTags(cf, worker, bindings) {
  return [
    `qualification-isolation-v1:${cf.admission.sha256}`,
    `qualification-worker:${worker}`,
    `qualification-bindings:${digest(
      canonical(
        bindingShape(bindings).map((binding) => {
          if (binding.type !== "durable_object_namespace") return binding;
          const { namespace_id, script_name, ...shape } = binding;
          return shape;
        }),
      ),
    )}`,
  ];
}

export function assertOwnedSettings(cf, worker, settings) {
  if (!settings || !same(settings.tags, ownershipTags(cf, worker, settings.bindings)))
    fail(`unowned or changed Worker ${worker}`);
  const bindings = settings.bindings;
  if (worker === controllerName) {
    const authority = bindings.find((b) => b.name === "AUTHORITY");
    if (
      bindings.length !== 2 ||
      !bindings.some((b) => b.name === "CONTROLLER_TOKEN" && b.type === "secret_text") ||
      authority?.type !== "service" ||
      authority.service !== authorityName ||
      authority.entrypoint !== "AWSQualificationController" ||
      authority.environment !== undefined ||
      !/^[0-9a-f]{64}$/.test(authority.props?.deploymentHash ?? "")
    )
      fail("controller authority redirect");
  } else {
    const candidate = /^crabbox-(image-qualification-[0-9]+-[0-9]+)$/.exec(worker);
    const relay = /^crabbox-image-qualification-relay-([0-9]+-[0-9]+)$/.exec(worker);
    if (!candidate && !relay) fail("unexpected Worker name");
    if (candidate && bindings.length === 0) return;
    const services = bindings.filter((b) => b.type === "service");
    const service = services[0];
    if (
      services.length !== 1 ||
      service.environment !== undefined ||
      service.entrypoint !== undefined ||
      (candidate
        ? service.name !== "CRABBOX_AWS_QUALIFICATION_TRANSPORT" ||
          service.service !== authorityName ||
          service.props?.runId !== candidate[1] ||
          service.props?.candidateWorker !== worker ||
          !/^[0-9a-f]{64}$/.test(service.props?.deploymentHash ?? "")
        : service.name !== "CANDIDATE" ||
          service.service !== `crabbox-image-qualification-${relay[1]}`) ||
      bindings.some(
        (b) =>
          ![
            "plain_text",
            "secret_text",
            "service",
            "durable_object_namespace",
            "version_metadata",
          ].includes(b.type),
      ) ||
      bindings.some(
        (b) =>
          b.type === "secret_text" &&
          !(
            candidate
              ? ["CRABBOX_ADMIN_TOKEN", "CRABBOX_SHARED_TOKEN"]
              : ["EXECUTOR_TOKEN", "CANDIDATE_ADMIN_TOKEN", "CANDIDATE_SHARED_TOKEN"]
          ).includes(b.name),
      ) ||
      bindings.some(
        (b) =>
          b.type === "durable_object_namespace" &&
          (!candidate || b.name !== "FLEET" || b.class_name !== "FleetDurableObject"),
      )
    )
      fail("candidate or relay binding redirect");
  }
}

export async function verifyOwned(cf, worker, { allowAbsent = false } = {}) {
  const settings = await cf.request(`/workers/scripts/${worker}/settings`, {}, { allow404: true });
  if (settings === undefined && allowAbsent) {
    if ((await namespaces(cf)).some((n) => n.script === worker))
      fail("namespace residue without owned Worker");
    return undefined;
  }
  assertOwnedSettings(cf, worker, settings);
  await ownedNamespaces(cf, worker, settings.bindings);
  return settings;
}
