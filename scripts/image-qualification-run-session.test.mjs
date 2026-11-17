import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

import {
  reusableQualificationPolicies,
  validateRunSession,
  runSessionTags,
  runSessionPolicy,
  cleanupSessionPolicy,
  handoffSession,
  forwardActions,
  cleanupActions,
  sessionTagKeys,
} from "./image-qualification-run-session.mjs";

const config = {
  accountId: "123456789012",
  region: "us-east-1",
  roleArn: "arn:aws:iam::123456789012:role/Qualification",
  issuerArn: "arn:aws:iam::123456789012:user/QualificationIssuer",
  subnetId: "subnet-00000000000000001",
  securityGroupId: "sg-00000000000000002",
  baseAmiId: "ami-00000000000000003",
  availabilityZone: "us-east-1a",
  rootGB: 20,
};
const scope = {
  profile: "first-walk-aws-session/v1",
  repository: "example/repo",
  workflowSha: "a".repeat(40),
  candidateSha: "b".repeat(40),
  runId: "image-qualification-42-1",
  candidateArtifactId: "9",
  candidateArtifactDigest: `sha256:${"c".repeat(64)}`,
  manifestSha256: "d".repeat(64),
  startsAt: "2030-01-01T00:00:00.000Z",
  workExpiresAt: "2030-01-01T02:10:00.000Z",
  expiresAt: "2030-01-01T02:30:00.000Z",
  recoveryExpiresAt: "2030-01-01T04:30:00.000Z",
};
const fixed = reusableQualificationPolicies(config);
const all = fixed.policies.flatMap((p) => p.Statement);
const tags = runSessionTags(scope);
const context = Object.fromEntries(
  Object.entries(tags).flatMap(([k, v]) => [
    [`aws:PrincipalTag/${k}`, v],
    [`ec2:ResourceTag/${k}`, v],
    [`aws:RequestTag/${k}`, v],
  ]),
);
Object.assign(context, {
  "aws:RequestedRegion": config.region,
  "aws:CurrentTime": "2030-01-01T01:00:00.000Z",
});
const arn = (type, id = "test", account = config.accountId) =>
  `arn:aws:ec2:${config.region}:${account}:${type}/${id}`;
const list = (v) => (Array.isArray(v) ? v : [v]);
const glob = (p, v) =>
  new RegExp(
    `^${p
      .replace(/[.+^${}()|[\]\\]/g, "\\$&")
      .replaceAll("*", ".*")
      .replaceAll("?", ".")}$`,
  ).test(v);
// Deliberately small independent evaluator for generated constructs only. It is
// not an AWS simulator: later AWS policy validation and DryRun remain required.
function matches(s, action, resource, ctx) {
  const resolve = (v) =>
    typeof v === "string" ? v.replace(/\$\{([^}]+)\}/g, (_, k) => ctx[k] ?? "MISSING") : String(v);
  if (
    !list(s.Action).some((v) => glob(v, action)) ||
    !list(s.Resource).some((v) => glob(v, resource))
  )
    return false;
  return Object.entries(s.Condition ?? {}).every(([op, keys]) =>
    Object.entries(keys).every(([k, value]) => {
      const actual = ctx[k],
        values = list(value).map(resolve);
      switch (op) {
        case "Null":
          return String(actual === undefined) === values[0];
        case "StringEquals":
          return actual !== undefined && values.includes(String(actual));
        case "StringNotEquals":
          return actual === undefined || !values.includes(String(actual));
        case "Bool":
          return actual !== undefined && values.includes(String(actual));
        case "NumericLessThanEquals":
          return actual !== undefined && Number(actual) <= Number(values[0]);
        case "NumericEquals":
          return actual !== undefined && Number(actual) === Number(values[0]);
        case "DateLessThan":
          return actual !== undefined && Date.parse(actual) < Date.parse(values[0]);
        case "DateGreaterThanEquals":
          return actual !== undefined && Date.parse(actual) >= Date.parse(values[0]);
        default:
          throw Error(`unsupported evaluator operator ${op}`);
      }
    }),
  );
}
function allowed(action, resources, ctx = context) {
  return resources.every((resource) => {
    const role = all.filter((s) => matches(s, action, resource, ctx));
    const session = runSessionPolicy(scope).Statement.filter((s) =>
      matches(s, action, resource, ctx),
    );
    return [role, session].every(
      (ss) =>
        ss.some((s) => s.Effect === "Allow") &&
        !ss.some((statement) => statement.Effect === "Deny"),
    );
  });
}

