// Protected executor evidence for one immutable first-walk preparation intent.
// Response bodies, error text, claims, URLs and correlation IDs are never exported.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const maximumBodyBytes = 64 * 1024;
const reasons = new Map([
  [
    "first-walk instance account is not verified",
    ["instance-readback", "instance-account-unverified"],
  ],
  [
    "first-walk instance is not the exact running owned instance",
    ["instance-readback", "instance-identity-mismatch"],
  ],
  [
    "first-walk requires a stock instance with one root EBS volume",
    ["instance-readback", "stock-root-mismatch"],
  ],
  ["first-walk device is already occupied", ["instance-readback", "device-occupied"]],
  [
    "first-walk capture is not a single owned completed snapshot up to 400 GiB",
    ["snapshot-readback", "snapshot-not-admitted"],
  ],
  ["first-walk AZ is not verified", ["instance-readback", "zone-unverified"]],
  [
    "first-walk source or instance is outside the run ledger",
    ["authority-admission", "ledger-mismatch"],
  ],
  ["first-walk request is immutable", ["authority-admission", "intent-mismatch"]],
  ["first-walk request is malformed", ["authority-admission", "invalid-intent"]],
  ["first-walk binding is not enrolled", ["authority-admission", "binding-not-enrolled"]],
  ["first-walk is not enabled", ["authority-admission", "mode-not-enabled"]],
  [
    "first-walk create outcome unresolved; finalize instead of replacing",
    ["copy-readback", "create-unresolved"],
  ],
  ["first-walk volume identity or policy mismatch", ["copy-readback", "volume-identity-mismatch"]],
  ["first-walk attachment identity mismatch", ["copy-readback", "attachment-identity-mismatch"]],
  [
    "first-walk volume/attachment not ready; retry reads or finalize",
    ["copy-readback", "volume-not-ready"],
  ],
  [
    "first-walk attachment dispatched; retry for readback",
    ["copy-readback", "attachment-dispatched"],
  ],
  ["first-walk attachment not yet settled", ["copy-readback", "attachment-pending"]],
  ["first-walk instance mapping mismatch", ["copy-readback", "instance-mapping-mismatch"]],
  [
    "first-walk delete-on-termination requires readback",
    ["copy-readback", "mapping-readback-required"],
  ],
]);
const providerCodes = new Set([
  "UnauthorizedOperation",
  "AuthFailure",
  "RequestExpired",
  "RequestLimitExceeded",
  "Throttling",
  "InternalError",
  "ServiceUnavailable",
  "InvalidInstanceID.NotFound",
  "InvalidSnapshot.NotFound",
  "InvalidVolume.NotFound",
  "IncorrectState",
]);
export function preparationReason(data) {
  const message = typeof data?.error === "string" ? data.error : "";
  const known = reasons.get(message);
  if (known) return { stage: known[0], reason: known[1] };
  // Match only the existing owner's fixed error envelope and allowlisted codes.
  // Do not interpret unknown remote text as an established authorization cause.
  const provider = message.match(
    /^first-walk (DescribeInstances|DescribeSnapshots|DescribeVolumes|CreateVolume|AttachVolume|ModifyInstanceAttribute) http ([45]\d\d): ([\s\S]*)$/,
  );
  if (provider) {
    const code = provider[3].match(/<Code>([A-Za-z0-9.]+)<\/Code>/)?.[1];
    return {
      stage: "provider-response",
      reason: "provider-rejected",
      action: provider[1],
      providerStatus: Number(provider[2]),
      providerCode: providerCodes.has(code) ? code : "unclassified",
    };
  }
  return { stage: "unlocated", reason: "unclassified" };
}
function correlation(response) {
  const value = response.headers.get("cf-ray");
  return value && value.length <= 128
    ? { correlationSha256: crypto.createHash("sha256").update(value).digest("hex") }
    : {};
}
async function responseData(response) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of response.body ?? []) {
    bytes += chunk.length;
    if (bytes > maximumBodyBytes) return { bodyFormat: "oversized" };
    chunks.push(chunk);
  }
  try {
    return {
      bodyFormat: "json",
      bodyBytes: bytes,
      data: JSON.parse(Buffer.concat(chunks).toString("utf8")),
    };
  } catch {
    return { bodyFormat: "malformed", bodyBytes: bytes };
  }
}
export async function prepareFirstWalkWithEvidence({
  relay,
  token,
  intent,
  checkpointID,
  proof,
  remaining,
  wait = () => new Promise((resolve) => setTimeout(resolve, 1000)),
}) {
  const file = path.join(proof, "first-walk-preparation.json");
  const fd = fs.openSync(
    file,
    fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW,
    0o600,
  );
  const evidence = {
    version: 1,
    operation: "first-walk-prepare",
    attempts: 0,
    responses: 0,
    outcome: "not-started",
    errorCounts: {},
  };
  const persist = () => {
    const bytes = Buffer.from(JSON.stringify(evidence, null, 2) + "\n");
    fs.writeSync(fd, bytes, 0, bytes.length, 0);
    fs.ftruncateSync(fd, bytes.length);
    fs.fsyncSync(fd);
  };
  try {
    persist();
    // The existing 120 attempts, one-second cadence and immutable intent remain.
    // The authority owns mutation fencing and uncertain-response reconciliation.
    for (let i = 0; i < 120; i++) {
      const timeout = Math.min(30_000, remaining());
      evidence.attempts++;
      evidence.outcome = "request-started";
      persist(); // Survives process death before a response or deployment proof.
      let failure;
      let response;
      try {
        response = await fetch(`${relay}/v1/checkpoints/${checkpointID}/first-walk`, {
          method: "POST",
          headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
          body: JSON.stringify(intent),
          redirect: "manual",
          signal: AbortSignal.timeout(timeout),
        });
        evidence.responses++;
        const { data, ...metadata } = await responseData(response);
        if (response.ok && metadata.bodyFormat === "json") {
          evidence.outcome = "response-received";
          persist();
          return data;
        }
        failure = {
          httpStatus: response.status,
          ...correlation(response),
          ...metadata,
          ...(metadata.bodyFormat === "json"
            ? preparationReason(data)
            : { stage: "unlocated", reason: metadata.bodyFormat }),
        };
      } catch (error) {
        failure = {
          stage: "unlocated",
          reason: error?.name === "TimeoutError" ? "timeout" : "transport-uncertain",
          ...(response ? { httpStatus: response.status, ...correlation(response) } : {}),
        };
      }
      evidence.outcome = "error";
      evidence.firstError ??= failure;
      evidence.lastError = failure;
      evidence.errorCounts[failure.reason] = (evidence.errorCounts[failure.reason] ?? 0) + 1;
      persist();
      if (i === 119) throw new Error(`first-walk preparation failed: ${failure.reason}`);
      remaining(); // Expired forward work cannot be kept alive by this retry loop.
      await wait();
    }
  } finally {
    fs.closeSync(fd);
  }
}
