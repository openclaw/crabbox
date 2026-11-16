// Qualification-only, credential-free policy and scope construction. No AWS effects.
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { canonical, digest } from "./image-qualification-isolation.mjs";

export const sessionTagKeys = ["run", "owner", "sha", "expiry"].map(
  (key) => `crabbox_qualification_${key}`,
);
export const forwardActions = [
  "RunInstances",
  "ImportKeyPair",
  "CreateImage",
  "CreateVolume",
  "AttachVolume",
  "ModifyInstanceAttribute",
  "CreateTags",
].map((action) => `ec2:${action}`);
export const cleanupActions = [
  "TerminateInstances",
  "DeleteVolume",
  "DeleteKeyPair",
  "DeregisterImage",
  "DeleteSnapshot",
].map((action) => `ec2:${action}`);
const iso = (value) =>
  typeof value === "string" &&
  Number.isFinite(Date.parse(value)) &&
  new Date(value).toISOString() === value;
const equal = (a, b) => canonical(a) === canonical(b);
const refuse = () => {
  throw new Error("qualification AWS run scope is invalid");
};

// Private protected input, never candidate-provided or an operational repo record.
export function validateRunSession(scope) {
  const fields = [
    "profile",
    "repository",
    "workflowSha",
    "candidateSha",
    "runId",
    "candidateArtifactId",
    "candidateArtifactDigest",
    "manifestSha256",
    "startsAt",
    "workExpiresAt",
    "expiresAt",
    "recoveryExpiresAt",
  ];
  if (
    !scope ||
    !equal(Object.keys(scope).toSorted(), fields.toSorted()) ||
    scope.profile !== "first-walk-aws-session/v1" ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(scope.repository ?? "") ||
    ![scope.workflowSha, scope.candidateSha].every((v) => /^[0-9a-f]{40}$/.test(v ?? "")) ||
    !/^image-qualification-[1-9][0-9]*-1$/.test(scope.runId ?? "") ||
    scope.runId.length > 64 ||
    !/^[1-9][0-9]*$/.test(scope.candidateArtifactId ?? "") ||
    !/^sha256:[0-9a-f]{64}$/.test(scope.candidateArtifactDigest ?? "") ||
    !/^[0-9a-f]{64}$/.test(scope.manifestSha256 ?? "") ||
    ![scope.startsAt, scope.workExpiresAt, scope.expiresAt, scope.recoveryExpiresAt].every(iso) ||
    Date.parse(scope.workExpiresAt) !== Date.parse(scope.startsAt) + 130 * 60_000 ||
    Date.parse(scope.expiresAt) !== Date.parse(scope.startsAt) + 150 * 60_000 ||
    Date.parse(scope.recoveryExpiresAt) !== Date.parse(scope.startsAt) + 270 * 60_000
  )
    refuse();
  return scope;
}

export function runSessionTags(scope) {
  validateRunSession(scope);
  return Object.fromEntries(
    sessionTagKeys.map((key, i) => [
      key,
      [scope.runId, `${scope.runId}@example.invalid`, scope.candidateSha, scope.expiresAt][i],
    ]),
  );
}

export function runSessionPolicy(scope) {
  validateRunSession(scope);
  const policy = {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Action: [
          ...forwardActions,
          ...cleanupActions,
          "ec2:Describe*",
          "ec2:GetEbsEncryptionByDefault",
          "ec2:GetEbsDefaultKmsKeyId",
          "servicequotas:GetServiceQuota",
          "sts:GetCallerIdentity",
        ],
        Resource: "*",
      },
      {
        Effect: "Deny",
        Action: forwardActions,
        Resource: "*",
        Condition: { DateGreaterThanEquals: { "aws:CurrentTime": scope.workExpiresAt } },
      },
      {
        Effect: "Deny",
        Action: "*",
        Resource: "*",
        Condition: { DateLessThan: { "aws:CurrentTime": scope.startsAt } },
      },
      {
        Effect: "Deny",
        Action: "*",
        Resource: "*",
        Condition: { DateGreaterThanEquals: { "aws:CurrentTime": scope.recoveryExpiresAt } },
      },
    ],
  };
  if (JSON.stringify(policy).length > 2048) refuse();
  return policy;
}