test("deterministic reusable policy packing and compact session size", () => {
  assert.deepEqual(reusableQualificationPolicies(config), fixed);
  assert.ok(fixed.policies.length <= 4);
  for (const p of fixed.policies) assert.ok(JSON.stringify(p).length <= 6144);
  assert.ok(JSON.stringify(runSessionPolicy(scope)).length <= 2048);
  assert.equal(fixed.trust.Statement[0].Principal.AWS, config.issuerArn);
  assert.equal(fixed.trust.Statement[0].Action, "sts:AssumeRole");
  assert.equal(fixed.trust.Statement[1].Action, "sts:TagSession");
  assert.deepEqual(
    fixed.trust.Statement[0].Condition["ForAllValues:StringEquals"]["aws:TagKeys"],
    sessionTagKeys,
  );
  assert.equal(fixed.issuer.Statement[0].Resource, config.roleArn);
  assert.doesNotMatch(JSON.stringify(fixed), /iam:(?:Create|Put|Attach|Pass)/);
});

test("cleanup-only intersection retains original tags/clocks and denies every forward action", () => {
  const normal = runSessionPolicy(scope),
    cleanup = cleanupSessionPolicy(scope);
  assert.deepEqual(cleanup.Statement.slice(0, -1), normal.Statement);
  assert.deepEqual(cleanup.Statement.at(-1), {
    Effect: "Deny",
    Action: forwardActions,
    Resource: "*",
  });
  assert.ok(JSON.stringify(cleanup).length <= 2048);
  for (const action of forwardActions)
    assert.ok(
      cleanup.Statement.some((s) => s.Effect === "Deny" && matches(s, action, "*", context)),
    );
  for (const action of cleanupActions)
    assert.ok(
      !cleanup.Statement.some((s) => s.Effect === "Deny" && matches(s, action, "*", context)),
    );
  for (const at of ["2029-12-31T23:59:59.999Z", scope.recoveryExpiresAt])
    for (const action of cleanupActions)
      assert.ok(
        cleanup.Statement.some(
          (s) =>
            s.Effect === "Deny" && matches(s, action, "*", { ...context, "aws:CurrentTime": at }),
        ),
      );
  assert.deepEqual(runSessionPolicy(scope), normal, "normal issuance is unchanged");
});

test("all generated EC2 allow conditions exist for each action/resource in AWS reference", () => {
  const reference = JSON.parse(
    fs.readFileSync(new URL("./image-qualification-ec2-authorization.json", import.meta.url)),
  );
  for (const s of all.filter((statement) => statement.Effect === "Allow"))
    for (const resource of list(s.Resource)) {
      const type = resource.split(":").at(-1).split("/")[0];
      const a = reference.actions[s.Action.slice(4)];
      assert.ok(a?.resources[type], `${s.Sid} ${type}`);
      const supported = [...a.conditions, ...a.resources[type]];
      for (const keys of Object.values(s.Condition))
        for (const key of Object.keys(keys)) {
          if (key.startsWith("aws:")) continue;
          assert.ok(
            supported.some((v) => v === key || v === key.replace(/\/[^/]+$/, "/${TagKey}")),
            `${s.Sid} ${type} ${key}`,
          );
        }
    }
});

for (const [action, type, account] of [
  ["ec2:ImportKeyPair", "key-pair", config.accountId],
  ...cleanupActions.map((a, i) => [
    a,
    ["instance", "volume", "key-pair", "image", "snapshot"][i],
    i > 2 ? "" : config.accountId,
  ]),
])
  test(`${action} same-run allowed; each missing/foreign ownership field rejected`, () => {
    const resources = [arn(type, "test", account)];
    assert.equal(allowed(action, resources), true);
    const prefix = action === "ec2:ImportKeyPair" ? "aws:RequestTag" : "ec2:ResourceTag";
    for (const key of sessionTagKeys) {
      for (const changed of [undefined, "foreign"]) {
        const bad = { ...context, [`${prefix}/${key}`]: changed };
        assert.equal(allowed(action, resources, bad), false, `${prefix}/${key}`);
      }
      assert.equal(
        allowed(action, resources, { ...context, [`aws:PrincipalTag/${key}`]: undefined }),
        false,
      );
    }
  });

