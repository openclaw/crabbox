import {
  defaultOSImage,
  osImageAliases,
  osImageSpecs as specs,
  supportedOSImages,
  type OSImageSpec,
} from "./os-image.generated";
export { defaultOSImage, type OSImageSpec } from "./os-image.generated";

export function normalizeOSImage(value: string | undefined): string {
  let normalized = (value ?? "").trim().toLowerCase();
  if (!normalized) {
    normalized = defaultOSImage;
  }
  normalized = normalized.replaceAll("_", ".").replaceAll("-", ":");
  normalized = Object.hasOwn(osImageAliases, normalized) ? osImageAliases[normalized]! : normalized;
  if (!Object.hasOwn(specs, normalized)) {
    throw new Error(`unsupported os ${JSON.stringify(value)}; supported: ${supportedOSImages}`);
  }
  return normalized;
}

export function osImageSpec(value: string | undefined): OSImageSpec {
  const spec = specs[normalizeOSImage(value)];
  if (!spec) {
    throw new Error(`unsupported os ${JSON.stringify(value)}; supported: ${supportedOSImages}`);
  }
  return spec;
}