export function handoffSession(env, identity, manifestSha256) {
  if (env.QUALIFICATION_MODE !== "first-walk") {
    if (env.QUALIFICATION_AWS_SESSION_JSON) refuse();
    return undefined;
  }
  const scope = validateRunSession(JSON.parse(env.QUALIFICATION_AWS_SESSION_JSON ?? "null"));
  if (
    scope.repository !== identity.repository ||
    scope.workflowSha !== identity.workflowSha ||
    scope.candidateSha !== identity.candidateSha ||
    scope.runId !== `image-qualification-${identity.runId}-${env.GITHUB_RUN_ATTEMPT}` ||
    scope.candidateArtifactId !== identity.artifactId ||
    scope.candidateArtifactDigest !== identity.artifactDigest ||
    scope.manifestSha256 !== manifestSha256
  )
    refuse();
  return { scope, sha256: digest(canonical(scope)) };
}

// Install outputs only after separate review/admission. No operational defaults.
export function reusableQualificationPolicies(config) {
  const keys = [
    "accountId",
    "region",
    "roleArn",
    "issuerArn",
    "subnetId",
    "securityGroupId",
    "baseAmiId",
    "availabilityZone",
    "rootGB",
  ];
  if (
    !config ||
    !equal(Object.keys(config).toSorted(), keys.toSorted()) ||
    !/^[0-9]{12}$/.test(config.accountId) ||
    !/^[a-z]{2}-[a-z]+-[0-9]+$/.test(config.region) ||
    !/^subnet-[0-9a-f]{17}$/.test(config.subnetId) ||
    !/^sg-[0-9a-f]{17}$/.test(config.securityGroupId) ||
    !/^ami-[0-9a-f]{17}$/.test(config.baseAmiId) ||
    !Number.isSafeInteger(config.rootGB) ||
    config.rootGB < 8 ||
    config.rootGB > 20 ||
    !new RegExp(`^${config.region}[a-z]$`).test(config.availabilityZone) ||
    !new RegExp(`^arn:aws:iam::${config.accountId}:role/[A-Za-z0-9_+=,.@/-]+$`).test(
      config.roleArn,
    ) ||
    !new RegExp(`^arn:aws:iam::${config.accountId}:user/[A-Za-z0-9_+=,.@/-]+$`).test(
      config.issuerArn,
    )
  )
    refuse();
  const c = config;
  const arn = (type, id = "*", account = c.accountId) =>
    `arn:aws:ec2:${c.region}:${account}:${type}/${id}`;
  const principal = (key) => `\${aws:PrincipalTag/${key}}`;
  const identity = (prefix) =>
    Object.fromEntries(sessionTagKeys.map((key) => [`${prefix}/${key}`, principal(key)]));
  const request = identity("aws:RequestTag"),
    owned = identity("ec2:ResourceTag");
  const region = { "aws:RequestedRegion": c.region };
  const statements = [];
  const allow = (Sid, Action, Resource, values = {}, extra = {}) =>
    statements.push({
      Sid,
      Effect: "Allow",
      Action: `ec2:${Action}`,
      Resource,
      Condition: { StringEquals: { ...region, ...values }, ...extra },
    });
  allow("StockNetworkImage", "RunInstances", [
    arn("image", c.baseAmiId, ""),
    arn("subnet", c.subnetId),
    arn("security-group", c.securityGroupId),
  ]);
  allow("PhysicalKey", "RunInstances", arn("key-pair"), owned);
  allow(
    "NewInterface",
    "RunInstances",
    arn("network-interface"),
    { "ec2:Subnet": arn("subnet", c.subnetId) },
    { Bool: { "ec2:AssociatePublicIpAddress": "true" } },
  );
  allow(
    "SmallInstance",
    "RunInstances",
    arn("instance"),
    {
      ...request,
      "ec2:InstanceType": ["t3.small", "t3a.small"],
      "ec2:MetadataHttpTokens": "required",
    },
    { Null: { "ec2:InstanceProfile": "true" } },
  );
  allow(
    "EncryptedRoot",
    "RunInstances",
    arn("volume"),
    { ...request, "ec2:VolumeType": "gp3" },
    { Bool: { "ec2:Encrypted": "true" }, NumericLessThanEquals: { "ec2:VolumeSize": c.rootGB } },
  );
  allow("NewKey", "ImportKeyPair", arn("key-pair"), request);
  allow("CaptureSource", "CreateImage", arn("instance"), owned);
  allow(
    "CaptureOutputs",
    "CreateImage",
    [arn("image", "*", ""), arn("snapshot", "*", "")],
    request,
  );
  allow(
    "OwnedSnapshot",
    "CreateVolume",
    arn("snapshot", "*", ""),
    { ...owned, "ec2:Owner": c.accountId },
    { Bool: { "ec2:Encrypted": "true" }, NumericLessThanEquals: { "ec2:VolumeSize": c.rootGB } },
  );
  allow(
    "DataCopy",
    "CreateVolume",
    arn("volume"),
    {
      ...request,
      "aws:RequestTag/crabbox_qualification_op": "first-walk-data",
      "ec2:AvailabilityZone": c.availabilityZone,
      "ec2:VolumeType": "gp3",
    },
    {
      Bool: { "ec2:Encrypted": "true" },
      NumericLessThanEquals: { "ec2:VolumeSize": c.rootGB },
      NumericEquals: { "ec2:VolumeIops": 3000, "ec2:VolumeThroughput": 125 },
      Null: { "ec2:VolumeInitializationRate": "true", "ec2:ParentSnapshot": "false" },
    },
  );
  allow("AttachInstance", "AttachVolume", arn("instance"), owned);
  allow("AttachCopy", "AttachVolume", arn("volume"), {
    ...owned,
    "ec2:ResourceTag/crabbox_qualification_op": "first-walk-data",
  });
  allow("DeleteOnTermination", "ModifyInstanceAttribute", arn("instance"), {
    ...owned,
    "ec2:Attribute": "blockDeviceMapping",
  });
  const tagged = [
    arn("instance"),
    arn("volume"),
    arn("key-pair"),
    arn("image", "*", ""),
    arn("snapshot", "*", ""),
  ];
  allow("CreateTimeTags", "CreateTags", tagged, {
    ...request,
    "ec2:CreateAction": ["RunInstances", "ImportKeyPair", "CreateImage", "CreateVolume"],
  });
  // Four ownership fields can only be restated, never replaced. The typed
  // authority still injects its operation ID and checks exact ledger targets.
  allow(
    "RestateOwnedTags",
    "CreateTags",
    tagged,
    { ...owned, ...request },
    { Null: { "ec2:CreateAction": "true" } },
  );
  for (const [action, type, account] of [
    ["TerminateInstances", "instance", c.accountId],
    ["DeleteVolume", "volume", c.accountId],
    ["DeleteKeyPair", "key-pair", c.accountId],
    ["DeregisterImage", "image", ""],
    ["DeleteSnapshot", "snapshot", ""],
  ])
    allow(`Cleanup${action}`, action, arn(type, "*", account), owned);

  const guard = [
    ...sessionTagKeys.map((key, i) => ({
      Sid: `MissingIdentity${i}`,
      Effect: "Deny",
      Action: "*",
      Resource: "*",
      Condition: { Null: { [`aws:PrincipalTag/${key}`]: "true" } },
    })),
    {
      Sid: "Region",
      Effect: "Deny",
      Action: ["ec2:*", "servicequotas:*"],
      Resource: "*",
      Condition: { StringNotEquals: { "aws:RequestedRegion": c.region } },
    },
    {
      Sid: "NoDelegationOrTagRemoval",
      Effect: "Deny",
      Resource: "*",
      Action: ["iam:*", "sts:AssumeRole*", "sts:TagSession", "ec2:DeleteTags"],
    },
    // Never let general retagging create or destroy the data-copy marker.
    {
      Sid: "NoPromotingDataCopy",
      Effect: "Deny",
      Action: "ec2:CreateTags",
      Resource: tagged,
      Condition: {
        Null: { "ec2:CreateAction": "true" },
        StringEquals: { "aws:RequestTag/crabbox_qualification_op": "first-walk-data" },
        StringNotEquals: { "ec2:ResourceTag/crabbox_qualification_op": "first-walk-data" },
      },
    },
    {
      Sid: "PreserveDataCopy",
      Effect: "Deny",
      Action: "ec2:CreateTags",
      Resource: tagged,
      Condition: {
        StringEquals: { "ec2:ResourceTag/crabbox_qualification_op": "first-walk-data" },
        StringNotEquals: { "aws:RequestTag/crabbox_qualification_op": "first-walk-data" },
      },
    },
  ];
  const policies = [];
  // Deterministic whole-statement packing; managed policies allow 6,144 chars.
  for (const statement of [...guard, ...statements]) {
    let policy = policies.at(-1);
    if (
      !policy ||
      JSON.stringify({ ...policy, Statement: [...policy.Statement, statement] }).length > 6144
    ) {
      policy = { Version: "2012-10-17", Statement: [] };
      policies.push(policy);
    }
    policy.Statement.push(statement);
    if (JSON.stringify(policy).length > 6144) refuse();
  }
  const trust = {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Principal: { AWS: c.issuerArn },
        Action: "sts:AssumeRole",
        Condition: {
          "ForAllValues:StringEquals": { "aws:TagKeys": sessionTagKeys },
          Null: {
            ...Object.fromEntries(sessionTagKeys.map((key) => [`aws:RequestTag/${key}`, "false"])),
            "sts:TransitiveTagKeys": "true",
          },
          StringLike: {
            "aws:RequestTag/crabbox_qualification_run": "image-qualification-*-1",
            "aws:RequestTag/crabbox_qualification_sha": "?".repeat(40),
            "aws:RequestTag/crabbox_qualification_expiry": "????-??-??T??:??:??.???Z",
          },
          StringEquals: {
            "sts:RoleSessionName": "${aws:RequestTag/crabbox_qualification_run}",
            "aws:RequestTag/crabbox_qualification_owner":
              "${aws:RequestTag/crabbox_qualification_run}@example.invalid",
          },
        },
      },
    ],
  };
  // TagSession has no sts:RoleSessionName condition key. Check that only on
  // AssumeRole; the permission-only action retains all required tag constraints.
  const tagTrust = structuredClone(trust.Statement[0]);
  tagTrust.Action = "sts:TagSession";
  delete tagTrust.Condition.StringEquals["sts:RoleSessionName"];
  trust.Statement.push(tagTrust);
  const issuer = {
    Version: "2012-10-17",
    Statement: [
      { Effect: "Allow", Action: ["sts:AssumeRole", "sts:TagSession"], Resource: c.roleArn },
    ],
  };
  return {
    profile: "reusable-qualification-permissions/v1",
    configSha256: digest(canonical(c)),
    policies,
    trust,
    issuer,
  };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  // Only fixed nonsecret configuration is accepted; output contains no credentials.
  if (process.argv.length !== 3) throw new Error("expected one fixed policy configuration file");
  console.log(
    JSON.stringify(
      reusableQualificationPolicies(JSON.parse(fs.readFileSync(process.argv[2], "utf8"))),
      null,
      2,
    ),
  );
}