test("multi-resource attach and launch require every supported resource check", () => {
  const attach = { ...context, "ec2:ResourceTag/crabbox_qualification_op": "first-walk-data" };
  assert.equal(allowed("ec2:AttachVolume", [arn("instance"), arn("volume")], attach), true);
  assert.equal(
    allowed("ec2:AttachVolume", [arn("instance"), arn("volume", "test", "999999999999")], attach),
    false,
  );
  assert.equal(allowed("ec2:AttachVolume", [arn("instance"), arn("volume")]), false);
  const launch = {
    ...context,
    "ec2:Subnet": arn("subnet", config.subnetId),
    "ec2:AssociatePublicIpAddress": "true",
    "ec2:InstanceType": "t3.small",
    "ec2:MetadataHttpTokens": "required",
    "ec2:Encrypted": "true",
    "ec2:VolumeType": "gp3",
    "ec2:VolumeSize": 20,
  };
  const resources = [
    arn("image", config.baseAmiId, ""),
    arn("subnet", config.subnetId),
    arn("security-group", config.securityGroupId),
    arn("network-interface"),
    arn("instance"),
    arn("volume"),
    arn("key-pair"),
  ];
  assert.equal(allowed("ec2:RunInstances", resources, launch), true);
  for (const [key, value] of [
    ["ec2:InstanceType", "m8i.32xlarge"],
    ["ec2:Encrypted", "false"],
    ["ec2:InstanceProfile", "foreign"],
    ["ec2:VolumeSize", 21],
    ["ec2:MetadataHttpTokens", "optional"],
    ["ec2:Subnet", arn("subnet", "foreign")],
  ]) {
    assert.equal(allowed("ec2:RunInstances", resources, { ...launch, [key]: value }), false, key);
  }
  assert.equal(
    allowed("ec2:RunInstances", [...resources, arn("image", "foreign", "")], launch),
    false,
  );
});

// Standalone CreateTags has an action-specific context: global resource tags
// are present while EC2-prefixed resource tags and ec2:CreateAction are absent.
const standaloneTagContext = Object.fromEntries(
  Object.entries(context).filter(([key]) => !key.startsWith("ec2:ResourceTag/")),
);
Object.assign(
  standaloneTagContext,
  Object.fromEntries(Object.entries(tags).map(([key, value]) => [`aws:ResourceTag/${key}`, value])),
);

for (const [type, account] of [
  ["key-pair", config.accountId],
  ["instance", config.accountId],
  ["volume", config.accountId],
  ["image", ""],
  ["snapshot", ""],
])
  test(`standalone CreateTags ${type} requires matching global resource, principal and request identity`, () => {
    const resources = [arn(type, "test", account)];
    assert.equal(allowed("ec2:CreateTags", resources, standaloneTagContext), true);
    for (const prefix of ["aws:PrincipalTag", "aws:RequestTag", "aws:ResourceTag"])
      for (const key of sessionTagKeys)
        for (const value of [undefined, "foreign"])
          assert.equal(
            allowed("ec2:CreateTags", resources, {
              ...standaloneTagContext,
              [`${prefix}/${key}`]: value,
            }),
            false,
            `${prefix}/${key}=${value}`,
          );
    // EC2-prefixed values cannot substitute for missing/mismatched global tags.
    for (const key of sessionTagKeys)
      for (const value of [undefined, "foreign"])
        assert.equal(
          allowed("ec2:CreateTags", resources, {
            ...context,
            ...standaloneTagContext,
            [`aws:ResourceTag/${key}`]: value,
          }),
          false,
        );
    assert.equal(
      allowed("ec2:CreateTags", [arn(type, "test", "999999999999")], standaloneTagContext),
      false,
    );
    assert.equal(allowed("ec2:DeleteTags", resources, standaloneTagContext), false);
    for (const existing of [undefined, "other", "first-walk-data"])
      for (const requested of [undefined, "other", "first-walk-data"])
        assert.equal(
          allowed("ec2:CreateTags", resources, {
            ...standaloneTagContext,
            "aws:ResourceTag/crabbox_qualification_op": existing,
            "aws:RequestTag/crabbox_qualification_op": requested,
          }),
          (existing === "first-walk-data") === (requested === "first-walk-data"),
          `marker ${existing} -> ${requested}`,
        );
  });

