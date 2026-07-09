import type { ImageCapabilities, ImageRequirements } from "./types";

const maxVersionEntries = 32;
const maxVersionLength = 128;
const namePattern = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const versionPattern = /^\d+(?:\.\d+)*$/;

export class InvalidImageCapabilitiesError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "InvalidImageCapabilitiesError";
  }
}

function normalizedVersion(value: unknown, label: string): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  if (
    typeof value !== "string" ||
    value.trim().length > maxVersionLength ||
    !versionPattern.test(value.trim())
  ) {
    throw new InvalidImageCapabilitiesError(`${label} must be a dot-separated numeric version`);
  }
  return value.trim();
}

function normalizedVersions(value: unknown, label: string): Record<string, string> | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new InvalidImageCapabilitiesError(
      `${label} must be an object of name-to-version entries`,
    );
  }
  const entries = Object.entries(value as Record<string, unknown>);
  if (entries.length > maxVersionEntries) {
    throw new InvalidImageCapabilitiesError(
      `${label} supports at most ${maxVersionEntries} entries`,
    );
  }
  const normalized: Record<string, string> = {};
  for (const [rawName, rawVersion] of entries) {
    const name = rawName.trim().toLowerCase();
    if (!namePattern.test(name)) {
      throw new InvalidImageCapabilitiesError(
        `${label} name ${JSON.stringify(rawName)} is invalid`,
      );
    }
    const version = normalizedVersion(rawVersion, `${label}.${name}`);
    if (!version) {
      throw new InvalidImageCapabilitiesError(`${label}.${name} requires a version`);
    }
    normalized[name] = version;
  }
  return entries.length > 0 ? normalized : undefined;
}

function normalizedBoolean(value: unknown, label: string): boolean | undefined {
  if (value === undefined || value === null || value === false) return undefined;
  if (value !== true) throw new InvalidImageCapabilitiesError(`${label} must be a boolean`);
  return true;
}

export function normalizeImageCapabilities(value: unknown): ImageCapabilities | undefined {
  if (value === undefined || value === null) return undefined;
  const input = imageCapabilityObject(value, "capabilities", [
    "osVersion",
    "sdks",
    "runtimes",
    "browser",
    "webview2",
    "desktop",
  ]);
  const osVersion = normalizedVersion(input["osVersion"], "capabilities.osVersion");
  const sdks = normalizedVersions(input["sdks"], "capabilities.sdks");
  const runtimes = normalizedVersions(input["runtimes"], "capabilities.runtimes");
  const browser = normalizedBoolean(input["browser"], "capabilities.browser");
  const webview2 = normalizedBoolean(input["webview2"], "capabilities.webview2");
  const desktop = normalizedBoolean(input["desktop"], "capabilities.desktop");
  const normalized: ImageCapabilities = {
    ...(osVersion ? { osVersion } : {}),
    ...(sdks ? { sdks } : {}),
    ...(runtimes ? { runtimes } : {}),
    ...(browser ? { browser } : {}),
    ...(webview2 ? { webview2 } : {}),
    ...(desktop ? { desktop } : {}),
  };
  return hasValues(normalized) ? normalized : undefined;
}

export function normalizeImageRequirements(value: unknown): ImageRequirements {
  if (value === undefined || value === null) return {};
  const input = imageCapabilityObject(value, "imageRequirements", [
    "minOS",
    "sdks",
    "runtimes",
    "browser",
    "webview2",
    "desktop",
  ]);
  const minOS = normalizedVersion(input["minOS"], "imageRequirements.minOS");
  const sdks = normalizedVersions(input["sdks"], "imageRequirements.sdks");
  const runtimes = normalizedVersions(input["runtimes"], "imageRequirements.runtimes");
  const browser = normalizedBoolean(input["browser"], "imageRequirements.browser");
  const webview2 = normalizedBoolean(input["webview2"], "imageRequirements.webview2");
  const desktop = normalizedBoolean(input["desktop"], "imageRequirements.desktop");
  return {
    ...(minOS ? { minOS } : {}),
    ...(sdks ? { sdks } : {}),
    ...(runtimes ? { runtimes } : {}),
    ...(browser ? { browser } : {}),
    ...(webview2 ? { webview2 } : {}),
    ...(desktop ? { desktop } : {}),
  };
}

function imageCapabilityObject(
  value: unknown,
  label: string,
  allowedKeys: string[],
): Record<string, unknown> {
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new InvalidImageCapabilitiesError(`${label} must be an object`);
  }
  const input = value as Record<string, unknown>;
  const unknownKey = Object.keys(input).find((key) => !allowedKeys.includes(key));
  if (unknownKey) {
    throw new InvalidImageCapabilitiesError(`${label}.${unknownKey} is not supported`);
  }
  return input;
}

export function hasImageRequirements(value: ImageRequirements): boolean {
  return hasValues(value);
}

export function missingImageCapabilities(
  capabilities: ImageCapabilities | undefined,
  requirements: ImageRequirements,
): string[] {
  const missing: string[] = [];
  if (requirements.minOS && !versionAtLeast(capabilities?.osVersion, requirements.minOS)) {
    missing.push(`OS >= ${requirements.minOS}`);
  }
  for (const [name, version] of Object.entries(requirements.sdks ?? {})) {
    if (!versionAtLeast(capabilities?.sdks?.[name], version))
      missing.push(`SDK ${name} >= ${version}`);
  }
  for (const [name, version] of Object.entries(requirements.runtimes ?? {})) {
    if (!versionAtLeast(capabilities?.runtimes?.[name], version)) {
      missing.push(`runtime ${name} >= ${version}`);
    }
  }
  if (requirements.browser && !capabilities?.browser) missing.push("browser");
  if (requirements.webview2 && !capabilities?.webview2) missing.push("WebView2");
  if (requirements.desktop && !capabilities?.desktop) missing.push("desktop");
  return missing;
}

export function imageSatisfiesRequirements(
  capabilities: ImageCapabilities | undefined,
  requirements: ImageRequirements,
): boolean {
  return missingImageCapabilities(capabilities, requirements).length === 0;
}

function versionAtLeast(actual: string | undefined, required: string): boolean {
  if (!actual || !versionPattern.test(actual)) return false;
  const actualParts = actual.split(".");
  const requiredParts = required.split(".");
  for (let index = 0; index < Math.max(actualParts.length, requiredParts.length); index += 1) {
    const actualPart = normalizedNumericPart(actualParts[index] ?? "0");
    const requiredPart = normalizedNumericPart(requiredParts[index] ?? "0");
    if (actualPart.length !== requiredPart.length) return actualPart.length > requiredPart.length;
    if (actualPart !== requiredPart) return actualPart > requiredPart;
  }
  return true;
}

function normalizedNumericPart(value: string): string {
  return value.replace(/^0+(?=\d)/, "");
}

function hasValues(value: ImageCapabilities | ImageRequirements): boolean {
  return Object.values(value).some((entry) =>
    typeof entry === "object" ? Object.keys(entry ?? {}).length > 0 : entry !== undefined,
  );
}
