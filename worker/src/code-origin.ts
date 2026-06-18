import { sha256Hex } from "./auth";
import type { Env } from "./types";

const placeholder = "{lease}";
export const codeProxyRequestBodyBytes = 10 * 1024 * 1024;

export async function codeOriginForLease(
  env: Pick<Env, "CRABBOX_CODE_ORIGIN_TEMPLATE">,
  leaseID: string,
): Promise<string | undefined> {
  const template = env.CRABBOX_CODE_ORIGIN_TEMPLATE?.trim();
  if (!template || template.split(placeholder).length !== 2) {
    return undefined;
  }
  const marker = "crabbox-lease-marker";
  let parsed: URL;
  try {
    parsed = new URL(template.replace(placeholder, marker));
  } catch {
    return undefined;
  }
  if (
    parsed.protocol !== "https:" ||
    parsed.username ||
    parsed.password ||
    parsed.pathname !== "/" ||
    parsed.search ||
    parsed.hash ||
    !parsed.hostname.includes(marker)
  ) {
    return undefined;
  }
  const leaseLabel = `cbx-${(await sha256Hex(leaseID)).slice(0, 32)}`;
  return new URL(template.replace(placeholder, leaseLabel)).origin;
}

export async function isIsolatedCodeRequest(
  request: Request,
  env: Pick<Env, "CRABBOX_CODE_ORIGIN_TEMPLATE">,
): Promise<boolean> {
  const url = new URL(request.url);
  const match = /^\/portal\/leases\/([^/]+)\/code(?:\/|$)/.exec(url.pathname);
  if (!match?.[1]) {
    return false;
  }
  let leaseID: string;
  try {
    leaseID = decodeURIComponent(match[1]);
  } catch {
    return false;
  }
  return url.origin === (await codeOriginForLease(env, leaseID));
}
