import type { RunRecord, TerminalRunReceipt } from "./types";

const terminalReceiptMaxBytes = 16 * 1024;
const terminalReceiptFieldMaxBytes = 4 * 1024;
const terminalReceiptIdentityMaxBytes = 256;
const terminalReceiptClockSkewMs = 30_000;
const terminalReceiptFields = [
  "schema_version",
  "receipt_type",
  "started_at",
  "ended_at",
  "provider",
  "lease_id",
  "slug",
  "run_id",
  "command",
  "command_sha256",
  "exit_code",
  "sync_ms",
  "command_ms",
  "duration_ms",
  "log_sha256",
  "retained_log_sha256",
  "log_truncated",
  "public_key",
  "signer",
  "signature",
] as const;
const terminalReceiptSigningFields = terminalReceiptFields.filter((field) => field !== "signature");
const encoder = new TextEncoder();

export async function verifyTerminalReceipt(
  value: unknown,
  binding: {
    run: RunRecord;
    exitCode: number;
    syncMs: number;
    commandMs: number;
    log: string;
    logTruncated: boolean;
    observedAt: Date;
  },
): Promise<TerminalRunReceipt> {
  const receipt = parseTerminalReceipt(value);
  const startedAt = Date.parse(receipt.started_at);
  const endedAt = Date.parse(receipt.ended_at);
  if (
    !Number.isFinite(startedAt) ||
    !Number.isFinite(endedAt) ||
    endedAt < startedAt ||
    endedAt > binding.observedAt.getTime() + terminalReceiptClockSkewMs ||
    receipt.duration_ms !== endedAt - startedAt ||
    receipt.duration_ms < binding.syncMs + binding.commandMs
  ) {
    throw new Error("invalid terminal receipt timestamps");
  }
  if (
    startedAt !== Date.parse(binding.run.startedAt) ||
    receipt.provider !== binding.run.provider ||
    receipt.lease_id !== (binding.run.leaseID || undefined) ||
    receipt.slug !== (binding.run.slug || undefined) ||
    receipt.run_id !== binding.run.id ||
    receipt.exit_code !== binding.exitCode ||
    receipt.sync_ms !== binding.syncMs ||
    receipt.command_ms !== binding.commandMs ||
    receipt.log_truncated !== binding.logTruncated
  ) {
    throw new Error("terminal receipt run binding mismatch");
  }
  if (receipt.command_sha256 !== (await commandSHA256(binding.run.command))) {
    throw new Error("terminal receipt command binding mismatch");
  }
  const retainedDigest = await sha256Digest(encoder.encode(binding.log));
  if (
    receipt.retained_log_sha256 !== retainedDigest ||
    (!binding.logTruncated && receipt.log_sha256 !== retainedDigest)
  ) {
    throw new Error("terminal receipt log binding mismatch");
  }
  const publicKey = decodeBase64(receipt.public_key, 32, "public_key");
  if (receipt.signer !== (await sha256Digest(publicKey))) {
    throw new Error("terminal receipt signer mismatch");
  }
  const signature = decodeBase64(receipt.signature, 64, "signature");
  const key = await crypto.subtle.importKey("raw", publicKey, "Ed25519", false, ["verify"]);
  if (
    !(await crypto.subtle.verify("Ed25519", key, signature, terminalReceiptSigningBytes(receipt)))
  ) {
    throw new Error("terminal receipt signature mismatch");
  }
  return receipt;
}

export async function terminalFinishSHA256(input: {
  exitCode: number;
  syncMs: number;
  commandMs: number;
  log: string;
  logTruncated: boolean;
  blockedStage: string | undefined;
  retryLikely: string | undefined;
  results: unknown;
  telemetry: unknown;
  receipt: TerminalRunReceipt | undefined;
}): Promise<string> {
  return sha256Digest(
    encoder.encode(
      JSON.stringify([
        input.exitCode,
        input.syncMs,
        input.commandMs,
        input.log,
        input.logTruncated,
        input.blockedStage ?? "",
        input.retryLikely ?? "",
        stableJSONValue(input.results),
        stableJSONValue(input.telemetry),
        stableJSONValue(input.receipt),
      ]),
    ),
  );
}