test("standalone CreateTags keeps creation contexts, session deadlines and cleanup distinct", () => {
  const resources = [arn("key-pair")];
  assert.equal(
    allowed("ec2:CreateTags", resources, {
      ...standaloneTagContext,
      "ec2:CreateAction": "UnknownAction",
    }),
    false,
  );
  for (const at of [scope.startsAt, "2030-01-01T02:09:59.999Z"])
    assert.equal(
      allowed("ec2:CreateTags", resources, { ...standaloneTagContext, "aws:CurrentTime": at }),
      true,
    );
  for (const at of [scope.workExpiresAt, scope.expiresAt]) {
    assert.equal(
      allowed("ec2:CreateTags", resources, { ...standaloneTagContext, "aws:CurrentTime": at }),
      false,
    );
    // Cleanup retains its own action-specific EC2 resource-tag conditions.
    assert.equal(
      allowed("ec2:DeleteKeyPair", resources, { ...context, "aws:CurrentTime": at }),
      true,
    );
  }
  for (const at of ["2029-12-31T23:59:59.999Z", scope.recoveryExpiresAt])
    assert.equal(
      allowed("ec2:CreateTags", resources, { ...standaloneTagContext, "aws:CurrentTime": at }),
      false,
    );
  assert.equal(
    allowed("ec2:CreateTags", resources, {
      ...standaloneTagContext,
      "aws:RequestedRegion": "eu-west-1",
    }),
    false,
  );
});

test("CreateTags resource reference includes both documented resource-tag namespaces", () => {
  const reference = JSON.parse(
    fs.readFileSync(new URL("./image-qualification-ec2-authorization.json", import.meta.url)),
  );
  for (const keys of Object.values(reference.actions.CreateTags.resources))
    for (const key of [
      "aws:RequestTag/${TagKey}",
      "aws:ResourceTag/${TagKey}",
      "ec2:ResourceTag/${TagKey}",
    ])
      assert.ok(keys.includes(key), key);
});

test("deadline ends forward work while same-run independent cleanup remains possible", () => {
  const key = [arn("key-pair")];
  for (const at of [scope.startsAt, "2030-01-01T02:09:59.999Z"])
    assert.equal(allowed("ec2:ImportKeyPair", key, { ...context, "aws:CurrentTime": at }), true);
  for (const at of [scope.workExpiresAt, scope.expiresAt]) {
    const ctx = { ...context, "aws:CurrentTime": at };
    assert.equal(allowed("ec2:ImportKeyPair", key, ctx), false);
    assert.equal(allowed("ec2:DeleteKeyPair", key, ctx), true);
  }
  for (const at of ["2029-12-31T23:59:59.999Z", scope.recoveryExpiresAt])
    assert.equal(allowed("ec2:DeleteKeyPair", key, { ...context, "aws:CurrentTime": at }), false);
  assert.equal(
    allowed("ec2:DeleteKeyPair", key, { ...context, "aws:RequestedRegion": "eu-west-1" }),
    false,
  );
  for (const action of ["iam:PutRolePolicy", "sts:AssumeRole", "sts:TagSession"])
    assert.equal(allowed(action, ["*"]), false);
  assert.deepEqual(runSessionPolicy(scope).Statement[1].Action, forwardActions);
});

test("non-first-walk modes ignore a shared first-walk session descriptor", () => {
  for (const mode of ["mint", "retained"])
    for (const value of [JSON.stringify(scope), "stale-or-malformed", "", undefined])
      assert.equal(
        handoffSession({ QUALIFICATION_MODE: mode, QUALIFICATION_AWS_SESSION_JSON: value }),
        undefined,
      );
});

test("malformed scopes, altered deadlines and mismatched artifact/handoff fail closed", () => {
  const id = {
    repository: scope.repository,
    workflowSha: scope.workflowSha,
    candidateSha: scope.candidateSha,
    runId: "42",
    artifactId: scope.candidateArtifactId,
    artifactDigest: scope.candidateArtifactDigest,
  };
  const env = {
    QUALIFICATION_MODE: "first-walk",
    GITHUB_RUN_ATTEMPT: "1",
    QUALIFICATION_AWS_SESSION_JSON: JSON.stringify(scope),
  };
  assert.equal(handoffSession(env, id, scope.manifestSha256).scope.runId, scope.runId);
  for (const key of Object.keys(scope)) {
    const broken = { ...scope };
    delete broken[key];
    assert.throws(() => validateRunSession(broken));
  }
  for (const change of [
    { runId: "image-qualification-*-1" },
    { runId: "image-qualification-42-2" },
    { candidateSha: "*".repeat(40) },
    { recoveryExpiresAt: scope.expiresAt },
    { unexpected: true },
  ])
    assert.throws(() => validateRunSession({ ...scope, ...change }));
  for (const change of [
    { repository: "foreign/repo" },
    { candidateSha: "f".repeat(40) },
    { artifactId: "10" },
    { workflowSha: "f".repeat(40) },
    { artifactDigest: `sha256:${"e".repeat(64)}` },
  ])
    assert.throws(() => handoffSession(env, { ...id, ...change }, scope.manifestSha256));
  assert.throws(() =>
    handoffSession({ ...env, QUALIFICATION_AWS_SESSION_JSON: "" }, id, scope.manifestSha256),
  );
  assert.throws(() => reusableQualificationPolicies({ ...config, rootGB: 21 }));
  assert.throws(() =>
    reusableQualificationPolicies({
      ...config,
      issuerArn: "arn:aws:iam::123456789012:role/Foreign",
    }),
  );
});

