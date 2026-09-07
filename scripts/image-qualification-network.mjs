import { execFileSync } from "node:child_process";
import { isIPv4 } from "node:net";

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const nativeCallTimeoutMs = 25_000;

function tags(receipt) {
  return {
    crabbox_qualification_run: receipt.runId,
    crabbox_qualification_attempt: receipt.attempt,
    crabbox_qualification_owner: receipt.owner,
    crabbox_qualification_sha: receipt.candidateSha,
    crabbox_qualification_expiry: receipt.expiresAt,
    crabbox_qualification_network: receipt.attemptId,
  };
}

function validate(receipt) {
  if (
    !/^[0-9]{12}$/.test(receipt.accountId) ||
    !/^[a-z]{2}-[a-z]+-[0-9]+$/.test(receipt.region) ||
    !/^sg-[0-9a-f]+$/.test(receipt.securityGroupId) ||
    !/^image-qualification-[0-9]+-[0-9]+$/.test(receipt.runId) ||
    typeof receipt.attempt !== "string" ||
    !/^[1-9][0-9]*$/.test(receipt.attempt) ||
    !receipt.runId.endsWith(`-${receipt.attempt}`) ||
    typeof receipt.owner !== "string" ||
    !receipt.owner ||
    receipt.owner.length > 256 ||
    !/^[0-9a-f]{40}$/.test(receipt.candidateSha) ||
    !/^[0-9a-f]{64}$/.test(receipt.attemptId) ||
    !isIPv4(receipt.ipv4) ||
    !Number.isFinite(Date.parse(receipt.expiresAt)) ||
    !Number.isFinite(Date.parse(receipt.cleanupNotAfter)) ||
    Date.parse(receipt.cleanupNotAfter) <= Date.parse(receipt.expiresAt)
  ) {
    throw new Error("invalid protected network receipt");
  }
}

// Only protected jobs receive these short-lived session credentials. The native CLI
// never loads a profile/cache or emits its raw response/error into public evidence.
export function networkCLI(env = process.env, minimumExpiry = Date.now() + 60_000) {
  for (const name of ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"]) {
    if (!env[name]) throw new Error("protected network session credentials are absent");
  }
  const expiration = Date.parse(env.QUALIFICATION_NETWORK_CREDENTIAL_EXPIRES_AT ?? "");
  if (!Number.isFinite(expiration) || expiration < minimumExpiry)
    throw new Error("protected network credential expiry is insufficient");
  return async (service, operation, input, region) => {
    if (
      !new Set([
        "sts:get-caller-identity",
        "ec2:describe-security-group-rules",
        "ec2:authorize-security-group-ingress",
        "ec2:revoke-security-group-ingress",
      ]).has(`${service}:${operation}`)
    )
      throw new Error("network operation is not permitted");
    if (Date.now() + 60_000 >= expiration) throw new Error("protected network session expired");
    try {
      return JSON.parse(
        execFileSync(
          "aws",
          [
            service,
            operation,
            "--region",
            region,
            "--output",
            "json",
            "--no-cli-pager",
            "--no-paginate",
            "--cli-connect-timeout",
            "5",
            "--cli-read-timeout",
            "15",
            "--cli-input-json",
            JSON.stringify(input),
          ],
          {
            encoding: "utf8",
            timeout: nativeCallTimeoutMs,
            maxBuffer: 1024 * 1024,
            env: {
              PATH: env.PATH,
              AWS_ACCESS_KEY_ID: env.AWS_ACCESS_KEY_ID,
              AWS_SECRET_ACCESS_KEY: env.AWS_SECRET_ACCESS_KEY,
              AWS_SESSION_TOKEN: env.AWS_SESSION_TOKEN,
              AWS_CONFIG_FILE: "/dev/null",
              AWS_SHARED_CREDENTIALS_FILE: "/dev/null",
              AWS_EC2_METADATA_DISABLED: "true",
              AWS_MAX_ATTEMPTS: "1",
              AWS_PAGER: "",
            },
            stdio: ["ignore", "pipe", "pipe"],
          },
        ),
      );
    } catch {
      throw new Error(`protected network ${operation} failed`);
    }
  };
}

