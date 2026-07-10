import type { Env } from "./types";

const currentOrgKeyPrefix = "~1.";
const legacyOrgKeyPattern = /^[a-zA-Z0-9_.@-]{1,63}$/;

export const MISSING_ORG_LABEL = "unknown";
export const MISSING_ORG_KEY = "~0";

export class InvalidOrgLabelError extends Error {
  constructor(source = "organization") {
    super(`${source} must be 1-63 printable ASCII characters without leading or trailing spaces`);
    this.name = "InvalidOrgLabelError";
  }
}

export function requestOrgLabel(request: Request, env: Pick<Env, "CRABBOX_DEFAULT_ORG">): string {
  const source = requestOrgSource(request, env);
  return source?.value ? validatedOrgLabel(source.value, source.name) : MISSING_ORG_LABEL;
}

export function requestOrgKey(request: Request, env: Pick<Env, "CRABBOX_DEFAULT_ORG">): string {
  const source = requestOrgSource(request, env);
  return source?.value
    ? orgKeyForLabel(validatedOrgLabel(source.value, source.name))
    : MISSING_ORG_KEY;
}

/** Stored and compared authorization identity. Use requestOrgLabel only at public boundaries. */
export function requestOrg(request: Request, env: Pick<Env, "CRABBOX_DEFAULT_ORG">): string {
  return requestOrgKey(request, env);
}

export function orgKeyForLabel(label: string): string {
  const validLabel = validatedOrgLabel(label);
  return `${currentOrgKeyPrefix}${encodeBase64URL(validLabel)}`;
}

/** Decode only canonical current keys. Legacy and malformed values return undefined. */
export function orgLabelFromKey(key: unknown): string | undefined {
  if (typeof key !== "string" || !key.startsWith(currentOrgKeyPrefix)) {
    return undefined;
  }
  const encoded = key.slice(currentOrgKeyPrefix.length);
  if (!encoded || !/^[a-zA-Z0-9_-]+$/.test(encoded) || encoded.length % 4 === 1) {
    return undefined;
  }
  try {
    const label = decodeBase64URL(encoded);
    return orgKeyForLabel(label) === key ? label : undefined;
  } catch {
    return undefined;
  }
}

/** Exact label for a trusted request header; missing identity is represented by an empty value. */
export function orgAuthLabelFromKey(key: unknown): string | undefined {
  return key === MISSING_ORG_KEY ? "" : orgLabelFromKey(key);
}

export function orgLabelForDisplay(key: unknown): string {
  if (key === MISSING_ORG_KEY) {
    return MISSING_ORG_LABEL;
  }
  return (
    orgLabelFromKey(key) ??
    (typeof key === "string" && isLegacyOrgKey(key) ? key : MISSING_ORG_LABEL)
  );
}

export function isCurrentOrgKey(key: unknown): key is string {
  return key === MISSING_ORG_KEY || orgLabelFromKey(key) !== undefined;
}

/** Legacy identities never authorize, including against an identical legacy value. */
export function sameOrgIdentityKey(left: string, right: string): boolean {
  return left === right && isCurrentOrgKey(left);
}

/** Query/report filters may select an exact legacy bucket; never use this for authorization. */
export function orgMatchesForFilter(left: string, right: string): boolean {
  return (
    sameOrgIdentityKey(left, right) ||
    (isLegacyOrgKey(left) && isLegacyOrgKey(right) && left === right)
  );
}

/**
 * Cost limits include a legacy bucket in every identity it could have represented.
 * This can over-count after an upgrade, but cannot let an ambiguous legacy record bypass a cap.
 */
export function orgMatchesForAccounting(left: string, right: string): boolean {
  if (sameOrgIdentityKey(left, right)) {
    return true;
  }
  const leftLegacy = isLegacyOrgKey(left);
  const rightLegacy = isLegacyOrgKey(right);
  if (leftLegacy && rightLegacy) {
    return left === right;
  }
  if (!leftLegacy && !rightLegacy) {
    return false;
  }
  const legacy = leftLegacy ? left : right;
  const current = leftLegacy ? right : left;
  const label = current === MISSING_ORG_KEY ? MISSING_ORG_LABEL : orgLabelFromKey(current);
  return label !== undefined && legacyOrgKey(label) === legacy;
}

function requestOrgSource(
  request: Request,
  env: Pick<Env, "CRABBOX_DEFAULT_ORG">,
): { value: string; name: string } | undefined {
  const header = request.headers.get("x-crabbox-org");
  if (header !== null) {
    return { value: header, name: "x-crabbox-org" };
  }
  if (env.CRABBOX_DEFAULT_ORG) {
    return { value: env.CRABBOX_DEFAULT_ORG, name: "CRABBOX_DEFAULT_ORG" };
  }
  return undefined;
}

function validatedOrgLabel(value: string, source?: string): string {
  const printableASCII = [...value].every((character) => {
    const code = character.charCodeAt(0);
    return code >= 0x20 && code <= 0x7e;
  });
  if (
    value.length < 1 ||
    value.length > 63 ||
    value.startsWith(" ") ||
    value.endsWith(" ") ||
    !printableASCII
  ) {
    throw new InvalidOrgLabelError(source);
  }
  return value;
}

export function isLegacyOrgKey(value: string): boolean {
  return legacyOrgKeyPattern.test(value);
}

function legacyOrgKey(value: string): string {
  return value.replaceAll(/[^a-zA-Z0-9_.@-]/g, "_").slice(0, 63) || MISSING_ORG_LABEL;
}

function encodeBase64URL(value: string): string {
  return btoa(value).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function decodeBase64URL(value: string): string {
  const base64 = value.replaceAll("-", "+").replaceAll("_", "/");
  return atob(base64.padEnd(Math.ceil(base64.length / 4) * 4, "="));
}