export function sameTerminalRunBinding(left: RunRecord, right: RunRecord): boolean {
  return (
    left.provider === right.provider &&
    left.leaseID === right.leaseID &&
    left.slug === right.slug &&
    left.startedAt === right.startedAt &&
    left.command.length === right.command.length &&
    left.command.every((argument, index) => argument === right.command[index])
  );
}

function stableJSONValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stableJSONValue);
  if (!value || typeof value !== "object") return value ?? null;
  return Object.fromEntries(
    Object.entries(value)
      .toSorted(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => [key, stableJSONValue(entry)]),
  );
}

function parseTerminalReceipt(value: unknown): TerminalRunReceipt {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("terminal receipt must be an object");
  }
  if (encoder.encode(JSON.stringify(value)).byteLength > terminalReceiptMaxBytes) {
    throw new Error("terminal receipt is too large");
  }
  const record = value as Record<string, unknown>;
  const allowed = new Set<string>(terminalReceiptFields);
  if (Object.keys(record).some((field) => !allowed.has(field))) {
    throw new Error("terminal receipt has unknown fields");
  }
  for (const field of terminalReceiptFields) {
    if ((field === "lease_id" || field === "slug") && record[field] === undefined) continue;
    if (record[field] === undefined) throw new Error(`terminal receipt is missing ${field}`);
  }
  const receipt = record as unknown as TerminalRunReceipt;
  if (receipt.schema_version !== 2 || receipt.receipt_type !== "terminal") {
    throw new Error("unsupported terminal receipt");
  }
  for (const field of [
    "started_at",
    "ended_at",
    "provider",
    "run_id",
    "command",
    "command_sha256",
    "log_sha256",
    "retained_log_sha256",
    "public_key",
    "signer",
    "signature",
  ] as const) {
    if (
      typeof receipt[field] !== "string" ||
      !receipt[field] ||
      encoder.encode(receipt[field]).byteLength > terminalReceiptFieldMaxBytes
    ) {
      throw new Error(`invalid terminal receipt ${field}`);
    }
  }
  for (const field of ["lease_id", "slug"] as const) {
    if (
      receipt[field] !== undefined &&
      (typeof receipt[field] !== "string" ||
        !receipt[field] ||
        encoder.encode(receipt[field]).byteLength > terminalReceiptIdentityMaxBytes)
    ) {
      throw new Error(`invalid terminal receipt ${field}`);
    }
  }
  for (const field of ["exit_code", "sync_ms", "command_ms", "duration_ms"] as const) {
    if (!Number.isSafeInteger(receipt[field]) || receipt[field] < 0) {
      throw new Error(`invalid terminal receipt ${field}`);
    }
  }
  if (typeof receipt.log_truncated !== "boolean") {
    throw new Error("invalid terminal receipt log_truncated");
  }
  for (const field of ["command_sha256", "log_sha256", "retained_log_sha256", "signer"] as const) {
    if (!/^sha256:[0-9a-f]{64}$/u.test(receipt[field])) {
      throw new Error(`invalid terminal receipt ${field}`);
    }
  }
  return receipt;
}

function terminalReceiptSigningBytes(receipt: TerminalRunReceipt): Uint8Array {
  return lengthPrefixedPayload(
    "crabbox-terminal-receipt-v2\0",
    terminalReceiptSigningFields.map((field) => String(receipt[field] ?? "")),
  );
}

async function commandSHA256(command: string[]): Promise<string> {
  return sha256Digest(lengthPrefixedPayload("crabbox-command-v1\0", command));
}

function lengthPrefixedPayload(prefix: string, values: string[]): Uint8Array {
  const parts = [encoder.encode(prefix)];
  for (const entry of values) {
    const value = encoder.encode(entry);
    const length = new Uint8Array(4);
    new DataView(length.buffer).setUint32(0, value.byteLength);
    parts.push(length, value);
  }
  const payload = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0));
  let offset = 0;
  for (const part of parts) {
    payload.set(part, offset);
    offset += part.byteLength;
  }
  return payload;
}

async function sha256Digest(value: BufferSource): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", value));
  return `sha256:${[...digest].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function decodeBase64(value: string, length: number, field: string): Uint8Array {
  try {
    const decoded = Uint8Array.from(atob(value), (character) => character.charCodeAt(0));
    if (decoded.byteLength === length) return decoded;
  } catch {
    // Report one stable validation error below.
  }
  throw new Error(`invalid terminal receipt ${field}`);
}
