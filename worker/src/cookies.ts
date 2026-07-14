export const portalSessionCookieName = "__Host-crabbox_session";
export const legacyPortalSessionCookieName = "crabbox_session";
export const codeViewerSessionCookieName = "__Host-crabbox_code_session";
export const legacyCodeViewerSessionCookieName = "crabbox_code_session";

export function cookieValue(header: string, name: string): string {
  let result: string | undefined;
  for (const part of header.split(";")) {
    const [key, ...value] = part.trim().split("=");
    if (key === name) {
      if (result !== undefined) {
        return "";
      }
      try {
        result = decodeURIComponent(value.join("="));
      } catch {
        return "";
      }
    }
  }
  return result ?? "";
}