/* oxlint-disable eslint/no-await-in-loop -- AWS cursors and bounded reconciliation observe the preceding response before proceeding. */
export async function qualificationNetwork(
  receipt,
  { remove = false, call, dispatch, sleep = wait, now = Date.now } = {},
) {
  validate(receipt);
  call ??= networkCLI(process.env, remove ? undefined : Date.parse(receipt.cleanupNotAfter));
  const invoke = (operation, input) => call("ec2", operation, input, receipt.region);
  const identity = await call("sts", "get-caller-identity", {}, receipt.region);
  if (identity.Account !== receipt.accountId) throw new Error("network owner account mismatch");
  if (!remove && now() >= Date.parse(receipt.expiresAt))
    throw new Error("network admission expired");
  const expectedTags = tags(receipt);
  const owned = (rule) => {
    const actual = Object.fromEntries((rule.Tags ?? []).map(({ Key, Value }) => [Key, Value]));
    return Object.entries(expectedTags).every(([key, value]) => actual[key] === value);
  };
  const exact = (rule) =>
    rule.GroupId === receipt.securityGroupId &&
    rule.IsEgress === false &&
    rule.IpProtocol === "tcp" &&
    rule.FromPort === 22 &&
    rule.ToPort === 22 &&
    rule.CidrIpv4 === `${receipt.ipv4}/32` &&
    !rule.CidrIpv6 &&
    !rule.PrefixListId &&
    !rule.ReferencedGroupInfo &&
    /^sgr-[0-9a-f]+$/.test(rule.SecurityGroupRuleId);
  const inventory = async () => {
    const rules = [];
    let NextToken;
    const seen = new Set();
    do {
      const page = await invoke("describe-security-group-rules", {
        Filters: [{ Name: "group-id", Values: [receipt.securityGroupId] }],
        MaxResults: 100,
        ...(NextToken ? { NextToken } : {}),
      });
      if (!Array.isArray(page.SecurityGroupRules)) throw new Error("invalid network inventory");
      rules.push(...page.SecurityGroupRules);
      NextToken = page.NextToken;
      if (NextToken && seen.has(NextToken)) throw new Error("network inventory cursor repeated");
      seen.add(NextToken);
      if (seen.size > 20) throw new Error("network inventory exceeds qualification capacity");
    } while (NextToken);
    const matches = rules.filter(owned);
    if (
      matches.some((rule) => !exact(rule)) ||
      matches.length > 1 ||
      (receipt.ruleId && matches.some((rule) => rule.SecurityGroupRuleId !== receipt.ruleId))
    ) {
      throw new Error("network rule ownership mismatch");
    }
    return { rules, matches };
  };
  if (remove) {
    const remaining = Date.parse(receipt.dispatchedUntil ?? "") - now();
    if (remaining > 60_000) throw new Error("invalid network dispatch window");
    if (remaining > 0) await sleep(remaining);
    // Reconcile lost responses and eventual visibility before confirming durable absence.
    for (const delay of [0, 1_000, 2_000, 4_000, 8_000, 15_000, 30_000]) {
      await sleep(delay);
      const { matches, rules } = await inventory();
      if (
        receipt.ruleId &&
        rules.some((rule) => rule.SecurityGroupRuleId === receipt.ruleId && !owned(rule))
      ) {
        throw new Error("recorded network rule is no longer owned");
      }
      for (const rule of matches) {
        await invoke("revoke-security-group-ingress", {
          GroupId: receipt.securityGroupId,
          SecurityGroupRuleIds: [rule.SecurityGroupRuleId],
        });
      }
    }
    if ((await inventory()).matches.length) throw new Error("network rule remains after cleanup");
    return { cleared: true };
  }
  const { rules, matches } = await inventory();
  if (rules.some((rule) => rule.IsEgress === false && !owned(rule))) {
    throw new Error("qualification group has foreign ingress");
  }
  if (matches.length) return { ruleId: matches[0].SecurityGroupRuleId };
  if (receipt.dispatchedUntil) throw new Error("ambiguous network admission requires cleanup");
  if (!dispatch) throw new Error("network dispatch fence is absent");
  const dispatched = await dispatch();
  const dispatchDeadline = Date.parse(dispatched?.dispatchedUntil ?? "");
  // A controller response can arrive after cleanup. Reserve the complete native
  // call budget inside the durable fence before starting any ingress write.
  if (!Number.isFinite(dispatchDeadline) || now() + nativeCallTimeoutMs >= dispatchDeadline) {
    throw new Error("network dispatch deadline expired");
  }
  if (now() + nativeCallTimeoutMs >= Date.parse(receipt.expiresAt)) {
    throw new Error("network admission expired before dispatch");
  }
  let created;
  try {
    created = await invoke("authorize-security-group-ingress", {
      GroupId: receipt.securityGroupId,
      IpPermissions: [
        {
          IpProtocol: "tcp",
          FromPort: 22,
          ToPort: 22,
          IpRanges: [{ CidrIp: `${receipt.ipv4}/32` }],
        },
      ],
      TagSpecifications: [
        {
          ResourceType: "security-group-rule",
          Tags: Object.entries(expectedTags).map(([Key, Value]) => ({ Key, Value })),
        },
      ],
    });
  } catch {
    // No blind retry: the persisted dispatch is recovered by run tags.
  }
  for (const delay of [0, 1_000, 2_000, 4_000, 8_000]) {
    await sleep(delay);
    const { matches: readback, rules: current } = await inventory();
    if (current.some((rule) => rule.IsEgress === false && !owned(rule)))
      throw new Error("qualification group ingress changed");
    if (readback.length) {
      const ruleId = readback[0].SecurityGroupRuleId;
      if (
        created &&
        (!Array.isArray(created.SecurityGroupRules) ||
          created.SecurityGroupRules.length !== 1 ||
          created.SecurityGroupRules[0].SecurityGroupRuleId !== ruleId)
      ) {
        throw new Error("network authorization readback mismatch");
      }
      return { ruleId };
    }
  }
  throw new Error("network authorization outcome is unresolved");
}
/* oxlint-enable eslint/no-await-in-loop */