test("capture, snapshot-backed copy and attribute policies authorize only constrained requests", () => {
  assert.equal(
    allowed("ec2:CreateImage", [
      arn("instance"),
      arn("image", "new", ""),
      arn("snapshot", "new", ""),
    ]),
    true,
  );
  const copy = {
    ...context,
    "ec2:Owner": config.accountId,
    "ec2:Encrypted": "true",
    "ec2:VolumeSize": 20,
    "ec2:VolumeType": "gp3",
    "ec2:AvailabilityZone": config.availabilityZone,
    "ec2:VolumeIops": 3000,
    "ec2:VolumeThroughput": 125,
    "ec2:ParentSnapshot": "snap-test",
    "aws:RequestTag/crabbox_qualification_op": "first-walk-data",
  };
  const resources = [arn("snapshot", "source", ""), arn("volume", "copy")];
  assert.equal(allowed("ec2:CreateVolume", resources, copy), true);
  for (const [key, value] of [
    ["ec2:Owner", "999999999999"],
    ["ec2:Encrypted", "false"],
    ["ec2:VolumeSize", 21],
    ["ec2:VolumeType", "io2"],
    ["ec2:VolumeInitializationRate", 100],
    ["ec2:AvailabilityZone", "us-east-1b"],
    ["ec2:ParentSnapshot", undefined],
    ["ec2:VolumeIops", 4000],
    ["ec2:VolumeThroughput", 500],
    ["aws:RequestTag/crabbox_qualification_run", "foreign"],
    ["ec2:ResourceTag/crabbox_qualification_run", "foreign"],
  ]) {
    assert.equal(allowed("ec2:CreateVolume", resources, { ...copy, [key]: value }), false, key);
  }
  // Real block-device requests have no usable ec2:Attribute context. IAM is
  // ownership-scoped; the unchanged typed authority restricts request contents.
  assert.equal(allowed("ec2:ModifyInstanceAttribute", [arn("instance")]), true);
  assert.equal(
    allowed("ec2:ModifyInstanceAttribute", [arn("instance")], {
      ...context,
      "ec2:ResourceTag/crabbox_qualification_run": "foreign",
    }),
    false,
  );
  const attribute = all.find((statement) => statement.Sid === "OwnedInstanceAttribute");
  assert.equal(attribute.Condition.StringEquals["ec2:Attribute"], undefined);
  for (const create of ["RunInstances", "CreateImage", "ImportKeyPair", "CreateVolume"]) {
    assert.equal(
      allowed("ec2:CreateTags", [arn("volume")], { ...context, "ec2:CreateAction": create }),
      true,
    );
    assert.equal(
      allowed("ec2:CreateTags", [arn("volume")], {
        ...context,
        "ec2:CreateAction": create,
        "aws:RequestTag/crabbox_qualification_run": "foreign",
      }),
      false,
    );
  }
});

test("trust uses action-supported keys and admits only the issuer with complete exact tag keys", () => {
  const reference = JSON.parse(
    fs.readFileSync(new URL("./image-qualification-ec2-authorization.json", import.meta.url)),
  );
  for (const statement of fixed.trust.Statement) {
    const supported = reference.stsActions[statement.Action.slice(4)];
    assert.ok(supported);
    for (const keys of Object.values(statement.Condition))
      for (const key of Object.keys(keys)) {
        assert.ok(
          supported.includes(key) || supported.includes(key.replace(/\/[^/]+$/, "/${TagKey}")),
          `${statement.Action} ${key}`,
        );
      }
    assert.equal(statement.Principal.AWS, config.issuerArn);
    assert.equal(statement.Condition.Null["sts:TransitiveTagKeys"], "true");
    assert.deepEqual(
      Object.keys(statement.Condition.Null)
        .filter((k) => k.startsWith("aws:RequestTag/"))
        .toSorted(),
      sessionTagKeys.map((k) => `aws:RequestTag/${k}`).toSorted(),
    );
  }
  assert.equal(
    fixed.trust.Statement[0].Condition.StringEquals["sts:RoleSessionName"],
    "${aws:RequestTag/crabbox_qualification_run}",
  );
  assert.equal(fixed.trust.Statement[1].Condition.StringEquals["sts:RoleSessionName"], undefined);
});
