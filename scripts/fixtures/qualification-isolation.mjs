// Synthetic cloud identities for offline isolation tests only.
import {
  bindingShape,
  canonical,
  digest,
  authorityName,
  ownershipTags,
} from "../image-qualification-isolation.mjs";
export const version = "11111111-1111-4111-8111-111111111111";
export function fixtureAdmission(accountId = "a".repeat(32), rootGB = 20) {
  const aws = {
    accountId: "123456789012",
    roleArn: "arn:aws:iam::123456789012:role/Qualification",
    region: "eu-west-1",
    subnetId: "subnet-00000000000000000",
    securityGroupId: "sg-00000000000000000",
    baseAmiId: "ami-00000000000000001",
    rootGB,
  };
  const authority = {
    name: authorityName,
    version,
    sourceSha: "c".repeat(40),
    logicalVersion: "v1",
    policyHash: "d".repeat(64),
    namespaces: [
      { id: "1".repeat(32), script: authorityName, class: "AWSQualificationRun" },
      { id: "2".repeat(32), script: authorityName, class: "AWSQualificationRegistry" },
    ],
  };
  const bindings = Object.entries({
    ACCOUNT_ID: aws.accountId,
    ROLE_ARN: aws.roleArn,
    REGION: aws.region,
    SUBNET_ID: aws.subnetId,
    SECURITY_GROUP_ID: aws.securityGroupId,
    BASE_AMI_ID: aws.baseAmiId,
    ROOT_GB: String(rootGB),
    AUTHORITY_SHA: authority.sourceSha,
    AUTHORITY_VERSION: authority.logicalVersion,
  }).map(([key, text]) => ({ name: `CRABBOX_AWS_QUALIFICATION_${key}`, type: "plain_text", text }));
  bindings.push(
    ...authority.namespaces.map((n) => ({
      name:
        n.class === "AWSQualificationRun" ? "AWS_QUALIFICATION_RUNS" : "AWS_QUALIFICATION_REGISTRY",
      type: "durable_object_namespace",
      class_name: n.class,
      namespace_id: n.id,
    })),
  );
  authority.bindingDigest = digest(canonical(bindingShape(bindings)));
  const record = {
    version: 1,
    accountId,
    authority,
    aws,
    production: {
      workers: ["production-coordinator"],
      namespaceIds: ["9".repeat(32)],
      roleArns: ["arn:aws:iam::123456789012:role/Production"],
      subnetIds: ["subnet-11111111"],
      securityGroupIds: ["sg-11111111"],
    },
  };
  const sha256 = digest(canonical(record));
  return {
    record,
    bindings,
    admission: { ...record, sha256 },
    env: {
      QUALIFICATION_ISOLATION_JSON: JSON.stringify(record),
      QUALIFICATION_ISOLATION_SHA256: sha256,
    },
  };
}
export function ownedSettings(admission, worker, bindings) {
  return { bindings, tags: ownershipTags({ admission }, worker, bindings) };
}
export const namespaceId = (worker) => digest(worker).slice(0, 32);
export function controllerContent(source) {
  const form = new FormData();
  form.set(
    "controller.mjs",
    new Blob([source], { type: "application/javascript+module" }),
    "controller.mjs",
  );
  return new Response(form);
}
